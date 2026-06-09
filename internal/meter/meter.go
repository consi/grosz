package meter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/store"
)

// meterResetThresholdWh is the smallest drop in cumulative meter energy that
// we treat as a counter reset. Smaller decreases (sensor noise, brief stale
// reads) are clamped silently to the previous effective value.
const meterResetThresholdWh = 1000.0

// meterEnergyOffsetSetting is the settings key holding the cumulative offset
// applied to raw meter readings to keep the stored series monotonic across
// counter resets.
const meterEnergyOffsetSetting = "meter.energy_offset_wh"

type stateResponse struct {
	MultiSensor struct {
		Sensors []sensor `json:"sensors"`
	} `json:"multiSensor"`
}

type sensor struct {
	ID    int     `json:"id"`
	Type  string  `json:"type"`
	Value float64 `json:"value"`
}

// Phase holds per-phase readings.
type Phase struct {
	Power   float64 `json:"power"`   // W
	Current float64 `json:"current"` // A
	Voltage float64 `json:"voltage"` // V
}

// LiveState is the latest full meter snapshot.
type LiveState struct {
	TotalPower float64 `json:"totalPower"` // W
	Frequency  float64 `json:"frequency"`  // Hz
	Phases     []Phase `json:"phases"`
	Timestamp  string  `json:"timestamp"`
}

// Poller periodically reads a Pstryk meter and stores readings.
type Poller struct {
	store  *store.Store
	events *events.Bound
	log    *slog.Logger
	client *http.Client

	mu               sync.RWMutex
	lastPower        float64
	lastStore        time.Time // last time we wrote to DB
	lastSyslog       time.Time // last time we recorded a system event
	lastSnapshotDate string    // tracks day boundary for idle snapshots
	lastSnapshotTime time.Time // tracks hourly refresh of today's idle
	live             LiveState
	onUpdate         func(LiveState)

	energyOffset  float64 // cumulative offset added to raw meter values
	lastEffective float64 // last value we wrote to DB (post-offset)
	energyInited  bool    // energy state loaded from settings/DB

	cancel context.CancelFunc
}

// New creates and starts a meter poller.
func New(st *store.Store, log *slog.Logger) *Poller {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Poller{
		store:  st,
		events: events.For(events.SourceMeter, st),
		log:    log.With("component", "meter"),
		client: &http.Client{Timeout: 5 * time.Second},
		cancel: cancel,
	}
	go p.loop(ctx)
	return p
}

// Stop shuts down the poller.
func (p *Poller) Stop() { p.cancel() }

// LastPower returns the most recent power reading in Watts.
func (p *Poller) LastPower() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastPower
}

// Live returns the latest full meter state.
func (p *Poller) Live() LiveState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.live
}

// SetOnUpdate registers a callback for each new meter reading.
func (p *Poller) SetOnUpdate(fn func(LiveState)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onUpdate = fn
}

// initEnergyState loads the persisted offset and the last stored effective
// reading. Called lazily on the first poll so a fresh DB / first-run scenario
// behaves correctly (no data → 0 offset, 0 lastEffective → no spurious reset
// detection).
func (p *Poller) initEnergyState() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.energyInited {
		return
	}
	p.energyOffset = p.store.GetFloat(meterEnergyOffsetSetting, 0)
	if last, err := p.store.LatestMeterReading(); err == nil && last != nil {
		p.lastEffective = last.EnergyWh
	}
	p.energyInited = true
}

// adjustEnergy converts a raw cumulative meter reading into the effective value
// to store, applying the persisted offset and detecting/handling counter
// resets. Resets bump the offset so the new effective value continues from the
// previous one (the reset minute counts as ~0 Wh consumed). Tiny decreases
// below the reset threshold are clamped to the previous effective value.
func (p *Poller) adjustEnergy(rawWh float64) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	effective := rawWh + p.energyOffset
	if p.lastEffective > 0 && effective < p.lastEffective-meterResetThresholdWh {
		prev := p.lastEffective
		p.energyOffset += p.lastEffective - effective
		effective = rawWh + p.energyOffset
		if err := p.store.Set(meterEnergyOffsetSetting, strconv.FormatFloat(p.energyOffset, 'f', -1, 64)); err != nil {
			p.log.Warn("failed to persist meter offset", "err", err)
		}
		p.log.Warn("meter counter reset detected",
			"raw_wh", rawWh,
			"previous_effective_wh", prev,
			"new_offset_wh", p.energyOffset,
		)
		p.events.Warn(events.ActionMeterResetDetected,
			map[string]any{"rawWh": rawWh, "previousEffectiveWh": prev},
			map[string]any{"newOffsetWh": p.energyOffset},
		)
	} else if effective < p.lastEffective {
		// Small decrease (within threshold) — clamp to keep the series monotonic.
		effective = p.lastEffective
	}
	p.lastEffective = effective
	return effective
}

func (p *Poller) loop(ctx context.Context) {
	for {
		meterURL := p.store.GetDefault("meter.url", "")
		interval := p.store.GetInt("meter.interval", 5)
		if interval < 1 {
			interval = 1
		}

		if meterURL == "" {
			// No meter configured, check again later
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		p.poll(meterURL)

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

func (p *Poller) poll(baseURL string) {
	resp, err := p.client.Get(baseURL + "/state")
	if err != nil {
		p.log.Debug("meter fetch failed", "err", err)
		p.events.Error(events.ActionMeterPoll, nil, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		p.log.Debug("meter bad status", "status", resp.StatusCode)
		p.events.Warn(events.ActionMeterPoll, nil,
			map[string]any{"error": fmt.Sprintf("bad status %d", resp.StatusCode)},
		)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return
	}

	var state stateResponse
	if err := json.Unmarshal(body, &state); err != nil {
		p.log.Debug("meter parse failed", "err", err)
		return
	}

	// Parse sensor data into structured state
	sensorVal := func(id int, typ string) float64 {
		for _, s := range state.MultiSensor.Sensors {
			if s.ID == id && s.Type == typ {
				return s.Value
			}
		}
		return 0
	}

	powerW := sensorVal(0, "activePower")
	rawEnergyWh := sensorVal(0, "forwardActiveEnergy")
	p.initEnergyState()
	energyWh := p.adjustEnergy(rawEnergyWh)

	live := LiveState{
		TotalPower: powerW,
		Frequency:  sensorVal(0, "frequency") / 1000,
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
	}
	for phase := 1; phase <= 3; phase++ {
		live.Phases = append(live.Phases, Phase{
			Power:   sensorVal(phase, "activePower"),
			Current: sensorVal(phase, "current") / 1000,
			Voltage: sensorVal(phase, "voltage") / 10,
		})
	}

	p.mu.Lock()
	p.lastPower = powerW
	p.live = live
	cb := p.onUpdate
	p.mu.Unlock()

	if cb != nil {
		cb(live)
	}

	// Store at most once per minute to avoid DB bloat
	// At 5s polling = 12 reads/min, we only write 1
	p.mu.RLock()
	sinceLastStore := time.Since(p.lastStore)
	p.mu.RUnlock()

	if sinceLastStore >= time.Minute {
		// energy_wh column is kept for MeterDeltaKWh/SnapshotDailyIdle's
		// recent-48h cost accounting. The dashboard chart and post-downtime
		// idle rebuilds read from pstryk_consumption (Pstryk API) instead, so
		// don't be tempted to drop this column thinking it's redundant.
		reading := store.MeterReading{
			Timestamp: time.Now(),
			PowerW:    powerW,
			EnergyWh:  energyWh,
		}
		if err := p.store.InsertMeterReading(reading); err != nil {
			p.log.Warn("failed to store meter reading", "err", err)
			return
		}

		// Store per-phase power
		if len(live.Phases) >= 3 {
			_ = p.store.InsertPhaseReading(store.PhaseReading{
				Timestamp: time.Now(),
				Phase1W:   live.Phases[0].Power,
				Phase2W:   live.Phases[1].Power,
				Phase3W:   live.Phases[2].Power,
			})
		}

		p.mu.Lock()
		p.lastStore = time.Now()
		p.mu.Unlock()

		// Snapshot daily idle before purging meter data
		p.snapshotIdle()

		// Purge old readings (keep 48h)
		if err := p.store.PurgeMeterReadings(48 * time.Hour); err != nil {
			p.log.Warn("failed to purge old readings", "err", err)
		}
		_ = p.store.PurgePhaseReadings(48 * time.Hour)

		// Record system event at most once per minute
		p.mu.RLock()
		sinceLastSyslog := time.Since(p.lastSyslog)
		p.mu.RUnlock()
		if sinceLastSyslog >= time.Minute {
			// energyWh flows via pstryk:fetchConsumption (authoritative source).
			p.events.Info(events.ActionMeterPoll, nil,
				map[string]any{"powerW": powerW},
			)
			p.mu.Lock()
			p.lastSyslog = time.Now()
			p.mu.Unlock()
		}
	}
}

// snapshotIdle persists daily idle energy. Called once per minute.
// On day boundary: snapshots yesterday and today.
// Hourly: re-snapshots today to keep partial-day data current.
func (p *Poller) snapshotIdle() {
	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.Add(-24 * time.Hour)

	p.mu.RLock()
	lastDate := p.lastSnapshotDate
	lastTime := p.lastSnapshotTime
	p.mu.RUnlock()

	dayChanged := today != lastDate

	if dayChanged {
		// Snapshot yesterday (finalizes it before data ages past 48h)
		if err := p.store.SnapshotDailyIdle(yesterday); err != nil {
			p.log.Warn("failed to snapshot yesterday idle", "err", err)
		}
		// Snapshot today (partial)
		if err := p.store.SnapshotDailyIdle(now); err != nil {
			p.log.Warn("failed to snapshot today idle", "err", err)
		}
		p.mu.Lock()
		p.lastSnapshotDate = today
		p.lastSnapshotTime = now
		p.mu.Unlock()
		return
	}

	// Hourly refresh of today's partial data
	if time.Since(lastTime) >= time.Hour {
		if err := p.store.SnapshotDailyIdle(now); err != nil {
			p.log.Warn("failed to snapshot today idle", "err", err)
		}
		p.mu.Lock()
		p.lastSnapshotTime = now
		p.mu.Unlock()
	}
}
