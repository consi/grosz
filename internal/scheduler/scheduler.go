package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

// Charger abstracts OCPP charge point operations for testability.
type Charger interface {
	ConnectorStatus(cpID string, connectorID int) (status string, txnID int, connected bool)
	RemoteStartTransaction(cpID, idTag string, connectorID int) error
	RemoteStopTransaction(cpID string, txnID int) error
	ClearChargingProfile(cpID string) error
	SetChargingProfile(cpID string, connectorID int, profile *types.ChargingProfile) error
}

// ServerCharger adapts *ocpp.Server to the Charger interface.
type ServerCharger struct{ S *ocpp.Server }

func (a *ServerCharger) ConnectorStatus(cpID string, connID int) (string, int, bool) {
	cp := a.S.ChargePoint(cpID)
	if cp == nil || !cp.IsConnected() {
		return "", 0, false
	}
	snap := cp.Snapshot()
	c, ok := snap.Connectors[connID]
	if !ok {
		return "", 0, true
	}
	return c.Status, c.TransactionID, true
}

func (a *ServerCharger) RemoteStartTransaction(cpID, idTag string, connID int) error {
	return a.S.RemoteStartTransaction(cpID, idTag, connID)
}

func (a *ServerCharger) RemoteStopTransaction(cpID string, txnID int) error {
	return a.S.RemoteStopTransaction(cpID, txnID)
}

func (a *ServerCharger) ClearChargingProfile(cpID string) error {
	return a.S.ClearChargingProfile(cpID)
}

func (a *ServerCharger) SetChargingProfile(cpID string, connID int, p *types.ChargingProfile) error {
	return a.S.SetChargingProfile(cpID, connID, p)
}

// SchedulePeriod represents a single period in the charging schedule.
type SchedulePeriod struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Power  float64   `json:"power"`            // W, 0 = don't charge
	Price  float64   `json:"price"`            // PLN/kWh (volume-weighted average for sub-hour periods)
	Source string    `json:"source,omitempty"` // "auto" (omitted) or "user_force"
}

// IsForce reports whether the period was materialized from a user force override.
func (p SchedulePeriod) IsForce() bool { return p.Source == sourceUserForce }

const (
	sourceAuto      = "auto"
	sourceUserForce = "user_force"
)

// ScheduleSlot represents one day's charging plan.
type ScheduleSlot struct {
	Date      string           `json:"date"` // YYYY-MM-DD of the deadline day
	Deadline  time.Time        `json:"deadline"`
	Periods   []SchedulePeriod `json:"periods"`
	Cost      float64          `json:"cost"`
	Energy    float64          `json:"energy"`
	Cancelled bool             `json:"cancelled"`
}

// Schedule is a computed multi-day charging schedule.
type Schedule struct {
	Slots    []ScheduleSlot `json:"slots"`
	Cost     float64        `json:"cost"`     // estimated PLN (active slots)
	Energy   float64        `json:"energy"`   // total kWh (active slots)
	Deadline time.Time      `json:"deadline"` // latest deadline
}

// ActivePeriods returns all periods from non-cancelled slots.
func (s *Schedule) ActivePeriods() []SchedulePeriod {
	if s == nil {
		return nil
	}
	var all []SchedulePeriod
	for _, slot := range s.Slots {
		if !slot.Cancelled {
			all = append(all, slot.Periods...)
		}
	}
	return all
}

// Config holds scheduler configuration from settings.
type Config struct {
	TargetEnergy    float64   // kWh (manual override or computed from SoC)
	Deadline        time.Time // first deadline (used for hour extraction)
	MaxPower        float64   // W
	MinPower        float64   // W
	BatteryCapacity float64   // kWh, 0 = unknown
	TargetSoC       float64   // %, 0 = use TargetEnergy directly
	SkipAboveSoC    float64   // %, 0 = never skip
	MinSoC          float64   // %, 0 = disabled; if SoC below this, ignore MaxPrice
	CurrentSoC      float64   // %
	MaxPrice        float64   // PLN/kWh, 0 = no limit
	ChargeHeadroom  float64   // %, extra capacity to account for power oscillations (default 3)
}

// Scheduler orchestrates the cheapest-window charging algorithm.
type Scheduler struct {
	charger Charger
	tariff  tariff.Provider
	store   *store.Store
	log     *slog.Logger

	mu            sync.RWMutex
	current       *Schedule
	config        *Config
	livePower     float64           // W, 0 = use MaxPower
	skipReason    string            // if non-empty, schedule was skipped
	skipReasonKey string            // i18n key for skip reason
	skipParams    map[string]string // parameters for the skip reason template

	notifyCh chan struct{} // trigger immediate recompute + control
	cancel   context.CancelFunc

	debounceMu    sync.Mutex
	debounceTimer *time.Timer

	// Cooldown tracking for cloud-proxied chargers (e.g. Zappi via myenergi cloud).
	// Commands are relayed slowly; duplicates queue up and cause issues.
	lastStartSent   time.Time
	lastStopSent    time.Time
	lastProfileHash string    // hash of last successfully applied charging profile
	lastProfileSent time.Time // when last SetChargingProfile was sent

	// Tracks the last recompute timestamp. Periods of the previous schedule
	// whose End falls between lastRecomputeAt and now are candidates for
	// missed-period detection (no overlapping charging session).
	lastRecomputeAt time.Time

	onRecompute func() // optional callback fired after every recompute
}

// SetOnRecompute registers a callback invoked after every recompute (including
// skipped recomputes). Used to push schedule changes over SSE.
func (s *Scheduler) SetOnRecompute(fn func()) {
	s.mu.Lock()
	s.onRecompute = fn
	s.mu.Unlock()
}

func (s *Scheduler) fireOnRecompute() {
	s.mu.RLock()
	fn := s.onRecompute
	s.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// persistedScheduleKey is the settings key holding the JSON-encoded
// active schedule. Persisted across restarts so the original schedule
// survives a power loss / server crash and the system doesn't re-pick
// "current hour" simply because windowStart=now puts it in range.
const persistedScheduleKey = "scheduler.persisted_schedule"

// New creates a Scheduler, restores any persisted schedule, and starts
// its background loop.
func New(charger Charger, tp tariff.Provider, st *store.Store, log *slog.Logger) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		charger:  charger,
		tariff:   tp,
		store:    st,
		log:      log.With("component", "scheduler"),
		notifyCh: make(chan struct{}, 1),
		cancel:   cancel,
	}
	s.loadPersistedSchedule()
	go s.loop(ctx)
	return s
}

// loadPersistedSchedule restores s.current from the settings store, pruning
// any slots whose every period ends before now. If the persisted blob is
// missing or empty, leaves s.current as nil (loop() will trigger a normal
// recompute on the first tick).
func (s *Scheduler) loadPersistedSchedule() {
	raw := s.store.GetDefault(persistedScheduleKey, "")
	if raw == "" {
		return
	}

	var sched Schedule
	if err := json.Unmarshal([]byte(raw), &sched); err != nil {
		s.log.Warn("failed to decode persisted schedule, ignoring", "err", err)
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "loadSchedule", Level: "warn",
			Result: map[string]any{"error": err.Error()},
		})
		return
	}

	now := timeNow()
	dropped := 0
	keep := sched.Slots[:0]
	for _, slot := range sched.Slots {
		// Keep only slots that have at least one period ending in the future.
		live := false
		for _, p := range slot.Periods {
			if p.End.After(now) {
				live = true
				break
			}
		}
		if live {
			keep = append(keep, slot)
		} else {
			dropped++
		}
	}
	sched.Slots = keep

	if len(sched.Slots) == 0 {
		s.log.Info("persisted schedule had no live slots, starting fresh", "droppedPastSlots", dropped)
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "loadSchedule",
			Result: map[string]any{"restored": false, "droppedPastSlots": dropped},
		})
		return
	}

	s.mu.Lock()
	s.current = &sched
	s.mu.Unlock()

	activePeriods := 0
	for _, slot := range sched.Slots {
		if !slot.Cancelled {
			activePeriods += len(slot.Periods)
		}
	}
	s.log.Info("persisted schedule restored",
		"slots", len(sched.Slots),
		"activePeriods", activePeriods,
		"droppedPastSlots", dropped,
	)
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "loadSchedule",
		Result: map[string]any{
			"restored":         true,
			"slots":            len(sched.Slots),
			"activePeriods":    activePeriods,
			"droppedPastSlots": dropped,
		},
	})
}

// saveCurrent persists s.current (or empty string if nil) to the settings
// store. Called after every recompute and after every user mutation that
// changes the schedule. Caller must NOT hold s.mu — this method takes its
// own RLock.
func (s *Scheduler) saveCurrent() {
	s.mu.RLock()
	cur := s.current
	s.mu.RUnlock()

	if cur == nil {
		if err := s.store.Set(persistedScheduleKey, ""); err != nil {
			s.log.Warn("failed to clear persisted schedule", "err", err)
		}
		return
	}

	data, err := json.Marshal(cur)
	if err != nil {
		s.log.Warn("failed to encode schedule for persistence", "err", err)
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "saveSchedule", Level: "warn",
			Result: map[string]any{"error": err.Error()},
		})
		return
	}
	if err := s.store.Set(persistedScheduleKey, string(data)); err != nil {
		s.log.Warn("failed to persist schedule", "err", err)
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "saveSchedule", Level: "warn",
			Result: map[string]any{"error": err.Error()},
		})
	}
}

// Stop shuts down the scheduler.
func (s *Scheduler) Stop() {
	s.debounceMu.Lock()
	if s.debounceTimer != nil {
		s.debounceTimer.Stop()
	}
	s.debounceMu.Unlock()
	s.cancel()
}

// ResetProfileState clears the in-memory profile hash and cooldown,
// forcing the next applyProfile to re-send. Called on BootNotification
// because the charger lost its profile state after a restart.
func (s *Scheduler) ResetProfileState() {
	s.mu.Lock()
	s.lastProfileHash = ""
	s.lastProfileSent = time.Time{}
	s.mu.Unlock()
}

// Schedule returns the current schedule, or nil if none.
func (s *Scheduler) Schedule() *Schedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return nil
	}
	cp := *s.current
	cp.Slots = make([]ScheduleSlot, len(s.current.Slots))
	for i, slot := range s.current.Slots {
		cp.Slots[i] = slot
		cp.Slots[i].Periods = make([]SchedulePeriod, len(slot.Periods))
		copy(cp.Slots[i].Periods, slot.Periods)
	}
	return &cp
}

// SkipReason returns why the schedule was skipped, or "" if active.
func (s *Scheduler) SkipReason() (string, string, map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.skipReason, s.skipReasonKey, s.skipParams
}

// SetConfig overrides the scheduler config for this session and recomputes.
func (s *Scheduler) SetConfig(cfg Config) {
	s.mu.Lock()
	s.config = &cfg
	s.mu.Unlock()
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(),
		Source:    "scheduler",
		Action:    "setConfig",
		Input: map[string]any{
			"targetEnergy": cfg.TargetEnergy,
			"deadline":     cfg.Deadline.Format(time.RFC3339),
			"maxPower":     cfg.MaxPower,
			"targetSoC":    cfg.TargetSoC,
			"currentSoC":   cfg.CurrentSoC,
			"maxPrice":     cfg.MaxPrice,
		},
		Result: map[string]any{"applied": true},
	})
	s.recompute()
}

// ReloadConfig drops the cached Config so the next recompute reads fresh
// values from the store, then triggers an immediate recompute. Call this
// whenever scheduler-relevant settings change.
func (s *Scheduler) ReloadConfig() {
	s.mu.Lock()
	s.config = nil
	s.mu.Unlock()
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(),
		Source:    "scheduler",
		Action:    "reloadConfig",
		Result:    map[string]any{"invalidated": true},
	})
	s.NotifyImmediate()
}

// ClearSchedule removes the current schedule and clears the charging profile.
func (s *Scheduler) ClearSchedule() {
	s.mu.Lock()
	s.current = nil
	s.config = nil
	s.skipReason = ""
	s.skipReasonKey = ""
	s.skipParams = nil
	hadProfile := s.lastProfileHash != ""
	s.lastProfileHash = ""
	s.mu.Unlock()

	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	clearedProfile := false
	if cpID != "" && hadProfile {
		// Only send ClearChargingProfile if no active transaction.
		// Active transaction means charger is busy; profile is irrelevant.
		_, txnID, connected := s.charger.ConnectorStatus(cpID, 1)
		if connected && txnID == 0 {
			_ = s.charger.ClearChargingProfile(cpID)
			clearedProfile = true
		}
	}
	s.log.Info("schedule cleared", "clearedProfile", clearedProfile)
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(),
		Source:    "scheduler",
		Action:    "clearSchedule",
		Input:     map[string]any{"cpID": cpID},
		Result:    map[string]any{"cleared": true, "clearedProfile": clearedProfile},
	})
	s.saveCurrent()
}

// CancelSlot cancels a specific day's slot by date string (YYYY-MM-DD).
func (s *Scheduler) CancelSlot(date string) bool {
	s.mu.Lock()
	if s.current == nil {
		s.mu.Unlock()
		return false
	}
	found := false
	for i, slot := range s.current.Slots {
		if slot.Date == date && !slot.Cancelled {
			s.current.Slots[i].Cancelled = true
			found = true
			break
		}
	}
	if found {
		s.recalcTotals()
	}
	s.mu.Unlock()

	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(),
		Source:    "scheduler",
		Action:    "cancelSlot",
		Input:     map[string]any{"date": date},
		Result:    map[string]any{"found": found},
	})
	if found {
		s.saveCurrent()
		// If we were charging in this slot's period, stop
		s.controlCharging()
	}
	return found
}

// RestoreSlot restores a previously cancelled slot by date string (YYYY-MM-DD).
func (s *Scheduler) RestoreSlot(date string) bool {
	s.mu.Lock()
	if s.current == nil {
		s.mu.Unlock()
		return false
	}
	found := false
	for i, slot := range s.current.Slots {
		if slot.Date == date && slot.Cancelled {
			s.current.Slots[i].Cancelled = false
			found = true
			break
		}
	}
	if found {
		s.recalcTotals()
	}
	s.mu.Unlock()

	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(),
		Source:    "scheduler",
		Action:    "restoreSlot",
		Input:     map[string]any{"date": date},
		Result:    map[string]any{"found": found},
	})
	if found {
		s.saveCurrent()
		// If we're now in this slot's period, start charging
		s.controlCharging()
	}
	return found
}

// recalcTotals updates the schedule's Cost and Energy from active slots.
// Must be called with mu held.
func (s *Scheduler) recalcTotals() {
	if s.current == nil {
		return
	}
	var cost, energy float64
	for _, slot := range s.current.Slots {
		if !slot.Cancelled {
			cost += slot.Cost
			energy += slot.Energy
		}
	}
	s.current.Cost = math.Round(cost*100) / 100
	s.current.Energy = math.Round(energy*100) / 100
}

// cloneSlot returns a deep copy of slot with its Periods slice copied,
// so callers can hand the result to a different *Schedule without aliasing
// the source slot's backing array.
func cloneSlot(slot ScheduleSlot) ScheduleSlot {
	cp := slot
	cp.Periods = append([]SchedulePeriod(nil), slot.Periods...)
	return cp
}

// preservedSchedule wraps a single slot in a fresh *Schedule with totals
// derived from the slot. Used when an in-progress slot must be kept while
// every other slot is dropped (destructive recompute branches).
func preservedSchedule(slot ScheduleSlot) *Schedule {
	return &Schedule{
		Slots:    []ScheduleSlot{slot},
		Cost:     slot.Cost,
		Energy:   slot.Energy,
		Deadline: slot.Deadline,
	}
}

// preserveActiveSlot keeps an in-progress slot intact when recompute would
// otherwise drop the schedule. The recomputed schedule reduces to that single
// slot; future-day slots from the previous schedule are dropped because the
// branch that triggered preservation said no future charging is warranted.
// skipReason is left empty — s.current != nil suppresses the UI's skip notice.
func (s *Scheduler) preserveActiveSlot(slot *ScheduleSlot, wouldSkipKey string, input map[string]any) {
	preserved := preservedSchedule(cloneSlot(*slot))

	s.mu.Lock()
	s.current = preserved
	s.skipReason = ""
	s.skipReasonKey = ""
	s.skipParams = nil
	s.mu.Unlock()

	s.log.Info("preserving in-progress slot through recompute",
		"slotDate", slot.Date, "wouldSkip", wouldSkipKey)

	result := map[string]any{
		"preservedActiveSlot": true,
		"slotDate":            slot.Date,
		"wouldSkip":           wouldSkipKey,
	}
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "recompute",
		Input:  input,
		Result: result,
	})

	// Re-applying the same profile is a no-op via hash dedup but keeps the
	// charger's profile state consistent if the previous apply was lost.
	s.applyProfile(preserved)
	s.saveCurrent()
	s.fireOnRecompute()
}

// detectMissedPeriods scans the previous schedule for charging periods that
// elapsed since the last recompute and emits a `missed_period` system event
// for any that had no overlapping charging session — i.e. the car wasn't
// plugged in (or refused to charge) when its scheduled hour came.
//
// Detection is observability-only. The recompute logic separately drives a
// new schedule from current SoC; this function exists so a user reviewing
// the system_events log can see *why* the schedule has shifted to add
// recovery hours.
func (s *Scheduler) detectMissedPeriods(prev *Schedule) {
	now := timeNow()
	defer func() {
		s.mu.Lock()
		s.lastRecomputeAt = now
		s.mu.Unlock()
	}()

	if prev == nil {
		return
	}
	s.mu.RLock()
	last := s.lastRecomputeAt
	s.mu.RUnlock()
	if last.IsZero() {
		return // first recompute — no prior window to scan
	}

	for _, p := range prev.ActivePeriods() {
		if p.Power <= 0 {
			continue
		}
		// Only inspect periods that fully ended within the scan window
		// (last, now]. Periods still in progress or already finalised in a
		// prior recompute are skipped.
		if !p.End.After(last) || p.End.After(now) {
			continue
		}

		overlaps, err := s.store.SessionsOverlapping(p.Start, p.End)
		if err != nil {
			s.log.Debug("missed-period overlap query failed", "err", err)
			continue
		}
		if overlaps {
			continue
		}

		hrs := p.End.Sub(p.Start).Hours()
		plannedKWh := math.Round(p.Power/1000*hrs*100) / 100

		s.log.Warn("scheduled charging period missed (no session overlap)",
			"start", p.Start, "end", p.End, "plannedKWh", plannedKWh, "source", p.Source)
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(),
			Source:    "scheduler",
			Action:    "missed_period",
			Level:     "warn",
			Input: map[string]any{
				"start":      p.Start.Format(time.RFC3339),
				"end":        p.End.Format(time.RFC3339),
				"plannedKWh": plannedKWh,
				"source":     p.Source,
			},
			Result: map[string]any{"reason": "no_overlapping_session"},
		})
	}
}

// mergeActiveSlotPreservingActive merges the in-progress slot from the
// previous schedule into the recomputed sched, replacing sched's same-date
// slot. The active period (the one whose [Start, End) contains now and has
// Power > 0) is preserved verbatim — never shortened, extended, split, or
// shifted, so an ongoing OCPP transaction keeps its scheduled bounds.
//
// Periods in the previous slot that fall after the active period are
// dropped: recompute owns the post-active future. Recomputed periods whose
// Start is at or after active.End are appended (after dropping any that
// overlap the active period). Hour-level periods are kept separate at the
// data layer; consolidation into continuous OCPP blocks happens in
// BuildProfile so the user can still adjust each hourly slot independently.
//
// If the in-progress slot has no period containing now (race at the boundary),
// falls back to a strict pin via mergeInProgressSlot.
func mergeActiveSlotPreservingActive(sched *Schedule, in ScheduleSlot) *Schedule {
	now := timeNow()
	activeIdx := -1
	for i, p := range in.Periods {
		if !now.Before(p.Start) && now.Before(p.End) && p.Power > 0 {
			activeIdx = i
			break
		}
	}
	if activeIdx < 0 {
		return mergeInProgressSlot(sched, in)
	}

	var recomputed *ScheduleSlot
	if sched != nil {
		for i := range sched.Slots {
			if sched.Slots[i].Date == in.Date {
				recomputed = &sched.Slots[i]
				break
			}
		}
	}

	merged := cloneSlot(in)
	// Drop everything after the active period — recompute owns post-active future.
	merged.Periods = merged.Periods[:activeIdx+1]
	active := merged.Periods[activeIdx]

	if recomputed != nil {
		for _, p := range recomputed.Periods {
			// Skip periods that overlap the active period — active owns its time range.
			if p.Start.Before(active.End) {
				continue
			}
			merged.Periods = append(merged.Periods, p)
		}
		sort.Slice(merged.Periods, func(i, j int) bool {
			return merged.Periods[i].Start.Before(merged.Periods[j].Start)
		})
	}

	var totC, totE float64
	for _, p := range merged.Periods {
		hrs := p.End.Sub(p.Start).Hours()
		e := p.Power / 1000 * hrs
		totE += e
		totC += p.Price * e
	}
	merged.Cost = math.Round(totC*100) / 100
	merged.Energy = math.Round(totE*100) / 100

	return mergeInProgressSlot(sched, merged)
}

// mergeInProgressSlot returns sched with the slot for in.Date replaced by in
// (or inserted in date order if absent), then recomputes Cost/Energy/Deadline
// over non-cancelled slots. Mutates sched in place.
func mergeInProgressSlot(sched *Schedule, in ScheduleSlot) *Schedule {
	if sched == nil {
		return preservedSchedule(in)
	}
	replaced := false
	for i, slot := range sched.Slots {
		if slot.Date == in.Date {
			sched.Slots[i] = in
			replaced = true
			break
		}
	}
	if !replaced {
		insertAt := len(sched.Slots)
		for i, slot := range sched.Slots {
			if in.Date < slot.Date {
				insertAt = i
				break
			}
		}
		sched.Slots = append(sched.Slots, ScheduleSlot{})
		copy(sched.Slots[insertAt+1:], sched.Slots[insertAt:])
		sched.Slots[insertAt] = in
	}

	var cost, energy float64
	var deadline time.Time
	for _, slot := range sched.Slots {
		if slot.Cancelled {
			continue
		}
		cost += slot.Cost
		energy += slot.Energy
		if slot.Deadline.After(deadline) {
			deadline = slot.Deadline
		}
	}
	sched.Cost = math.Round(cost*100) / 100
	sched.Energy = math.Round(energy*100) / 100
	sched.Deadline = deadline
	return sched
}

// notifyDebounce is the delay before a debounced Notify fires.
const notifyDebounce = 5 * time.Second

// Notify triggers a debounced recompute and charge control check.
// Multiple calls within notifyDebounce are coalesced into one.
func (s *Scheduler) Notify() {
	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()
	if s.debounceTimer != nil {
		s.debounceTimer.Stop()
	}
	s.debounceTimer = time.AfterFunc(notifyDebounce, func() {
		select {
		case s.notifyCh <- struct{}{}:
		default:
		}
	})
}

// NotifyImmediate triggers an immediate recompute and charge control check.
// Use for physical events (connector status changes, boot) that should not wait.
func (s *Scheduler) NotifyImmediate() {
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

// OnModeChanged is the synchronous user-driven mode-change entry point.
// Compared to a plain Notify():
//   - clears the start/stop cooldowns so a user click is never silently
//     swallowed by a stuck cloud-proxy timestamp from a prior attempt
//   - runs controlCharging synchronously (no 5s debounce, no 30s tick wait)
//     against the explicit new mode (so a rapid double-toggle doesn't race
//     the store)
//   - bypasses the SuspendedEV early-return: when the user explicitly forces
//     charging, we attempt the RemoteStartTransaction even though Zappi may
//     ignore it — the user's intent is honored and recorded in events
//   - schedules a follow-up recompute via NotifyImmediate so the schedule
//     view reflects the new mode (e.g., Off→Schedule needs to recompute)
func (s *Scheduler) OnModeChanged(oldMode, newMode string) {
	s.mu.Lock()
	s.lastStartSent = time.Time{}
	s.lastStopSent = time.Time{}
	s.mu.Unlock()

	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "modeChange",
		Input:  map[string]any{"old": oldMode, "new": newMode},
		Result: map[string]any{"clearedCooldowns": true},
	})

	s.controlChargingInternal(newMode, true)
	s.NotifyImmediate()
}

// UpdateLivePower updates the actual power draw from MeterValues.
func (s *Scheduler) UpdateLivePower(powerW float64) {
	s.mu.Lock()
	old := s.livePower
	s.livePower = powerW
	s.mu.Unlock()

	// Recompute if power changed >10%
	if old > 0 && math.Abs(powerW-old)/old > 0.10 {
		s.log.Info("live power changed significantly", "old", old, "new", powerW)
		s.recompute()
	}
}

// UpdateSoC updates the estimated SoC after a charging session.
func (s *Scheduler) UpdateSoC(energyKWh float64) {
	capacity := s.store.GetFloat("scheduler.battery_capacity", 0)
	if capacity <= 0 || energyKWh <= 0 {
		return
	}
	currentSoC := s.store.GetFloat("scheduler.current_soc", 0)
	newSoC := math.Min(100, currentSoC+(energyKWh/capacity*100))
	_ = s.store.Set("scheduler.current_soc", fmt.Sprintf("%.1f", newSoC))
	s.log.Info("SoC updated after session",
		"energy", energyKWh,
		"oldSoC", currentSoC,
		"newSoC", newSoC,
	)
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(),
		Source:    "scheduler",
		Action:    "updateSoC",
		Input:     map[string]any{"energyKWh": energyKWh, "capacity": capacity},
		Result:    map[string]any{"oldSoC": currentSoC, "newSoC": newSoC},
	})
}

func (s *Scheduler) loop(ctx context.Context) {
	recomputeTicker := time.NewTicker(15 * time.Minute)
	controlTicker := time.NewTicker(30 * time.Second)
	defer recomputeTicker.Stop()
	defer controlTicker.Stop()

	isScheduleMode := func() bool {
		return s.store.GetDefault("charger.mode", "schedule") == "schedule" &&
			s.store.GetBool("scheduler.enabled", true)
	}

	// Initial recompute on startup — but only when no schedule was restored
	// from persistence. Skipping the initial recompute is the whole point of
	// persistence: the pre-crash schedule survives, and we don't accidentally
	// re-pick "current hour" just because windowStart=now puts it in range.
	// The regular 15-min ticker still drives recomputes, so live tariff
	// changes still take effect — just not as a knee-jerk on boot.
	s.mu.RLock()
	hasRestored := s.current != nil
	s.mu.RUnlock()
	if isScheduleMode() && !hasRestored {
		s.recompute()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-recomputeTicker.C:
			if isScheduleMode() {
				s.recompute()
			}
		case <-controlTicker.C:
			s.controlCharging()
		case <-s.notifyCh:
			if isScheduleMode() {
				s.recompute()
			}
			s.controlCharging()
		}
	}
}

// isPluggedIn returns true if the connector status indicates a car is present.
func isPluggedIn(status string) bool {
	switch status {
	case "Preparing", "Charging", "SuspendedEV", "SuspendedEVSE":
		return true
	}
	return false
}

// isVehiclePlugBlocked returns (blocked, skipReason). It blocks only when the
// plug check is enabled, Renault reports unplugged, AND that reading is fresh
// enough to trust over the OCPP physical plug signal. Stale readings are
// ignored because the Renault API can lag behind reality by up to one poll
// interval — without this check, a "0" cached from before the user plugged
// in would block a legitimate charge until the next Renault poll.
func (s *Scheduler) isVehiclePlugBlocked() (blocked bool, skipReason string) {
	if !s.store.GetBool("vehicle.require_plug_check", false) {
		return false, ""
	}
	if s.store.GetDefault("vehicle.plug_status", "") != "0" {
		return false, ""
	}
	ts := s.store.GetDefault("vehicle.battery_timestamp", "")
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false, "no_timestamp"
	}
	pollInterval := s.store.GetInt("vehicle.poll_interval", 15)
	maxAge := 2 * time.Duration(pollInterval) * time.Minute
	if maxAge < 30*time.Minute {
		maxAge = 30 * time.Minute
	}
	if timeNow().Sub(parsed) > maxAge {
		return false, "stale_data"
	}
	return true, ""
}

// recordPlugCheckSkip emits a system event when a fresh start is about to fire
// despite Renault reporting unplugged. Gated on lastStartSent so it only fires
// once per cooldown window — otherwise the 30s controlTicker would spam events.
func (s *Scheduler) recordPlugCheckSkip(reason, mode, cpID string) {
	if reason == "" {
		return
	}
	if !s.lastStartSent.IsZero() && time.Since(s.lastStartSent) < remoteCmdCooldown {
		return
	}
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "controlCharging", Level: "info",
		Input:  map[string]any{"mode": mode, "cpID": cpID, "plugStatus": "0"},
		Result: map[string]any{"plugCheckSkipped": reason, "action": "start"},
	})
}

// remoteCmdCooldown is the minimum interval between sending the same type of
// remote command. The myenergi cloud proxy relays OCPP commands slowly;
// re-sending during this window queues duplicates.
const remoteCmdCooldown = 5 * time.Minute

// profileCmdCooldown is the minimum interval between SetChargingProfile calls.
// Prevents burst flooding when multiple Notify() calls cascade.
const profileCmdCooldown = 30 * time.Second

// controlCharging starts or stops transactions based on the current mode and schedule.
func (s *Scheduler) controlCharging() {
	mode := s.store.GetDefault("charger.mode", "schedule")
	s.controlChargingInternal(mode, false)
}

// controlChargingInternal is the workhorse of controlCharging, with two
// extras to support explicit user-driven mode switches via OnModeChanged:
//   - modeOverride: lets the caller pin the mode for this invocation rather
//     than re-reading the store mid-flight (a rapid double-toggle race).
//   - forced: bypasses the SuspendedEV early-return so an explicit user
//     click on Force still attempts a RemoteStartTransaction (Zappi may
//     ignore it, but the user's intent is honored and recorded).
func (s *Scheduler) controlChargingInternal(mode string, forced bool) {
	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	if cpID == "" {
		return
	}

	status, txnID, connected := s.charger.ConnectorStatus(cpID, 1)
	if !connected {
		return
	}

	pluggedIn := isPluggedIn(status)
	hasTransaction := txnID > 0

	// Clear cooldowns when state confirms the command completed,
	// or when the car is unplugged (previous start is moot).
	if hasTransaction || !pluggedIn {
		s.lastStartSent = time.Time{}
	}
	if !hasTransaction {
		s.lastStopSent = time.Time{}
	}

	// Car declared it's done charging (battery full or EV decision). The OCPP
	// handler eagerly finalizes the DB session on this transition. Don't send
	// RemoteStopTransaction — Zappi ignores it in this state — and don't start
	// a new transaction; the connector still reports the open OCPP transaction
	// until unplug. Logged at Debug to avoid filling the journal every 30s.
	// User-forced mode changes bypass this gate so an explicit click still
	// gets a RemoteStartTransaction attempt.
	if status == "SuspendedEV" && !forced {
		s.log.Debug("car suspended charging, waiting for unplug", "cpID", cpID, "txnID", txnID, "mode", mode)
		return
	}

	suspendedEVOverride := status == "SuspendedEV" && forced

	switch mode {
	case "off":
		if hasTransaction {
			s.sendStop(cpID, txnID, "off")
		}
	case "force":
		if pluggedIn && !hasTransaction {
			blocked, skipReason := s.isVehiclePlugBlocked()
			if blocked {
				s.log.Warn("charge blocked by vehicle plug check", "cpID", cpID, "mode", "force")
				_ = s.store.RecordSystemEvent(store.SystemEvent{
					Timestamp: time.Now(), Source: "scheduler", Action: "controlCharging", Level: "warn",
					Input:  map[string]any{"mode": "force", "cpID": cpID},
					Result: map[string]any{"blocked": "vehicle_plug_check", "plugStatus": "0"},
				})
				return
			}
			s.recordPlugCheckSkip(skipReason, "force", cpID)
			s.sendStartForced(cpID, "force", suspendedEVOverride)
		}
	default: // "schedule"
		if !pluggedIn {
			return
		}
		s.mu.RLock()
		sched := s.current
		s.mu.RUnlock()

		shouldCharge := IsChargeTime(sched)

		if shouldCharge && !hasTransaction {
			blocked, skipReason := s.isVehiclePlugBlocked()
			if blocked {
				s.log.Warn("charge blocked by vehicle plug check", "cpID", cpID, "mode", "schedule")
				_ = s.store.RecordSystemEvent(store.SystemEvent{
					Timestamp: time.Now(), Source: "scheduler", Action: "controlCharging", Level: "warn",
					Input:  map[string]any{"mode": "schedule", "cpID": cpID, "shouldCharge": true},
					Result: map[string]any{"blocked": "vehicle_plug_check", "plugStatus": "0"},
				})
				return
			}
			s.recordPlugCheckSkip(skipReason, "schedule", cpID)
			s.sendStartForced(cpID, "schedule", suspendedEVOverride)
		} else if !shouldCharge && hasTransaction {
			s.sendStop(cpID, txnID, "schedule")
		}
	}
}

func (s *Scheduler) sendStart(cpID, mode string) {
	s.sendStartForced(cpID, mode, false)
}

func (s *Scheduler) sendStartForced(cpID, mode string, suspendedEVOverride bool) {
	if !s.lastStartSent.IsZero() && time.Since(s.lastStartSent) < remoteCmdCooldown {
		s.log.Debug("remote start already sent, waiting for cloud proxy",
			"sentAgo", time.Since(s.lastStartSent).Round(time.Second))
		return
	}
	idTag := s.store.GetDefault("zappi.id_tag", "grosz")
	s.log.Info("starting charge", "cpID", cpID, "mode", mode, "suspendedEVOverride", suspendedEVOverride)
	s.lastStartSent = time.Now()
	err := s.charger.RemoteStartTransaction(cpID, idTag, 1)
	if err != nil {
		s.log.Warn("failed to start charge", "err", err)
	} else {
		_ = s.store.InsertChartMarker("start", time.Now())
	}
	result := resultFromErr(err, "start")
	if suspendedEVOverride {
		result["suspendedEVOverride"] = true
	}
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "controlCharging",
		Level:  levelFromErr(err),
		Input:  map[string]any{"mode": mode, "cpID": cpID},
		Result: result,
	})
}

func (s *Scheduler) sendStop(cpID string, txnID int, mode string) {
	if !s.lastStopSent.IsZero() && time.Since(s.lastStopSent) < remoteCmdCooldown {
		s.log.Debug("remote stop already sent, waiting for cloud proxy",
			"sentAgo", time.Since(s.lastStopSent).Round(time.Second))
		return
	}
	s.log.Info("stopping charge", "cpID", cpID, "mode", mode, "txn", txnID)
	s.lastStopSent = time.Now()
	err := s.charger.RemoteStopTransaction(cpID, txnID)
	if err != nil {
		s.log.Warn("failed to stop charge", "err", err)
	}
	// "stop" chart marker is emitted by the OCPP handler when the resulting
	// StopTransaction arrives, so the timestamp matches the actual stop time
	// (and we don't double-mark when the EV stops on its own).
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "controlCharging",
		Level:  levelFromErr(err),
		Input:  map[string]any{"mode": mode, "cpID": cpID, "txnID": txnID},
		Result: resultFromErr(err, "stop"),
	})
}

func (s *Scheduler) recompute() {
	s.mu.RLock()
	cfg := s.config
	power := s.livePower
	prev := s.current
	s.mu.RUnlock()

	// A slot whose period contains now is "in progress" — recompute may not
	// drop or replace it. Only user actions (CancelSlot, ClearSchedule) can.
	// Without this guard, the very SoC rise caused by ongoing charging can
	// trip the SoC-skip / target-reached branches and stop the active session.
	inProgress := activeSlot(prev)

	s.detectMissedPeriods(prev)

	if cfg == nil {
		cfg = s.configFromStore()
	}
	if cfg == nil {
		return
	}

	// Always re-read live SoC from store — a cached config from SetConfig
	// holds user intent (deadline, target, limits) but its CurrentSoC is
	// frozen at SetConfig time and would otherwise mask Renault updates.
	fresh := *cfg
	fresh.CurrentSoC = s.store.GetFloat("scheduler.current_soc", 0)
	if fresh.BatteryCapacity > 0 && fresh.TargetSoC > 0 {
		if needed := fresh.TargetSoC - fresh.CurrentSoC; needed > 0 {
			fresh.TargetEnergy = needed / 100 * fresh.BatteryCapacity
		} else {
			fresh.TargetEnergy = 0
		}
	}
	cfg = &fresh

	rates, err := s.tariff.Rates()
	if err != nil {
		s.log.Debug("no rates for scheduling", "err", err)
		rates = nil
	}

	now := timeNow()
	_ = s.store.PurgeOldOverrides(now.Add(-7 * 24 * time.Hour))
	overrides, _ := s.store.LoadOverrides(now)
	forces, _ := splitOverrides(overrides)
	forcePeriods := buildForcePeriods(forces, rates)
	forceEnergy := totalForceEnergy(forcePeriods)

	// Force-only schedules survive SoC/target/no-rate skip paths — the user
	// has explicitly committed to those windows.
	hasForces := len(forcePeriods) > 0

	if len(rates) == 0 && !hasForces {
		return
	}

	chargePower := cfg.MaxPower
	if power > 0 {
		chargePower = power
	}

	autoEligible := true
	skipReason := ""
	skipKey := ""
	var skipParams map[string]string

	// SoC > skip threshold: auto-selection only allows negative-price rates.
	// Force overrides bypass this filter.
	autoRates := rates
	if cfg.BatteryCapacity > 0 && cfg.SkipAboveSoC > 0 && cfg.CurrentSoC >= cfg.SkipAboveSoC {
		var negativeRates []tariff.Rate
		for _, r := range rates {
			if r.Price < 0 {
				negativeRates = append(negativeRates, r)
			}
		}
		if len(negativeRates) == 0 {
			autoEligible = false
			skipReason = fmt.Sprintf("SoC %.0f%% >= skip threshold %.0f%%", cfg.CurrentSoC, cfg.SkipAboveSoC)
			skipKey = "soc_above_threshold"
			skipParams = map[string]string{"soc": fmt.Sprintf("%.0f", cfg.CurrentSoC), "threshold": fmt.Sprintf("%.0f", cfg.SkipAboveSoC)}
		} else {
			autoRates = negativeRates
			s.log.Info("SoC above skip threshold, using negative-price rates only",
				"currentSoC", cfg.CurrentSoC, "skipAboveSoC", cfg.SkipAboveSoC, "negativeRates", len(negativeRates))
		}
	}

	autoTarget := fresh.TargetEnergy - forceEnergy
	if autoTarget < 0 {
		autoTarget = 0
	}
	if autoTarget == 0 {
		autoEligible = false
		if skipReason == "" {
			if forceEnergy > 0 {
				skipReason = "force overrides cover target"
				skipKey = "covered_by_overrides"
			} else {
				skipReason = "no energy needed"
				skipKey = "no_energy_needed"
			}
		}
	}

	var autoSched *Schedule
	if autoEligible && len(autoRates) > 0 {
		fragRates := fragmentRates(autoRates, overrides)
		autoCfg := *cfg
		autoCfg.TargetEnergy = autoTarget
		autoSched = ComputeSchedule(fragRates, autoCfg, chargePower)
		if autoSched == nil || len(autoSched.Slots) == 0 {
			autoSched = nil
			skipReason = "no affordable rates available"
			skipKey = "no_affordable_rates"
			if cfg.MaxPrice > 0 {
				skipReason = fmt.Sprintf("no rates below %.2f PLN/kWh", cfg.MaxPrice)
				skipKey = "no_rates_below_price"
				skipParams = map[string]string{"price": fmt.Sprintf("%.2f", cfg.MaxPrice)}
			}
		}
	}

	firstDeadline := nextDeadlineAfter(now, cfg.Deadline.Hour(), cfg.Deadline.Minute(), now.Location())
	sched := mergeForcesIntoSchedule(autoSched, forcePeriods, firstDeadline)

	if sched == nil {
		if inProgress != nil {
			key := skipKey
			if key == "" {
				key = "no_affordable_rates"
			}
			s.preserveActiveSlot(inProgress, key,
				map[string]any{"targetEnergy": fresh.TargetEnergy, "maxPrice": cfg.MaxPrice, "ratesCount": len(rates)})
			return
		}
		s.log.Info("skipping charge", "reason", skipReason)
		s.mu.Lock()
		s.current = nil
		s.skipReason = skipReason
		s.skipReasonKey = skipKey
		s.skipParams = skipParams
		s.mu.Unlock()
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "recompute",
			Input:  map[string]any{"targetEnergy": fresh.TargetEnergy, "maxPrice": cfg.MaxPrice, "ratesCount": len(rates), "currentSoC": cfg.CurrentSoC, "forces": len(forcePeriods)},
			Result: map[string]any{"skipped": true, "reason": skipReason},
		})
		s.saveCurrent()
		s.fireOnRecompute()
		return
	}

	// Preserve slot cancellations from previous schedule
	if prev != nil {
		cancelled := make(map[string]bool)
		for _, slot := range prev.Slots {
			if slot.Cancelled {
				cancelled[slot.Date] = true
			}
		}
		for i, slot := range sched.Slots {
			if cancelled[slot.Date] {
				sched.Slots[i].Cancelled = true
			}
		}
		var cost, energy float64
		for _, slot := range sched.Slots {
			if !slot.Cancelled {
				cost += slot.Cost
				energy += slot.Energy
			}
		}
		sched.Cost = math.Round(cost*100) / 100
		sched.Energy = math.Round(energy*100) / 100
	}

	// Preserve the in-progress slot through recompute. The active period
	// (containing now) is never shortened — but it may be extended into
	// adjacent recomputed hours so a running session continues seamlessly
	// when the deficit can be made up by charging a bit longer.
	if inProgress != nil {
		sched = mergeActiveSlotPreservingActive(sched, cloneSlot(*inProgress))
	}

	s.mu.Lock()
	s.current = sched
	// Auto-skipped but force-only schedule: keep skip reason for UI context.
	if autoEligible || !hasForces {
		s.skipReason = ""
		s.skipReasonKey = ""
		s.skipParams = nil
	} else {
		s.skipReason = skipReason
		s.skipReasonKey = skipKey
		s.skipParams = skipParams
	}
	s.mu.Unlock()

	activeSlots := 0
	for _, slot := range sched.Slots {
		if !slot.Cancelled {
			activeSlots++
		}
	}
	s.log.Info("schedule computed",
		"slots", len(sched.Slots),
		"activeSlots", activeSlots,
		"cost", sched.Cost,
		"energy", sched.Energy,
		"forces", len(forcePeriods),
	)
	result := map[string]any{
		"slots": len(sched.Slots), "activeSlots": activeSlots,
		"cost": sched.Cost, "energy": sched.Energy,
		"forces":   len(forcePeriods),
		"blocks":   len(overrides) - len(forces),
		"preAlloc": math.Round(forceEnergy*100) / 100,
	}
	if inProgress != nil {
		result["mergedActiveSlot"] = inProgress.Date
	}
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "recompute",
		Input: map[string]any{
			"targetEnergy": fresh.TargetEnergy, "maxPower": chargePower,
			"ratesCount": len(rates), "currentSoC": cfg.CurrentSoC,
			"forces": len(forcePeriods),
		},
		Result: result,
	})

	s.applyProfile(sched)
	s.saveCurrent()
	s.fireOnRecompute()
}

func (s *Scheduler) configFromStore() *Config {
	deadlineStr := s.store.GetDefault("scheduler.deadline_time", "07:00")
	deadline := nextDeadline(deadlineStr)

	cfg := &Config{
		Deadline:        deadline,
		MaxPower:        s.store.GetFloat("charger.max_power", 11000),
		MinPower:        s.store.GetFloat("charger.min_power", 1380),
		BatteryCapacity: s.store.GetFloat("scheduler.battery_capacity", 0),
		TargetSoC:       s.store.GetFloat("scheduler.target_soc", 0),
		SkipAboveSoC:    s.store.GetFloat("scheduler.skip_above_soc", 0),
		MinSoC:          s.store.GetFloat("scheduler.min_soc", 0),
		CurrentSoC:      s.store.GetFloat("scheduler.current_soc", 0),
		MaxPrice:        s.store.GetFloat("scheduler.max_price", 0),
		ChargeHeadroom:  s.store.GetFloat("scheduler.charge_headroom", 3),
	}

	// Calculate target energy from SoC if configured
	if cfg.BatteryCapacity > 0 && cfg.TargetSoC > 0 {
		neededSoC := cfg.TargetSoC - cfg.CurrentSoC
		if neededSoC > 0 {
			cfg.TargetEnergy = neededSoC / 100 * cfg.BatteryCapacity
		}
	} else {
		// Fall back to fixed target energy
		cfg.TargetEnergy = s.store.GetFloat("scheduler.target_energy", 0)
	}

	if cfg.TargetEnergy <= 0 && cfg.TargetSoC <= 0 {
		return nil
	}

	return cfg
}

func (s *Scheduler) applyProfile(sched *Schedule) {
	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	if cpID == "" {
		s.log.Debug("no charge_box_id, skipping profile apply")
		return
	}

	hash := ScheduleHash(sched)
	if hash == "" {
		return
	}

	profile := BuildProfile(sched, s.store.GetFloat("charger.max_power", 11000))
	if profile == nil {
		return
	}

	s.mu.Lock()
	unchanged := hash == s.lastProfileHash
	cooldown := !s.lastProfileSent.IsZero() && time.Since(s.lastProfileSent) < profileCmdCooldown
	s.mu.Unlock()

	if unchanged {
		s.log.Debug("profile unchanged, skipping OCPP update")
		return
	}
	if cooldown {
		s.log.Debug("profile cooldown active, skipping OCPP update")
		return
	}

	if err := s.charger.SetChargingProfile(cpID, 1, profile); err != nil {
		s.log.Warn("failed to set charging profile", "err", err)
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "scheduler", Action: "applyProfile", Level: "warn",
			Input:  map[string]any{"cpID": cpID},
			Result: map[string]any{"error": err.Error()},
		})
		return
	}

	s.mu.Lock()
	s.lastProfileHash = hash
	s.lastProfileSent = time.Now()
	s.mu.Unlock()

	s.log.Info("charging profile applied", "hash", hash)
	_ = s.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "scheduler", Action: "applyProfile",
		Input:  map[string]any{"cpID": cpID, "periods": len(sched.ActivePeriods())},
		Result: map[string]any{"applied": true, "hash": hash},
	})
}

// ComputeSchedule finds the cheapest hours for each day's window,
// creating one slot per day up to the available rate data.
func ComputeSchedule(rates []tariff.Rate, cfg Config, chargePowerW float64) *Schedule {
	if cfg.TargetEnergy < 0 {
		cfg.TargetEnergy = 0
	}

	// If SoC is below minimum threshold, ignore MaxPrice to ensure charging
	if cfg.MinSoC > 0 && cfg.CurrentSoC < cfg.MinSoC {
		cfg.MaxPrice = 0
	}

	now := time.Now()
	deadlineHour := cfg.Deadline.Hour()
	deadlineMinute := cfg.Deadline.Minute()

	// Find first deadline
	firstDeadline := time.Date(now.Year(), now.Month(), now.Day(),
		deadlineHour, deadlineMinute, 0, 0, now.Location())
	if firstDeadline.Before(now) {
		firstDeadline = firstDeadline.Add(24 * time.Hour)
	}

	var slots []ScheduleSlot
	windowStart := now
	deadline := firstDeadline
	remainingEnergy := cfg.TargetEnergy

	// Iterate up to maxCycles deadline windows (today + tomorrow). Today may
	// produce no eligible rates (e.g. all hours above MaxPrice, or the user
	// plugged in close to deadline) — in that case we still try tomorrow so
	// the deficit has a fallback window before giving up.
	const maxCycles = 2
	for cycle := 0; cycle < maxCycles; cycle++ {
		var windowRates []tariff.Rate
		for _, r := range rates {
			if r.End.After(windowStart) && r.Start.Before(deadline) && r.Price != 0 {
				if cfg.MaxPrice > 0 && r.Price > cfg.MaxPrice {
					continue
				}
				windowRates = append(windowRates, r)
			}
		}

		if len(windowRates) > 0 {
			energyForSlot := remainingEnergy
			if energyForSlot < 0 {
				energyForSlot = 0
			}
			slot := computeSlot(windowRates, energyForSlot, chargePowerW, cfg.ChargeHeadroom, deadline)
			if slot != nil {
				slots = append(slots, *slot)
				remainingEnergy -= slot.Energy
			}
		}

		if remainingEnergy <= 0 {
			break
		}

		windowStart = deadline
		deadline = deadline.Add(24 * time.Hour)
	}

	if len(slots) == 0 {
		return nil
	}

	var totalCost, totalEnergy float64
	for _, slot := range slots {
		totalCost += slot.Cost
		totalEnergy += slot.Energy
	}

	return &Schedule{
		Slots:    slots,
		Cost:     math.Round(totalCost*100) / 100,
		Energy:   math.Round(totalEnergy*100) / 100,
		Deadline: slots[len(slots)-1].Deadline,
	}
}

// computeSlot picks the cheapest hours within a single day window.
func computeSlot(rates []tariff.Rate, targetEnergy, chargePowerW, chargeHeadroom float64, deadline time.Time) *ScheduleSlot {
	energyPerHour := chargePowerW / 1000
	if energyPerHour <= 0 {
		return nil
	}

	// Add headroom to account for charging speed oscillations around nominal power.
	headroomFactor := 1.0 + chargeHeadroom/100
	hoursNeeded := int(math.Ceil(targetEnergy * headroomFactor / energyPerHour))

	// Sort by price ascending
	sorted := make([]tariff.Rate, len(rates))
	copy(sorted, rates)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Price < sorted[j].Price
	})

	// Select the cheapest hoursNeeded rates. Negative-price rates sort first,
	// so they are naturally prioritized — maximizing earnings from negative prices
	// while respecting battery capacity.
	selected := make(map[time.Time]bool)
	totalSelected := 0
	for _, r := range sorted {
		if totalSelected >= hoursNeeded {
			break
		}
		selected[r.Start] = true
		totalSelected++
	}

	// Build periods chronologically
	var periods []SchedulePeriod
	var totalCost, totalEnergy float64
	for _, r := range rates {
		if selected[r.Start] {
			periods = append(periods, SchedulePeriod{
				Start: r.Start,
				End:   r.End,
				Power: chargePowerW,
				Price: r.Price,
			})
			hrs := r.End.Sub(r.Start).Hours()
			totalEnergy += energyPerHour * hrs
			totalCost += r.Price * energyPerHour * hrs
		}
	}

	if len(periods) == 0 {
		return nil
	}

	return &ScheduleSlot{
		Date:     deadline.Format("2006-01-02"),
		Deadline: deadline,
		Periods:  periods,
		Cost:     math.Round(totalCost*100) / 100,
		Energy:   math.Round(totalEnergy*100) / 100,
	}
}

func levelFromErr(err error) string {
	if err != nil {
		return "warn"
	}
	return "info"
}

func resultFromErr(err error, action string) map[string]any {
	if err != nil {
		return map[string]any{"action": action, "error": err.Error()}
	}
	return map[string]any{"action": action}
}

// nextDeadline returns the next occurrence of HH:MM in local time.
func nextDeadline(hhmm string) time.Time {
	now := time.Now()
	var h, m int
	_, _ = time.Parse("15:04", hhmm) // validate format
	_, _ = fmt.Sscanf(hhmm, "%d:%d", &h, &m)

	deadline := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if deadline.Before(now) {
		deadline = deadline.Add(24 * time.Hour)
	}
	return deadline
}
