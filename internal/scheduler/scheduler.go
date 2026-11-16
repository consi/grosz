package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/consi/grosz/internal/events"
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
	// Reason explains why the planner picked this period (see Reason* consts).
	Reason string `json:"reason,omitempty"`
	// FillSoC > 0 means a cheap-fill period: the car limit is raised to it.
	FillSoC float64 `json:"fillSoc,omitempty"`
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
	Deadline time.Time      `json:"deadline"` // latest departure covered

	Mode    string    `json:"mode,omitempty"`    // Mode* constant
	Target  float64   `json:"target,omitempty"`  // % the plan charges to
	ReadyBy time.Time `json:"readyBy,omitempty"` // when the target must be reached
	NeedKWh float64   `json:"needKwh,omitempty"` // wall kWh still needed
	Sig     string    `json:"sig,omitempty"`     // planner input signature (stickiness)
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

// Scheduler orchestrates the cheapest-window charging algorithm.
type Scheduler struct {
	charger Charger
	tariff  tariff.Provider
	store   *store.Store
	events  *events.Bound
	log     *slog.Logger

	mu            sync.RWMutex
	current       *Schedule
	planMode      string            // last planner mode (also when nothing is planned)
	skipReason    string            // if non-empty, schedule was skipped
	skipReasonKey string            // i18n key for skip reason
	skipParams    map[string]string // parameters for the skip reason template

	notifyCh chan struct{} // trigger immediate recompute + control
	cancel   context.CancelFunc

	debounceMu    sync.Mutex
	debounceTimer *time.Timer

	// User-driven mode changes are debounced 5s server-side so a rapid
	// Schedule → Off → Schedule misclick doesn't churn the charger. Only
	// the last requested mode applies. pendingMode is in-memory only: a
	// restart drops any in-flight pending change.
	modeDebounceMu    sync.Mutex
	modeDebounceTimer *time.Timer
	pendingMode       string
	pendingModeAt     time.Time

	// Cooldown tracking for cloud-proxied chargers (e.g. Zappi via myenergi cloud).
	// Commands are relayed slowly; duplicates queue up and cause issues.
	lastStartSent   time.Time
	lastStopSent    time.Time
	lastProfileHash string    // planHash of the profile the charger last accepted ("" = none)
	lastProfileSent time.Time // when last SetChargingProfile was sent

	// Profile write budget / backoff (see profilectl.go).
	profileMu             sync.Mutex
	profileFailures       int
	profileBackoffUntil   time.Time
	profileDeferredLogged string

	// Pause bookkeeping: txn currently paused between plan blocks via the
	// profile (0 = not paused), and since when the charger has reported
	// SuspendedEVSE inside a charge period (watchdog).
	pausedTxnID        int
	evseSuspendedSince time.Time
	lastFinishingStart time.Time

	carLimiter        CarLimiter
	lastCarLimitWrite time.Time
	lastPlugged       bool
	holdLogged        int // txn for which "kept open, charger idle" was logged

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
		events:   events.For(events.SourceScheduler, st),
		log:      log.With("component", "scheduler"),
		notifyCh: make(chan struct{}, 1),
		cancel:   cancel,
	}
	s.loadPersistedSchedule()
	// The charger keeps its profile across our restarts; remembering what it
	// holds avoids a pointless (budgeted) rewrite after every deploy.
	s.lastProfileHash = st.GetDefault(settingProfileHash, "")
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
		s.events.Error(events.ActionLoadSchedule, nil, err)
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
		s.events.Info(events.ActionLoadSchedule, nil,
			map[string]any{"restored": false, "droppedPastSlots": dropped},
		)
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
	s.events.Info(events.ActionLoadSchedule, nil,
		map[string]any{
			"restored":         true,
			"slots":            len(sched.Slots),
			"activePeriods":    activePeriods,
			"droppedPastSlots": dropped,
		},
	)
}

// saveCurrent persists s.current (or empty string if nil) to the settings
// store. Called after every recompute and after every user mutation that
// changes the schedule. Caller must NOT hold s.mu: this method takes its
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
		s.events.Error(events.ActionSaveSchedule, nil, err)
		return
	}
	if err := s.store.Set(persistedScheduleKey, string(data)); err != nil {
		s.log.Warn("failed to persist schedule", "err", err)
		s.events.Error(events.ActionSaveSchedule, nil, err)
	}
}

// Stop shuts down the scheduler.
func (s *Scheduler) Stop() {
	s.debounceMu.Lock()
	if s.debounceTimer != nil {
		s.debounceTimer.Stop()
	}
	s.debounceMu.Unlock()
	s.modeDebounceMu.Lock()
	if s.modeDebounceTimer != nil {
		s.modeDebounceTimer.Stop()
	}
	s.modeDebounceMu.Unlock()
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
	_ = s.store.Set(settingProfileHash, "")
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

// PlanMode returns the planner's last mode (Mode* constants).
func (s *Scheduler) PlanMode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.planMode
}

// SkipReason returns why the schedule was skipped, or "" if active.
func (s *Scheduler) SkipReason() (string, string, map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.skipReason, s.skipReasonKey, s.skipParams
}

// ReloadConfig drops the cached Config so the next recompute reads fresh
// values from the store, then triggers an immediate recompute. Call this
// whenever scheduler-relevant settings change.
func (s *Scheduler) ReloadConfig() {
	s.events.Info(events.ActionReloadConfig, nil,
		map[string]any{"invalidated": true},
	)
	s.NotifyImmediate()
}

// ClearSchedule removes the current schedule and clears the charging profile.
func (s *Scheduler) ClearSchedule() {
	s.mu.Lock()
	s.current = nil
	s.skipReason = ""
	s.skipReasonKey = ""
	s.skipParams = nil
	hadProfile := s.lastProfileHash != ""
	s.lastProfileHash = ""
	s.mu.Unlock()

	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	clearedProfile := false
	if cpID != "" && hadProfile {
		// Always drop the plan profile, also mid-transaction: a leftover
		// zero-power gap would otherwise throttle a Force / manual session.
		_, _, connected := s.charger.ConnectorStatus(cpID, 1)
		if connected {
			s.recordProfileWrite(timeNow())
			if err := s.charger.ClearChargingProfile(cpID); err == nil {
				clearedProfile = true
			}
		}
	}
	_ = s.store.Set(settingProfileHash, "")
	s.log.Info("schedule cleared", "clearedProfile", clearedProfile)
	s.events.Info(events.ActionClearSchedule,
		map[string]any{"cpID": cpID},
		map[string]any{"cleared": true, "clearedProfile": clearedProfile},
	)
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

	s.events.Info(events.ActionCancelSlot,
		map[string]any{"date": date},
		map[string]any{"found": found},
	)
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

	s.events.Info(events.ActionRestoreSlot,
		map[string]any{"date": date},
		map[string]any{"found": found},
	)
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

// detectMissedPeriods scans the previous schedule for charging periods that
// elapsed since the last recompute and emits a `missed_period` system event
// for any that had no overlapping charging session: i.e. the car wasn't
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
		return // first recompute, no prior window to scan
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
		s.events.Warn(events.ActionMissedPeriod,
			map[string]any{
				"start":      p.Start.Format(time.RFC3339),
				"end":        p.End.Format(time.RFC3339),
				"plannedKWh": plannedKWh,
				"source":     p.Source,
			},
			map[string]any{"reason": "no_overlapping_session"},
		)
	}
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

// HasActiveTransaction reports whether a session must be protected from schedule cancellation.
func (s *Scheduler) HasActiveTransaction() bool {
	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	if cpID == "" {
		return false
	}
	_, txnID, connected := s.charger.ConnectorStatus(cpID, 1)
	return connected && txnID > 0
}

// NotifyImmediate triggers an immediate recompute and charge control check.
// Use for physical events (connector status changes, boot) that should not wait.
func (s *Scheduler) NotifyImmediate() {
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

// modeChangeDebounce is the window that absorbs UI mode-button misclicks
// (e.g., Schedule → Off → Schedule fired in under a second). Only the last
// requested mode in this window is applied. UI pulses the chosen button
// once per second for the duration so the user sees the countdown.
const modeChangeDebounce = 3 * time.Second

// RequestModeChange registers a user-driven mode change to be applied after
// modeChangeDebounce of silence. Subsequent calls within the window replace
// the pending mode and reset the timer.
//
// Callers MUST NOT also call store.Set("charger.mode", ...) or OnModeChanged
// directly: that path is the synchronous internal one. RequestModeChange
// is the UI-facing entry point and owns both the store update and the
// OnModeChanged invocation (in applyPendingMode).
func (s *Scheduler) RequestModeChange(newMode string) {
	s.modeDebounceMu.Lock()
	current := s.store.GetDefault("charger.mode", "schedule")
	s.pendingMode = newMode
	s.pendingModeAt = time.Now().Add(modeChangeDebounce)
	if s.modeDebounceTimer != nil {
		s.modeDebounceTimer.Stop()
	}
	s.modeDebounceTimer = time.AfterFunc(modeChangeDebounce, s.applyPendingMode)
	applyAt := s.pendingModeAt
	s.modeDebounceMu.Unlock()

	s.events.Info(events.ActionModeChangeRequested,
		map[string]any{"requested": newMode, "current": current},
		map[string]any{"applyAt": applyAt.Format(time.RFC3339), "debounceSeconds": int(modeChangeDebounce / time.Second)},
	)

	// Push the pending state out to any SSE listener so the UI's pulse
	// indicator picks up the timer immediately (without waiting for a
	// status poll). MUST be called outside modeDebounceMu: the broadcast
	// path calls PendingModeChange which re-acquires this mutex.
	s.fireOnRecompute()
}

// PendingModeChange returns the pending mode and apply timestamp, or
// ("", time.Time{}) when nothing is pending.
func (s *Scheduler) PendingModeChange() (string, time.Time) {
	s.modeDebounceMu.Lock()
	defer s.modeDebounceMu.Unlock()
	return s.pendingMode, s.pendingModeAt
}

// applyPendingMode is the timer callback fired modeChangeDebounce after the
// last RequestModeChange. Persists the new mode, clears any auto schedule
// when transitioning to off/force, and calls OnModeChanged.
func (s *Scheduler) applyPendingMode() {
	s.modeDebounceMu.Lock()
	pending := s.pendingMode
	s.pendingMode = ""
	s.pendingModeAt = time.Time{}
	s.modeDebounceMu.Unlock()

	if pending == "" {
		return
	}

	oldMode := s.store.GetDefault("charger.mode", "schedule")
	_ = s.store.Set("charger.mode", pending)
	if pending == "off" || pending == "force" {
		s.ClearSchedule()
	}
	s.OnModeChanged(oldMode, pending)

	// SSE push of the cleared pending state. OnModeChanged → NotifyImmediate
	// only triggers fireOnRecompute via the loop's recompute, which is
	// skipped for non-schedule modes, so without this call, the frontend
	// would never learn the pending change has applied and the pulse
	// indicator would never stop.
	s.fireOnRecompute()
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
//     ignore it: the user's intent is honored and recorded in events
//   - schedules a follow-up recompute via NotifyImmediate so the schedule
//     view reflects the new mode (e.g., Off→Schedule needs to recompute)
func (s *Scheduler) OnModeChanged(oldMode, newMode string) {
	s.mu.Lock()
	s.lastStartSent = time.Time{}
	s.lastStopSent = time.Time{}
	s.mu.Unlock()

	s.events.Info(events.ActionModeChange,
		map[string]any{"old": oldMode, "new": newMode},
		map[string]any{"clearedCooldowns": true},
	)

	s.controlChargingInternal(newMode, true)
	s.NotifyImmediate()
}

// UpdateSoC updates the estimated SoC after a charging session.
func (s *Scheduler) UpdateSoC(energyKWh float64) {
	capacity := s.store.GetFloat("scheduler.battery_capacity", 0)
	if capacity <= 0 || energyKWh <= 0 {
		return
	}
	// A fresh reading from the car already includes this session's energy;
	// adding it again double-counts (seen live: 54% + bogus 24.8 kWh → 100%).
	if ts, err := time.Parse(time.RFC3339, s.store.GetDefault("vehicle.battery_timestamp", "")); err == nil &&
		timeNow().Sub(ts) < 2*time.Hour {
		s.log.Debug("skip SoC estimate: vehicle SoC is fresh", "energy", energyKWh)
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
	s.events.Info(events.ActionUpdateSoC,
		map[string]any{"energyKWh": energyKWh, "capacity": capacity},
		map[string]any{"oldSoC": currentSoC, "newSoC": newSoC},
	)
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

	// Initial recompute on startup, but only when no schedule was restored
	// from persistence. Skipping the initial recompute is the whole point of
	// persistence: the pre-crash schedule survives, and we don't accidentally
	// re-pick "current hour" just because windowStart=now puts it in range.
	// The regular 15-min ticker still drives recomputes, so live tariff
	// changes still take effect, just not as a knee-jerk on boot.
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
// interval, without this check, a "0" cached from before the user plugged
// in would block a legitimate charge until the next Renault poll.
//
// vehicle.plug_status / battery_timestamp are refreshed by the poller's 15-min
// battery tier plus a ~1-min debounced poll on every real plug/unplug socket
// transition, so a genuine plug-in clears the block within ~1 min and maxAge
// (2×poll_interval, min 30 min) still bounds a stale unplugged reading.
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
// once per cooldown window, otherwise the 30s controlTicker would spam events.
func (s *Scheduler) recordPlugCheckSkip(reason, mode, cpID string) {
	if reason == "" {
		return
	}
	if !s.lastStartSent.IsZero() && time.Since(s.lastStartSent) < remoteCmdCooldown {
		return
	}
	s.events.Info(events.ActionControlCharging,
		map[string]any{"mode": mode, "cpID": cpID, "plugStatus": "0"},
		map[string]any{"plugCheckSkipped": reason, "action": "start"},
	)
}

// remoteCmdCooldown is the minimum interval between sending the same type of
// remote command. The myenergi cloud proxy relays OCPP commands slowly;
// re-sending during this window queues duplicates.
const remoteCmdCooldown = 5 * time.Minute

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

	// Track plug state to restore the car's limit after a cheap fill or a
	// trip once the car leaves.
	plugNow := pluggedIn || status == "Finishing"
	justUnplugged := s.lastPlugged && !plugNow
	s.lastPlugged = plugNow
	if mode == "schedule" {
		s.mu.RLock()
		cur := s.current
		s.mu.RUnlock()
		s.manageCarLimit(cur, plugNow, justUnplugged)
	}

	// Car declared it's done charging (battery full or EV decision). The OCPP
	// handler eagerly finalizes the DB session on this transition. Don't send
	// RemoteStopTransaction (Zappi ignores it in this state) and don't start
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
				s.events.Warn(events.ActionControlCharging,
					map[string]any{"mode": "force", "cpID": cpID},
					map[string]any{"blocked": "vehicle_plug_check", "plugStatus": "0"},
				)
				return
			}
			s.recordPlugCheckSkip(skipReason, "force", cpID)
			s.sendStartForced(cpID, "force", suspendedEVOverride)
		}
	default: // "schedule"
		s.mu.RLock()
		sched := s.current
		s.mu.RUnlock()

		// Keep the charger's profile in sync with the plan (or the hold
		// profile); a no-op when already accepted. This tick is also the
		// retry path after a rejected write.
		s.applyProfile(sched)

		// After a stop, Zappi parks in Finishing until re-plug. If the plan
		// (e.g. a new manual window) wants charging, try one RemoteStart per
		// 30 min: the charger may reject it, which is harmless and logged.
		if status == "Finishing" && !hasTransaction {
			if IsChargeTime(sched) && time.Since(s.lastFinishingStart) > 30*time.Minute {
				s.lastFinishingStart = time.Now()
				s.lastStartSent = time.Time{}
				s.sendStartForced(cpID, "schedule", false)
			}
			return
		}
		if !pluggedIn {
			s.setPausedTxn(0)
			return
		}

		shouldCharge := IsChargeTime(sched)

		if shouldCharge && !hasTransaction {
			blocked, skipReason := s.isVehiclePlugBlocked()
			if blocked {
				s.log.Warn("charge blocked by vehicle plug check", "cpID", cpID, "mode", "schedule")
				s.events.Warn(events.ActionControlCharging,
					map[string]any{"mode": "schedule", "cpID": cpID, "shouldCharge": true},
					map[string]any{"blocked": "vehicle_plug_check", "plugStatus": "0"},
				)
				return
			}
			s.recordPlugCheckSkip(skipReason, "schedule", cpID)
			s.sendStartForced(cpID, "schedule", suspendedEVOverride)
		} else if !shouldCharge && hasTransaction {
			s.idleTransaction(cpID, txnID, status, sched)
		} else if shouldCharge && hasTransaction {
			if s.pausedTxn() == txnID {
				s.setPausedTxn(0)
				s.events.Info(events.ActionControlCharging,
					map[string]any{"mode": "schedule", "cpID": cpID, "txnID": txnID, "status": status},
					map[string]any{"action": "resume"},
				)
			}
			s.watchSuspendedEVSE(cpID, txnID, status)
		}
	}
}

// idleTransaction handles an open transaction outside any planned period.
// Stopping makes Zappi park in Finishing until the cable is re-plugged, so a
// plugged-in car is only stopped when it is done (target reached and nothing
// more planned) or, as a safety net, when the charger is drawing power while
// it does not hold our profile. Otherwise the transaction stays open and the
// profile's 0 W period (plan gap or hold) keeps it paused.
func (s *Scheduler) idleTransaction(cpID string, txnID int, status string, sched *Schedule) {
	target := s.normalTarget()
	if sched != nil && sched.Target > 0 {
		target = sched.Target
	} else if car := s.store.GetFloat("vehicle.soc_target", 0); car > 0 && s.store.GetDefault(settingFillRaisedFrom, "") == "" {
		target = car
	}
	soc := s.store.GetFloat("scheduler.current_soc", 0)
	done := soc >= target-1 && !hasFutureCharge(sched)

	switch {
	case done:
		s.sendStopReason(cpID, txnID, "schedule", "target_reached")
	case s.profileInSync(sched):
		if s.pausedTxn() != txnID {
			s.setPausedTxn(txnID)
			why := "plan_gap"
			if !hasFutureCharge(sched) {
				why = "hold"
			}
			s.log.Info("transaction kept open, paused by profile", "txn", txnID, "why", why)
			s.events.Info(events.ActionControlCharging,
				map[string]any{"mode": "schedule", "cpID": cpID, "txnID": txnID, "status": status, "soc": soc},
				map[string]any{"action": "pause", "why": why},
			)
		}
	case status == "Charging":
		s.sendStopReason(cpID, txnID, "schedule", "profile_not_in_sync")
	default:
		if s.holdLogged != txnID {
			s.holdLogged = txnID
			s.events.Info(events.ActionControlCharging,
				map[string]any{"mode": "schedule", "cpID": cpID, "txnID": txnID, "status": status},
				map[string]any{"action": "keep_open", "why": "charger_idle_profile_pending"},
			)
		}
	}
}

func (s *Scheduler) pausedTxn() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pausedTxnID
}

func (s *Scheduler) setPausedTxn(id int) {
	s.mu.Lock()
	s.pausedTxnID = id
	s.mu.Unlock()
}

// watchSuspendedEVSE flags a charge period where the charger keeps the car
// paused (stale / unaccepted profile). After 10 minutes it forces a profile
// rewrite (still within budget) and records why.
func (s *Scheduler) watchSuspendedEVSE(cpID string, txnID int, status string) {
	if status != "SuspendedEVSE" {
		s.evseSuspendedSince = time.Time{}
		return
	}
	now := timeNow()
	if s.evseSuspendedSince.IsZero() {
		s.evseSuspendedSince = now
		return
	}
	if now.Sub(s.evseSuspendedSince) < 10*time.Minute {
		return
	}
	s.evseSuspendedSince = now // re-arm
	s.log.Warn("charger paused during a planned charge period, re-sending profile", "txn", txnID)
	s.events.Warn(events.ActionControlCharging,
		map[string]any{"mode": "schedule", "cpID": cpID, "txnID": txnID, "status": status},
		map[string]any{"action": "resync_profile", "reason": "suspended_evse_in_charge_period"},
	)
	s.mu.Lock()
	s.lastProfileHash = ""
	s.mu.Unlock()
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
	emitControlCharging(s, err,
		map[string]any{"mode": mode, "cpID": cpID},
		result,
	)
}

func (s *Scheduler) sendStop(cpID string, txnID int, mode string) {
	s.sendStopReason(cpID, txnID, mode, "")
}

func (s *Scheduler) sendStopReason(cpID string, txnID int, mode, reason string) {
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
	result := resultFromErr(err, "stop")
	if reason != "" {
		result["reason"] = reason
	}
	emitControlCharging(s, err,
		map[string]any{"mode": mode, "cpID": cpID, "txnID": txnID},
		result,
	)
}

// emitControlCharging chooses Info or Warn for the controlCharging action
// based on whether the underlying RemoteStart/Stop call returned an error.
func emitControlCharging(s *Scheduler, err error, input, result map[string]any) {
	if err != nil {
		s.events.Warn(events.ActionControlCharging, input, result)
		return
	}
	s.events.Info(events.ActionControlCharging, input, result)
}

func (s *Scheduler) recompute() {
	s.mu.RLock()
	prev := s.current
	s.mu.RUnlock()

	s.detectMissedPeriods(prev)

	in := s.planInput(prev)
	sched, mode := Plan(in)

	// Keep slot (day) cancellations: drop that day's automatic periods, but
	// a manual window the user added afterwards still applies.
	if sched != nil && prev != nil {
		cancelled := map[string]bool{}
		for _, slot := range prev.Slots {
			if slot.Cancelled {
				cancelled[slot.Date] = true
			}
		}
		for i, slot := range sched.Slots {
			if !cancelled[slot.Date] {
				continue
			}
			var manual []SchedulePeriod
			for _, p := range slot.Periods {
				if p.IsForce() {
					manual = append(manual, p)
				}
			}
			if len(manual) > 0 {
				sched.Slots[i].Periods = manual
				recalcSlotTotals(&sched.Slots[i])
			} else {
				sched.Slots[i].Cancelled = true
			}
		}
		recalcScheduleTotals(sched)
	}

	key, text := skipText(mode)
	s.mu.Lock()
	s.current = sched
	s.planMode = mode
	if sched == nil || mode == ModePaused {
		s.skipReason, s.skipReasonKey, s.skipParams = text, key, nil
	} else {
		s.skipReason, s.skipReasonKey, s.skipParams = "", "", nil
	}
	s.mu.Unlock()

	result := map[string]any{"mode": mode}
	if sched != nil {
		result["cost"] = sched.Cost
		result["energy"] = sched.Energy
		result["periods"] = len(sched.ActivePeriods())
		result["target"] = sched.Target
		result["needKwh"] = sched.NeedKWh
		if prev != nil && prev.Sig == sched.Sig {
			result["sticky"] = true
		}
	}
	s.log.Info("plan computed", "mode", mode, "soc", in.SoC, "result", result)
	s.events.Info(events.ActionRecompute,
		map[string]any{
			"soc": in.SoC, "carLimit": in.CarLimit, "normalTarget": in.NormalTarget,
			"plugged": in.PluggedNow, "rates": len(in.Rates), "forces": len(in.Forces),
			"blocks": len(in.Blocks), "dailyKwh": math.Round(in.DailyKWh*10) / 10,
			"plugPattern": len(in.PlugPattern) > 0, "paused": in.Paused,
		},
		result,
	)

	s.applyProfile(sched)
	s.saveCurrent()
	s.fireOnRecompute()
}

// skipText maps a planner mode with an empty plan to the UI's skip-reason
// key and an English fallback text.
func skipText(mode string) (key, text string) {
	switch mode {
	case ModeFull:
		return "target_reached", "target reached"
	case ModePaused:
		return "user_cancelled", "automatic scheduling paused"
	case ModeNoRates:
		return "no_rates", "no price data"
	case ModeWait:
		return "waiting_cheaper", "waiting for cheaper hours"
	}
	return "no_affordable_rates", "no suitable hours"
}

// recalcScheduleTotals recomputes Cost/Energy/Deadline over non-cancelled slots.
func recalcScheduleTotals(sched *Schedule) {
	var cost, energy float64
	for _, slot := range sched.Slots {
		if slot.Cancelled {
			continue
		}
		cost += slot.Cost
		energy += slot.Energy
		if slot.Deadline.After(sched.Deadline) {
			sched.Deadline = slot.Deadline
		}
	}
	sched.Cost = math.Round(cost*100) / 100
	sched.Energy = math.Round(energy*100) / 100
}

// planInput gathers the planner inputs from settings, live state and history.
func (s *Scheduler) planInput(prev *Schedule) PlanInput {
	st := s.store
	now := timeNow()
	loc := time.Local
	if tz := st.GetDefault("display.timezone", ""); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}

	rates, err := s.tariff.Rates()
	if err != nil {
		s.log.Debug("no rates for scheduling", "err", err)
	}
	_ = st.PurgeOldOverrides(now.Add(-7 * 24 * time.Hour))
	overrides, _ := st.LoadOverrides(now)
	forces, blocks := splitOverrides(overrides)
	power := st.GetFloat("charger.max_power", 11000)
	for i := range forces {
		forces[i].PowerW = power
	}

	dh, dm := parseHHMM(st.GetDefault("scheduler.departure_time", "07:00"), 7, 0)

	plugged := false
	if cpID := st.GetDefault("zappi.charge_box_id", ""); cpID != "" {
		status, _, connected := s.charger.ConnectorStatus(cpID, 1)
		plugged = connected && (isPluggedIn(status) || status == "Finishing")
	}

	pattern, days, err := st.PlugPattern(now.Add(-28*24*time.Hour), now, loc)
	if err != nil || days < 7 {
		pattern = nil // too little history: don't restrict hours
	}

	var tripReady time.Time
	if v := st.GetDefault(settingTripReadyBy, ""); v != "" {
		tripReady, _ = time.Parse(time.RFC3339, v)
	}

	return PlanInput{
		Now:            now,
		Loc:            loc,
		Rates:          rates,
		SoC:            st.GetFloat("scheduler.current_soc", 0),
		BatteryKWh:     st.GetFloat("scheduler.battery_capacity", 52),
		Efficiency:     chargeEfficiency,
		PowerW:         power,
		NormalTarget:   s.normalTarget(),
		CarLimit:       st.GetFloat("vehicle.soc_target", 0),
		FillRaised:     st.GetDefault(settingFillRaisedFrom, "") != "",
		TripReadyBy:    tripReady,
		Floor:          st.GetFloat("scheduler.min_soc", 15),
		Safe:           st.GetFloat("scheduler.safe_soc", 50),
		DepartureH:     dh,
		DepartureM:     dm,
		MaxPrice:       st.GetFloat("scheduler.max_price", 0),
		CheapFillPrice: st.GetFloat("scheduler.cheap_fill_price", 0.45),
		CheapFillSoC:   st.GetFloat("scheduler.cheap_fill_soc", 100),
		PluggedNow:     plugged,
		PlugPattern:    pattern,
		DailyKWh:       s.dailyKWh(now),
		Forces:         forces,
		Blocks:         blocks,
		Paused:         st.GetBool("scheduler.paused", false),
		Prev:           prev,
	}
}

// chargeEfficiency is battery kWh gained per wall kWh (measured ~0.91 from
// SoC rise vs metered energy, Apr-Nov 2026).
const chargeEfficiency = 0.9

// dailyKWh estimates battery energy used per day: average km/day over the
// last 14 days times the 30-day kWh/100km (wall based, converted). Falls
// back to 6 kWh/day when there is not enough data.
func (s *Scheduler) dailyKWh(now time.Time) float64 {
	km, err := s.store.OdometerDelta(now.Add(-14*24*time.Hour), now)
	if err != nil || km <= 0 {
		return 6
	}
	energy, dist, ok, _ := s.store.ConsumptionWindow(now)
	per100 := 17.0 // wall kWh/100km fallback
	if ok && dist > 0 {
		per100 = energy / dist * 100
	}
	return km / 14 * per100 / 100 * chargeEfficiency
}

// parseHHMM parses "HH:MM", returning the defaults on error.
func parseHHMM(v string, defH, defM int) (int, int) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return defH, defM
	}
	return t.Hour(), t.Minute()
}

// EffectiveTargetSoC returns the charge limit (target state of charge, %) to aim
// for: the live value read from the car (vehicle.soc_target) when available,
// otherwise the local scheduler.target_soc setting, which acts as a fallback
// when the Renault API doesn't provide one (older models, transient failures).
func EffectiveTargetSoC(st *store.Store) float64 {
	if v := st.GetFloat("vehicle.soc_target", 0); v > 0 {
		return v
	}
	return st.GetFloat("scheduler.target_soc", 0)
}

func resultFromErr(err error, action string) map[string]any {
	if err != nil {
		return map[string]any{"action": action, "error": err.Error()}
	}
	return map[string]any{"action": action}
}
