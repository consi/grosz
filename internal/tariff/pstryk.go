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

	"github.com/consi/grosz/internal/events"
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
	Start    string        `json:"start"`
	End      string        `json:"end"`
	IsLive   bool          `json:"is_live"`
	Metrics  pstrykMetrics `json:"metrics"`
	FaeUsage *float64      `json:"fae_usage"` // top-level kWh imported, present on meter_values responses
}

type pstrykMetrics struct {
	Pricing     *pstrykPricing     `json:"pricing"`
	MeterValues *pstrykMeterValues `json:"meter_values"`
}

type pstrykPricing struct {
	PriceGross float64 `json:"price_gross"`
}

type pstrykMeterValues struct {
	// EnergyActiveImportRegister is kWh imported during this frame.
	EnergyActiveImportRegister *float64 `json:"energy_active_import_register"`
}

// Pstryk implements Provider for the Pstryk.pl API.
type Pstryk struct {
	store        *store.Store
	tariffEvents *events.Bound // SourceTariff — pricing events
	pstrykEvents *events.Bound // SourcePstryk — consumption / backfill events
	log          *slog.Logger
	client       *http.Client
	baseURL      string

	mu    sync.RWMutex
	rates []Rate

	// noTokenWarnAt debounces the "token not configured" warn so an unconfigured
	// instance doesn't spam the system log every tick. Reset to zero whenever a
	// token IS present so a later misconfiguration warns once again.
	noTokenWarnAt time.Time

	// liveEventHour is the last hour for which the live loop logged its
	// once-per-hour success event. liveWarnAt debounces live-fetch error
	// events the same way noTokenWarnAt debounces the token warn.
	liveEventHour time.Time
	liveWarnAt    time.Time

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
		store:        st,
		tariffEvents: events.For(events.SourceTariff, st),
		pstrykEvents: events.For(events.SourcePstryk, st),
		log:          log.With("component", "tariff", "provider", pstrykName),
		client:       &http.Client{Timeout: 30 * time.Second},
		cancel:       cancel,
		baseURL:      baseURL,
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
	// Pricing is synchronous: the scheduler waits on `hasRates` below.
	// Consumption backfill can be slow on cold-start (up to 48h of frames +
	// idle rebuilds), so run it off the goroutine that owns the ticker.
	p.fetchAndStore(ctx)
	go p.fetchConsumptionAndStore(ctx)
	go p.liveLoop(ctx)

	// Use a short interval initially — once rates are loaded, switch to hourly
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	settled := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.fetchAndStore(ctx)
			p.fetchConsumptionAndStore(ctx)

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

func (p *Pstryk) fetchAndStore(ctx context.Context) {
	token := p.store.GetDefault("tariff.pstryk_token", "")
	if token == "" {
		p.log.Warn("pstryk token not configured, skipping fetch")
		p.tariffEvents.Warn(events.ActionFetchRates, nil,
			map[string]any{"skipped": true, "reason": "token not configured"},
		)
		return
	}

	rates, err := p.fetch(ctx, token)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; not an error
		}
		p.log.Warn("failed to fetch rates", "err", err)
		p.tariffEvents.Error(events.ActionFetchRates, nil, err)
		return
	}

	// Filter out placeholder/forecast data for tomorrow
	beforeCount := len(rates)
	rates = filterPlaceholders(rates)
	if len(rates) < beforeCount {
		p.tariffEvents.Info(events.ActionFilterPlaceholders,
			map[string]any{"beforeCount": beforeCount},
			map[string]any{"afterCount": len(rates), "removed": beforeCount - len(rates)},
		)
	}

	if len(rates) == 0 {
		p.log.Warn("no valid rates returned")
		p.tariffEvents.Warn(events.ActionFetchRates, nil,
			map[string]any{"error": "no valid rates returned"},
		)
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
	p.tariffEvents.Info(events.ActionFetchRates, nil,
		map[string]any{
			"count": len(rates),
			"from":  rates[0].Start.Format(time.RFC3339),
			"to":    rates[len(rates)-1].End.Format(time.RFC3339),
		},
	)
}

// unifiedMetrics issues a GET to the shared unified-metrics endpoint and
// decodes the response. All Pstryk integration endpoints we use share auth,
// rate-limit shape, and JSON envelope, so this centralizes the boilerplate.
func (p *Pstryk) unifiedMetrics(ctx context.Context, token string, params url.Values) (*pstrykResponse, error) {
	reqURL := fmt.Sprintf("%s/integrations/meter-data/unified-metrics/?%s", p.baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
	return &data, nil
}

func (p *Pstryk) fetch(ctx context.Context, token string) ([]Rate, error) {
	now := time.Now().UTC()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	windowEnd := todayMidnight.Add(48 * time.Hour)

	params := url.Values{}
	params.Set("metrics", "pricing")
	params.Set("resolution", "hour")
	params.Set("window_start", todayMidnight.Format(time.RFC3339))
	params.Set("window_end", windowEnd.Format(time.RFC3339))

	data, err := p.unifiedMetrics(ctx, token, params)
	if err != nil {
		return nil, err
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

// FetchConsumption returns hourly consumption frames for [start, end). Uses
// the same unified-metrics endpoint as pricing, but with metrics=meter_values.
// Frames where neither fae_usage nor meter_values.energy_active_import_register
// is populated are skipped. Empty result on success is fine — caller checks.
func (p *Pstryk) FetchConsumption(ctx context.Context, start, end time.Time) ([]store.PstrykConsumption, error) {
	token := p.store.GetDefault("tariff.pstryk_token", "")
	if token == "" {
		return nil, fmt.Errorf("pstryk token not configured")
	}

	params := url.Values{}
	params.Set("metrics", "meter_values")
	params.Set("resolution", "hour")
	params.Set("window_start", start.UTC().Format(time.RFC3339))
	params.Set("window_end", end.UTC().Format(time.RFC3339))
	// `for_tz` is only valid for day/month/year resolutions per Pstryk's API
	// (HTTP 400 otherwise). Frames come back with UTC offset markers anyway
	// and we Truncate to UTC top-of-hour when keying, so we don't need it.

	data, err := p.unifiedMetrics(ctx, token, params)
	if err != nil {
		return nil, err
	}

	out := make([]store.PstrykConsumption, 0, len(data.Frames))
	for _, f := range data.Frames {
		hour, err := time.Parse(time.RFC3339, f.Start)
		if err != nil {
			continue
		}
		// Key on the UTC top-of-hour. for_tz handles local-time alignment on the
		// API side; we still store UTC keys so DST transitions don't collide.
		hour = hour.UTC().Truncate(time.Hour)
		kWh, ok := extractFaeUsage(f)
		if !ok {
			continue
		}
		out = append(out, store.PstrykConsumption{
			Hour:     hour,
			EnergyWh: kWh * 1000,
		})
	}
	return out, nil
}

// liveRefreshInterval paces the background live-consumption loop. The graph
// doesn't need to be realtime — a couple of minutes of lag is acceptable and
// keeps Pstryk API usage modest (~30 cycles/hour, 1-2 requests each).
const liveRefreshInterval = 2 * time.Minute

// liveLoop keeps today's finalized consumption fresh in the store: a just-
// finished hour shows up within ~liveRefreshInterval of the rollover instead
// of whenever the hourly backfill tick happens to fire. The in-progress hour
// itself is NOT available from Pstryk (even is_live frames only grow as hours
// finalize) — store.HourlyConsumption derives it from local meter readings.
func (p *Pstryk) liveLoop(ctx context.Context) {
	ticker := time.NewTicker(liveRefreshInterval)
	defer ticker.Stop()
	_ = p.RefreshTodayConsumption(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.RefreshTodayConsumption(ctx)
		}
	}
}

// RefreshTodayConsumption fetches today's hourly frames from Pstryk and
// upserts whatever it returns (finalized hours, plus the in-progress hour
// should Pstryk ever start reporting it). Errors are event-logged (debounced)
// and returned; the next tick retries.
func (p *Pstryk) RefreshTodayConsumption(ctx context.Context) error {
	token := p.store.GetDefault("tariff.pstryk_token", "")
	if token == "" {
		return nil // fetchConsumptionAndStore already warns about this
	}

	currentHour := time.Now().UTC().Truncate(time.Hour)
	loc, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		loc = time.FixedZone("CET", 3600)
	}
	nowLocal := time.Now().In(loc)
	dayStart := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)

	frames, err := p.FetchConsumption(ctx, dayStart, currentHour.Add(time.Hour))
	if err != nil {
		p.warnLive(ctx, currentHour, err)
		return err
	}
	if len(frames) > 0 {
		if err := p.store.UpsertPstrykConsumption(frames); err != nil {
			p.warnLive(ctx, currentHour, err)
			return err
		}
	}

	p.mu.Lock()
	newHour := !p.liveEventHour.Equal(currentHour)
	if newHour {
		p.liveEventHour = currentHour
	}
	p.liveWarnAt = time.Time{}
	p.mu.Unlock()
	if newHour {
		p.pstrykEvents.Info(events.ActionFetchLiveConsumption,
			map[string]any{"hour": currentHour.Format(time.RFC3339)},
			map[string]any{"frames": len(frames)},
		)
	}
	return nil
}

// warnLive event-logs a live-refresh failure at most once per hour, mirroring
// the noTokenWarnAt debounce. Shutdown-canceled contexts are not warned about.
func (p *Pstryk) warnLive(ctx context.Context, hour time.Time, err error) {
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	shouldWarn := time.Since(p.liveWarnAt) >= time.Hour
	if shouldWarn {
		p.liveWarnAt = time.Now()
	}
	p.mu.Unlock()
	if shouldWarn {
		p.pstrykEvents.Warn(events.ActionFetchLiveConsumption,
			map[string]any{"hour": hour.Format(time.RFC3339)},
			map[string]any{"error": err.Error()},
		)
	}
	p.log.Warn("failed to refresh live consumption", "err", err)
}

// extractFaeUsage pulls per-frame imported energy (kWh) from either the
// top-level fae_usage field (newer responses) or meter_values.energy_active_
// import_register (older shape). Both have been observed in the wild via the
// balgerion/ha_Pstryk integration; we accept either.
func extractFaeUsage(f pstrykFrame) (float64, bool) {
	if f.FaeUsage != nil {
		return *f.FaeUsage, true
	}
	if f.Metrics.MeterValues != nil && f.Metrics.MeterValues.EnergyActiveImportRegister != nil {
		return *f.Metrics.MeterValues.EnergyActiveImportRegister, true
	}
	return 0, false
}

const (
	// consumptionBootstrap is the minimum amount of history we want in the
	// table. fetchConsumptionAndStore reaches back this far when the table
	// is empty OR when the earliest stored row doesn't cover the window.
	consumptionBootstrap = 48 * time.Hour
	// consumptionCap caps a single backfill cycle. If a longer gap exists
	// (e.g., year-long outage), it's filled over multiple cycles. Also
	// prevents runaway fetches when LatestPstrykHour returns something
	// unexpectedly stale.
	consumptionCap = 30 * 24 * time.Hour
	// consumptionChunk is the per-request window. The pricing path uses 48h
	// successfully; we go wider for consumption because it's pulled less
	// often. Adjust if Pstryk returns 4xx for large windows.
	consumptionChunk = 7 * 24 * time.Hour
)

// fetchConsumptionAndStore detects the gap between the latest stored hour and
// now, pulls Pstryk consumption frames to fill it, upserts them, and triggers
// a daily-idle rebuild for each affected date. Idempotent — returns quickly
// when there's nothing new to fetch (steady state during normal operation).
func (p *Pstryk) fetchConsumptionAndStore(ctx context.Context) {
	token := p.store.GetDefault("tariff.pstryk_token", "")
	if token == "" {
		p.mu.Lock()
		shouldWarn := time.Since(p.noTokenWarnAt) >= 24*time.Hour
		if shouldWarn {
			p.noTokenWarnAt = time.Now()
		}
		p.mu.Unlock()
		if shouldWarn {
			p.pstrykEvents.Warn(events.ActionFetchConsumption, nil,
				map[string]any{"skipped": true, "reason": "token not configured"},
			)
		}
		return
	}
	// Token present — clear the debounce so a later removal warns again.
	p.mu.Lock()
	p.noTokenWarnAt = time.Time{}
	p.mu.Unlock()

	now := time.Now().UTC()
	// Pstryk only finalizes past hours; clamp to the previous top-of-hour.
	until := now.Truncate(time.Hour)
	desired := until.Add(-consumptionBootstrap)

	// Bring the table up to `desired` if we don't already reach that far back;
	// otherwise just fill the gap between the latest stored hour and now.
	// This is what lets the bootstrap window grow (e.g. for a one-off deeper
	// historical fetch) without first wiping the table.
	since := desired
	if earliest, ok := p.store.EarliestPstrykHour(); ok && !earliest.After(desired) {
		if latest, ok := p.store.LatestPstrykHour(); ok {
			// Re-fetch from `latest` inclusive: the live loop stores a partial
			// value for the in-progress hour, so the newest row may need its
			// finalized value (e.g., a partial left behind before a restart).
			since = latest
		}
	}
	if floor := now.Add(-consumptionCap); since.Before(floor) {
		since = floor
	}
	if !until.After(since) {
		return // nothing to fetch
	}

	totalHours := 0
	var totalWh float64
	// Key on local-midnight time values so date math is consistent end-to-end
	// (SnapshotDailyIdle/RebuildDailyIdle build dayStart from day.Location()).
	// String-format/reparse round-trips were silently shifting dates by the
	// UTC↔Warsaw offset for hours near midnight.
	affectedDates := make(map[time.Time]struct{})

	for chunkStart := since; chunkStart.Before(until); {
		if ctx.Err() != nil {
			return // shutting down
		}
		chunkEnd := chunkStart.Add(consumptionChunk)
		if chunkEnd.After(until) {
			chunkEnd = until
		}
		frames, err := p.FetchConsumption(ctx, chunkStart, chunkEnd)
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down; not an error
			}
			p.log.Warn("failed to fetch consumption", "err", err,
				"from", chunkStart.Format(time.RFC3339),
				"to", chunkEnd.Format(time.RFC3339),
			)
			p.pstrykEvents.Error(events.ActionFetchConsumption,
				map[string]any{
					"windowStart": chunkStart.Format(time.RFC3339),
					"windowEnd":   chunkEnd.Format(time.RFC3339),
				},
				err,
			)
			return // bail; next tick will retry
		}
		if len(frames) > 0 {
			if err := p.store.UpsertPstrykConsumption(frames); err != nil {
				p.log.Warn("failed to upsert consumption", "err", err)
				p.pstrykEvents.Error(events.ActionFetchConsumption, nil, err)
				return
			}
			for _, f := range frames {
				totalWh += f.EnergyWh
				local := f.Hour.In(time.Local)
				dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
				affectedDates[dayStart] = struct{}{}
			}
			totalHours += len(frames)
		}
		p.pstrykEvents.Info(events.ActionFetchConsumption,
			map[string]any{
				"windowStart": chunkStart.Format(time.RFC3339),
				"windowEnd":   chunkEnd.Format(time.RFC3339),
			},
			map[string]any{"count": len(frames)},
		)
		chunkStart = chunkEnd
	}

	if totalHours == 0 {
		return
	}

	rebuilt := make([]string, 0, len(affectedDates))
	for day := range affectedDates {
		if ctx.Err() != nil {
			return // shutting down
		}
		if err := p.store.RebuildDailyIdle(day); err != nil {
			p.log.Warn("failed to rebuild daily idle", "date", day.Format(time.DateOnly), "err", err)
			continue
		}
		rebuilt = append(rebuilt, day.Format(time.DateOnly))
	}

	p.pstrykEvents.Info(events.ActionBackfillConsumption,
		map[string]any{
			"since": since.Format(time.RFC3339),
			"until": until.Format(time.RFC3339),
		},
		map[string]any{
			"hoursAdded":   totalHours,
			"totalWh":      totalWh,
			"datesRebuilt": rebuilt,
		},
	)
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
