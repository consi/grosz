package tariff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/consi/grosz/internal/store"
)

const (
	pstrykBaseURL = "https://api.pstryk.pl"
	pstrykName    = "pstryk"
)

// pstrykResponse is the unified-metrics API response.
type pstrykResponse struct {
	Frames []pstrykFrame `json:"frames"`
}

type pstrykFrame struct {
	Start   string        `json:"start"`
	End     string        `json:"end"`
	IsLive  bool          `json:"is_live"`
	Metrics pstrykMetrics `json:"metrics"`
}

type pstrykMetrics struct {
	Pricing *pstrykPricing `json:"pricing"`
}

type pstrykPricing struct {
	PriceGross float64 `json:"price_gross"`
}

// Pstryk implements Provider for the Pstryk.pl API.
type Pstryk struct {
	store   *store.Store
	log     *slog.Logger
	client  *http.Client
	baseURL string

	mu    sync.RWMutex
	rates []Rate

	cancel context.CancelFunc
}

// NewPstryk creates a Pstryk provider that starts refreshing rates in the background.
func NewPstryk(st *store.Store, log *slog.Logger) *Pstryk {
	return NewPstrykWithURL(st, log, pstrykBaseURL)
}

// NewPstrykWithURL creates a Pstryk provider with a custom base URL (for testing).
func NewPstrykWithURL(st *store.Store, log *slog.Logger, baseURL string) *Pstryk {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pstryk{
		store:   st,
		log:     log.With("component", "tariff", "provider", pstrykName),
		client:  &http.Client{Timeout: 30 * time.Second},
		cancel:  cancel,
		baseURL: baseURL,
	}

	// Load cached rates from store
	if cached, err := st.LoadRates(pstrykName); err == nil && len(cached) > 0 {
		p.mu.Lock()
		p.rates = toTariffRates(cached)
		p.mu.Unlock()
		p.log.Info("loaded cached rates", "count", len(cached))
	}

	go p.refreshLoop(ctx)
	return p
}

func (p *Pstryk) Name() string { return pstrykName }

func (p *Pstryk) Rates() ([]Rate, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.rates) == 0 {
		return nil, fmt.Errorf("no rates available")
	}
	out := make([]Rate, len(p.rates))
	copy(out, p.rates)
	return out, nil
}

func (p *Pstryk) Stop() {
	p.cancel()
}

func (p *Pstryk) refreshLoop(ctx context.Context) {
	// Fetch immediately
	p.fetchAndStore()

	// Use a short interval initially — once rates are loaded, switch to hourly
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	settled := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.fetchAndStore()

			p.mu.RLock()
			hasRates := len(p.rates) > 0
			p.mu.RUnlock()

			if hasRates && !settled {
				// Rates loaded — slow down to hourly
				ticker.Reset(1 * time.Hour)
				settled = true
			}

			// Afternoon check for tomorrow's prices (14:00-16:00 Warsaw)
			if settled {
				loc, _ := time.LoadLocation("Europe/Warsaw")
				if loc == nil {
					loc = time.FixedZone("CET", 3600)
				}
				now := time.Now().In(loc)
				if now.Hour() >= 14 && now.Hour() < 16 {
					ticker.Reset(15 * time.Minute)
					settled = false // will re-settle after afternoon window
				}
			}
		}
	}
}

func (p *Pstryk) fetchAndStore() {
	token := p.store.GetDefault("tariff.pstryk_token", "")
	if token == "" {
		p.log.Warn("pstryk token not configured, skipping fetch")
		p.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "tariff", Action: "fetchRates", Level: "warn",
			Result: map[string]any{"skipped": true, "reason": "token not configured"},
		})
		return
	}

	rates, err := p.fetch(token)
	if err != nil {
		p.log.Warn("failed to fetch rates", "err", err)
		p.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "tariff", Action: "fetchRates", Level: "warn",
			Result: map[string]any{"error": err.Error()},
		})
		return
	}

	// Filter out placeholder/forecast data for tomorrow
	beforeCount := len(rates)
	rates = filterPlaceholders(rates)
	if len(rates) < beforeCount {
		p.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "tariff", Action: "filterPlaceholders",
			Input:  map[string]any{"beforeCount": beforeCount},
			Result: map[string]any{"afterCount": len(rates), "removed": beforeCount - len(rates)},
		})
	}

	if len(rates) == 0 {
		p.log.Warn("no valid rates returned")
		p.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "tariff", Action: "fetchRates", Level: "warn",
			Result: map[string]any{"error": "no valid rates returned"},
		})
		return
	}

	// Save to store
	storeRates := toStoreRates(rates)
	if err := p.store.SaveRates(pstrykName, storeRates); err != nil {
		p.log.Warn("failed to cache rates", "err", err)
	}

	p.mu.Lock()
	p.rates = rates
	p.mu.Unlock()

	p.log.Info("rates updated", "count", len(rates),
		"from", rates[0].Start.Format(time.RFC3339),
		"to", rates[len(rates)-1].End.Format(time.RFC3339),
	)
	p.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "tariff", Action: "fetchRates",
		Result: map[string]any{
			"count": len(rates),
			"from":  rates[0].Start.Format(time.RFC3339),
			"to":    rates[len(rates)-1].End.Format(time.RFC3339),
		},
	})
}

func (p *Pstryk) fetch(token string) ([]Rate, error) {
	now := time.Now().UTC()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	windowEnd := todayMidnight.Add(48 * time.Hour)

	params := url.Values{}
	params.Set("metrics", "pricing")
	params.Set("resolution", "hour")
	params.Set("window_start", todayMidnight.Format(time.RFC3339))
	params.Set("window_end", windowEnd.Format(time.RFC3339))

	reqURL := fmt.Sprintf("%s/integrations/meter-data/unified-metrics/?%s", p.baseURL, params.Encode())

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("rate limited (429)")
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var data pstrykResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	var rates []Rate
	for _, f := range data.Frames {
		if f.Metrics.Pricing == nil {
			continue
		}
		start, err := time.Parse(time.RFC3339, f.Start)
		if err != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339, f.End)
		if err != nil {
			continue
		}
		rates = append(rates, Rate{
			Start: start,
			End:   end,
			Price: f.Metrics.Pricing.PriceGross,
		})
	}

	return rates, nil
}

// filterPlaceholders detects and removes placeholder data for tomorrow.
// If >90% of tomorrow's values are identical, they're forecasts.
func filterPlaceholders(rates []Rate) []Rate {
	now := time.Now().UTC()
	todayEnd := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

	var todayRates, tomorrowRates []Rate
	for _, r := range rates {
		if r.Start.Before(todayEnd) {
			todayRates = append(todayRates, r)
		} else {
			tomorrowRates = append(tomorrowRates, r)
		}
	}

	if len(tomorrowRates) < 2 {
		return rates
	}

	// Check if >90% of tomorrow's prices are identical
	priceCounts := make(map[float64]int)
	for _, r := range tomorrowRates {
		rounded := math.Round(r.Price*10000) / 10000
		priceCounts[rounded]++
	}
	maxCount := 0
	for _, c := range priceCounts {
		if c > maxCount {
			maxCount = c
		}
	}
	if float64(maxCount)/float64(len(tomorrowRates)) > 0.9 {
		// Placeholder data — discard tomorrow
		return todayRates
	}

	return rates
}

func toStoreRates(rates []Rate) []store.Rate {
	out := make([]store.Rate, len(rates))
	for i, r := range rates {
		out[i] = store.Rate{Start: r.Start, End: r.End, Price: r.Price}
	}
	return out
}

func toTariffRates(rates []store.Rate) []Rate {
	out := make([]Rate, len(rates))
	for i, r := range rates {
		out[i] = Rate{Start: r.Start, End: r.End, Price: r.Price}
	}
	return out
}
