package scheduler

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/consi/grosz/internal/events"
)

// Zappi profile writes are scarce. myenergi documents 24 memory writes per
// 60 minutes, but on the live charger (2026-11-16) only ~1 write per hour got
// through, and refused attempts appear to extend the lockout. So the charger
// is written to lazily:
//
//   - grosz remembers what the charger holds (installedProfile, persisted)
//     and compares it with the plan. A write happens only when the two would
//     make the charger behave differently, and only profileWriteLead before
//     that moment, so replans in between collapse into one write (or none).
//   - One profile serves as both plan and hold: block power during blocks,
//     0 W in gaps and for profileValidity after the last block, so a
//     plugged-in car stays paused without a separate daily hold write.
//   - Nothing is written while no car is plugged in, and the profile is not
//     cleared on unplug; it is cleared only when a force session needs power
//     the profile would deny.
//   - At most profileWriteBudget writes per rolling window, spaced at least
//     profileWriteSpacing apart; a refusal backs off profileRefusedBackoff.
//     Budget and backoff are persisted, so restarts and deploys can't reset
//     them.
const (
	profileWriteBudget      = 2
	profileWriteWindow      = 60 * time.Minute
	profileWriteSpacing     = 20 * time.Minute
	profileRefusedBackoff   = 60 * time.Minute
	profileTransportBackoff = 2 * time.Minute  // not delivered (no client): retry soon, nothing counted
	profileWriteLead        = 30 * time.Minute // write this long before the charger would act wrongly
	profileValidity         = 48 * time.Hour   // 0 W hold after the last block
	maxProfileBlocks        = 10               // ≤ 2*10+2 schedule periods (Zappi has 84 slots)

	settingProfileInstalled = "scheduler.profile_installed"     // JSON installedProfile
	settingProfileWrites    = "scheduler.profile_writes"        // unix seconds of recent attempts, comma separated
	settingProfileBackoff   = "scheduler.profile_backoff_until" // unix seconds
	settingProfileFailures  = "scheduler.profile_failures"
	settingProfileHashOld   = "scheduler.profile_hash" // pre-lazy-write key, dropped on start
)

// unlimited is the limit of a charger without an active profile.
const unlimited = -1.0

// chargeBlock is a contiguous run of active periods with the same power.
type chargeBlock struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Power float64   `json:"power"`
}

// futureBlocks consolidates the schedule's active periods that end after now
// into contiguous same-power blocks, ordered by start.
func futureBlocks(sched *Schedule, now time.Time) []chargeBlock {
	var periods []SchedulePeriod
	for _, p := range sched.ActivePeriods() {
		if p.Power > 0 && p.End.After(now) {
			periods = append(periods, p)
		}
	}
	sort.SliceStable(periods, func(i, j int) bool { return periods[i].Start.Before(periods[j].Start) })
	var out []chargeBlock
	for _, p := range periods {
		if n := len(out); n > 0 && !p.Start.After(out[n-1].End) && p.Power == out[n-1].Power {
			if p.End.After(out[n-1].End) {
				out[n-1].End = p.End
			}
			continue
		}
		out = append(out, chargeBlock{Start: p.Start, End: p.End, Power: p.Power})
	}
	return out
}

// profileBlocks is what the charger profile can encode: the first
// maxProfileBlocks future blocks.
func profileBlocks(sched *Schedule, now time.Time) []chargeBlock {
	b := futureBlocks(sched, now)
	if len(b) > maxProfileBlocks {
		b = b[:maxProfileBlocks]
	}
	return b
}

// blocksLimitAt is the plan's limit at t: block power inside a block, else 0.
func blocksLimitAt(blocks []chargeBlock, t time.Time) float64 {
	for _, b := range blocks {
		if !t.Before(b.Start) && t.Before(b.End) {
			return b.Power
		}
	}
	return 0
}

// hasFutureCharge reports whether the schedule has any active charging period
// that starts after now.
func hasFutureCharge(sched *Schedule) bool {
	now := timeNow()
	for _, p := range sched.ActivePeriods() {
		if p.Power > 0 && p.Start.After(now) {
			return true
		}
	}
	return false
}

// installedProfile is grosz's record of what the charger holds.
type installedProfile struct {
	// State: "plan" (our TxDefaultProfile), "none" (cleared), "" (unknown:
	// never written, charger rebooted, or written from the OCPP console).
	State  string        `json:"state"`
	From   time.Time     `json:"from,omitempty"`
	To     time.Time     `json:"to,omitempty"`
	Blocks []chargeBlock `json:"blocks,omitempty"`
}

// limitAt is the power the installed profile allows at t.
func (p installedProfile) limitAt(t time.Time) float64 {
	if p.State != "plan" || t.Before(p.From) || !t.Before(p.To) {
		return unlimited
	}
	return blocksLimitAt(p.Blocks, t)
}

// divergence returns the first moment from now on where the installed
// profile would make the charger behave differently from the plan blocks
// (block power in blocks, 0 W otherwise). ok=false: they agree throughout.
func divergence(ip installedProfile, blocks []chargeBlock, now time.Time) (time.Time, bool) {
	if ip.State == "" {
		return now, true
	}
	points := []time.Time{now}
	add := func(t time.Time) {
		if t.After(now) {
			points = append(points, t)
		}
	}
	for _, b := range blocks {
		add(b.Start)
		add(b.End)
	}
	for _, b := range ip.Blocks {
		add(b.Start)
		add(b.End)
	}
	if ip.State == "plan" {
		add(ip.From)
		add(ip.To)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Before(points[j]) })
	for _, t := range points {
		if ip.limitAt(t) != blocksLimitAt(blocks, t) {
			return t, true
		}
	}
	return time.Time{}, false
}

// buildProfile encodes blocks as one absolute TxDefaultProfile: block power
// during blocks, 0 W before, between and for profileValidity after them. The
// zero periods pause an open transaction (SuspendedEVSE) instead of ending
// it, which Zappi would otherwise leave in Finishing until re-plug. With no
// blocks it is a plain 0 W hold.
func buildProfile(blocks []chargeBlock, now time.Time) (*types.ChargingProfile, installedProfile) {
	anchor := now.Truncate(time.Hour)
	if len(blocks) > 0 && blocks[0].Start.Before(anchor) {
		anchor = blocks[0].Start
	}
	var periods []types.ChargingSchedulePeriod
	cursor := anchor
	for _, bl := range blocks {
		if bl.Start.After(cursor) {
			periods = append(periods, types.ChargingSchedulePeriod{StartPeriod: int(cursor.Sub(anchor).Seconds()), Limit: 0})
		}
		periods = append(periods, types.ChargingSchedulePeriod{StartPeriod: int(bl.Start.Sub(anchor).Seconds()), Limit: bl.Power})
		cursor = bl.End
	}
	periods = append(periods, types.ChargingSchedulePeriod{StartPeriod: int(cursor.Sub(anchor).Seconds()), Limit: 0})
	end := cursor
	if now.After(end) {
		end = now
	}
	validTo := end.Add(profileValidity)

	profile := &types.ChargingProfile{
		ChargingProfileId:      1,
		StackLevel:             0,
		ChargingProfilePurpose: types.ChargingProfilePurposeTxDefaultProfile,
		ChargingProfileKind:    types.ChargingProfileKindAbsolute,
		ValidFrom:              types.NewDateTime(anchor),
		ValidTo:                types.NewDateTime(validTo),
		ChargingSchedule: &types.ChargingSchedule{
			ChargingRateUnit:       types.ChargingRateUnitWatts,
			ChargingSchedulePeriod: periods,
			StartSchedule:          types.NewDateTime(anchor),
		},
	}
	rec := installedProfile{State: "plan", From: anchor, To: validTo, Blocks: append([]chargeBlock(nil), blocks...)}
	return profile, rec
}

// BuildPlanProfile returns the profile for sched's future blocks, or nil when
// nothing is planned.
func BuildPlanProfile(sched *Schedule) *types.ChargingProfile {
	now := timeNow()
	blocks := profileBlocks(sched, now)
	if len(blocks) == 0 {
		return nil
	}
	p, _ := buildProfile(blocks, now)
	return p
}

// --- installed-profile bookkeeping ---

func (s *Scheduler) installed() installedProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.installedProf
}

func (s *Scheduler) setInstalled(ip installedProfile) {
	s.mu.Lock()
	s.installedProf = ip
	s.mu.Unlock()
	if ip.State == "" {
		_ = s.store.Set(settingProfileInstalled, "")
		return
	}
	if b, err := json.Marshal(ip); err == nil {
		_ = s.store.Set(settingProfileInstalled, string(b))
	}
}

// loadProfileState restores the installed profile, write log and backoff.
func (s *Scheduler) loadProfileState() {
	var ip installedProfile
	if raw := s.store.GetDefault(settingProfileInstalled, ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &ip)
	}
	s.installedProf = ip
	if sec, err := strconv.ParseInt(s.store.GetDefault(settingProfileBackoff, ""), 10, 64); err == nil {
		s.profileBackoffUntil = time.Unix(sec, 0)
	}
	s.profileFailures = int(s.store.GetFloat(settingProfileFailures, 0))
	_ = s.store.Set(settingProfileHashOld, "")
}

// recentProfileWrites returns write timestamps inside the budget window,
// loaded from the store so restarts and deploys can't reset the budget.
func (s *Scheduler) recentProfileWrites(now time.Time) []time.Time {
	var out []time.Time
	for _, f := range strings.Split(s.store.GetDefault(settingProfileWrites, ""), ",") {
		sec, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil {
			continue
		}
		if t := time.Unix(sec, 0); now.Sub(t) < profileWriteWindow {
			out = append(out, t)
		}
	}
	return out
}

func (s *Scheduler) recordProfileWrite(now time.Time) {
	writes := append(s.recentProfileWrites(now), now)
	parts := make([]string, len(writes))
	for i, t := range writes {
		parts[i] = strconv.FormatInt(t.Unix(), 10)
	}
	_ = s.store.Set(settingProfileWrites, strings.Join(parts, ","))
}

// profileWriteAllowed checks backoff, budget and spacing. reason is "" when
// allowed.
func (s *Scheduler) profileWriteAllowed(now time.Time) (bool, string) {
	s.mu.RLock()
	backoff := s.profileBackoffUntil
	s.mu.RUnlock()
	if now.Before(backoff) {
		return false, "backoff"
	}
	writes := s.recentProfileWrites(now)
	if len(writes) >= profileWriteBudget {
		return false, "budget"
	}
	for _, w := range writes {
		if now.Sub(w) < profileWriteSpacing {
			return false, "spacing"
		}
	}
	return true, ""
}

// notDelivered reports errors where the request never reached the charger
// (no websocket client), so it costs no Zappi quota.
func notDelivered(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "send ")
}

// profileWriteDone does the accounting for one SetChargingProfile /
// ClearChargingProfile attempt. Every attempt that reached the charger counts
// against the budget, refused ones included.
func (s *Scheduler) profileWriteDone(cpID, op string, err error, now time.Time, rec installedProfile) {
	if notDelivered(err) {
		s.mu.Lock()
		s.profileBackoffUntil = now.Add(profileTransportBackoff)
		s.mu.Unlock()
		s.log.Warn("profile write not delivered", "op", op, "err", err)
		s.events.Warn(events.ActionApplyProfile, map[string]any{"cpID": cpID, "op": op},
			map[string]any{"error": err.Error(), "delivered": false})
		return
	}
	s.recordProfileWrite(now)
	writes := len(s.recentProfileWrites(now))
	if err != nil {
		s.mu.Lock()
		s.profileFailures++
		failures := s.profileFailures
		s.profileBackoffUntil = now.Add(profileRefusedBackoff)
		until := s.profileBackoffUntil
		s.mu.Unlock()
		_ = s.store.Set(settingProfileBackoff, strconv.FormatInt(until.Unix(), 10))
		_ = s.store.Set(settingProfileFailures, strconv.Itoa(failures))
		s.log.Warn("profile write refused", "op", op, "err", err, "failures", failures, "backoffUntil", until)
		s.events.Warn(events.ActionApplyProfile,
			map[string]any{"cpID": cpID, "op": op, "writesInWindow": writes},
			map[string]any{"error": err.Error(), "refused": true, "failures": failures,
				"backoffMinutes": int(profileRefusedBackoff.Minutes())})
		return
	}
	s.mu.Lock()
	s.profileFailures = 0
	s.profileBackoffUntil = time.Time{}
	s.profileDeferredLogged = time.Time{}
	s.mu.Unlock()
	_ = s.store.Set(settingProfileBackoff, "")
	_ = s.store.Set(settingProfileFailures, "0")
	s.setInstalled(rec)
}

// applyProfile keeps the charger's profile from contradicting the plan.
// Cheap and idempotent: it writes only when the installed profile would make
// the charger act differently within profileWriteLead, and never beyond the
// write budget. Called after every recompute and from the 30 s control tick.
func (s *Scheduler) applyProfile(sched *Schedule) {
	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	if cpID == "" {
		return
	}
	// Serialize writers: recompute and the control tick can race.
	s.profileMu.Lock()
	defer s.profileMu.Unlock()

	if _, _, connected := s.charger.ConnectorStatus(cpID, 1); !connected {
		return
	}
	// No car: nothing to steer. The profile stays as it is (no clear on
	// unplug); plugging in re-runs this check.
	if !s.carPlugged() {
		return
	}
	now := timeNow()
	blocks := profileBlocks(sched, now)
	ip := s.installed()
	at, diverges := divergence(ip, blocks, now)
	if !diverges {
		return
	}
	if lead := at.Sub(now); lead > profileWriteLead {
		s.noteProfileDeferred(cpID, "not_needed_yet", at, ip)
		return
	}
	if ok, why := s.profileWriteAllowed(now); !ok {
		s.noteProfileDeferred(cpID, why, at, ip)
		return
	}

	profile, rec := buildProfile(blocks, now)
	err := s.charger.SetChargingProfile(cpID, 1, profile)
	s.profileWriteDone(cpID, "set", err, now, rec)
	if err != nil {
		return
	}
	s.log.Info("charging profile applied", "blocks", len(blocks),
		"periods", len(profile.ChargingSchedule.ChargingSchedulePeriod), "divergedAt", at, "was", ip.State)
	s.events.Info(events.ActionApplyProfile,
		map[string]any{"cpID": cpID, "blocks": len(blocks),
			"periods":        len(profile.ChargingSchedule.ChargingSchedulePeriod),
			"writesInWindow": len(s.recentProfileWrites(now)),
			"divergedInMin":  int(at.Sub(now).Minutes()), "was": ip.State},
		map[string]any{"applied": true, "validTo": rec.To},
	)
}

// ensureUnthrottled clears the profile when it would deny a force session
// power right now (0 W period) or when what the charger holds is unknown.
func (s *Scheduler) ensureUnthrottled(cpID string) {
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	now := timeNow()
	ip := s.installed()
	if ip.State != "" && ip.limitAt(now) != 0 {
		return
	}
	if ok, why := s.profileWriteAllowed(now); !ok {
		s.noteProfileDeferred(cpID, why, now, ip)
		return
	}
	err := s.charger.ClearChargingProfile(cpID)
	s.profileWriteDone(cpID, "clear", err, now, installedProfile{State: "none"})
	if err == nil {
		s.log.Info("charging profile cleared for force session", "was", ip.State)
		s.events.Info(events.ActionApplyProfile, map[string]any{"cpID": cpID, "op": "clear", "was": ip.State},
			map[string]any{"cleared": true, "reason": "force_session"})
	}
}

// carPlugged reports whether the connector shows a connected car.
func (s *Scheduler) carPlugged() bool {
	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	if cpID == "" {
		return false
	}
	status, _, connected := s.charger.ConnectorStatus(cpID, 1)
	return connected && (isPluggedIn(status) || status == "Finishing")
}

// profilePausesNow reports whether the installed profile holds the charger at
// 0 W right now, i.e. an open transaction outside the plan is paused.
func (s *Scheduler) profilePausesNow() bool {
	return s.installed().limitAt(timeNow()) == 0
}

// profileInSync reports whether the installed profile agrees with the plan
// for at least the write lead time.
func (s *Scheduler) profileInSync(sched *Schedule) bool {
	now := timeNow()
	at, diverges := divergence(s.installed(), profileBlocks(sched, now), now)
	return !diverges || at.Sub(now) > profileWriteLead
}

// noteProfileDeferred records (once per divergence moment) that a write is
// being held back, so the log explains why the charger may differ from plan.
func (s *Scheduler) noteProfileDeferred(cpID, why string, at time.Time, ip installedProfile) {
	key := at
	if ip.State == "" {
		key = time.Time{} // unknown profile diverges "now" on every tick
	}
	s.mu.Lock()
	already := s.profileDeferredLogged.Equal(key) && s.profileDeferredWhy == why
	s.profileDeferredLogged = key
	s.profileDeferredWhy = why
	until := s.profileBackoffUntil
	s.mu.Unlock()
	if already {
		return
	}
	s.logProfileDeferred(cpID, why, at, ip, until)
}

func (s *Scheduler) logProfileDeferred(cpID, why string, at time.Time, ip installedProfile, until time.Time) {
	now := timeNow()
	s.log.Info("profile write deferred", "reason", why, "divergesAt", at, "backoffUntil", until)
	s.events.Info(events.ActionApplyProfile, map[string]any{"cpID": cpID, "installed": ip.State},
		map[string]any{"deferred": why, "divergesInMin": int(at.Sub(now).Minutes()),
			"writesInWindow": len(s.recentProfileWrites(now))})
}

// NoteExternalProfileWrite records a profile write made outside the
// scheduler (OCPP debug console): it counts against the Zappi write budget
// and what the charger holds becomes unknown.
func (s *Scheduler) NoteExternalProfileWrite() {
	s.recordProfileWrite(timeNow())
	s.setInstalled(installedProfile{})
}

// ResetProfileState marks the charger's profile as unknown, so the next
// applyProfile rewrites it. Called on BootNotification (charger rebooted).
func (s *Scheduler) ResetProfileState() {
	s.setInstalled(installedProfile{})
}

// ProfileStatus summarizes whether the charger holds the current plan.
type ProfileStatus struct {
	InSync         bool       `json:"inSync"`
	WritesLastHour int        `json:"writesLastHour"`
	WriteBudget    int        `json:"writeBudget"`
	BackoffUntil   *time.Time `json:"backoffUntil,omitempty"`
	Paused         bool       `json:"paused"` // transaction held open in a planned gap
}

// ProfileStatus reports profile sync / budget state for the API.
func (s *Scheduler) ProfileStatus() ProfileStatus {
	now := timeNow()
	s.mu.RLock()
	cur := s.current
	backoff := s.profileBackoffUntil
	paused := s.pausedTxnID != 0
	s.mu.RUnlock()
	st := ProfileStatus{
		InSync:         s.profileInSync(cur),
		WritesLastHour: len(s.recentProfileWrites(now)),
		WriteBudget:    profileWriteBudget,
		Paused:         paused,
	}
	if now.Before(backoff) {
		st.BackoffUntil = &backoff
	}
	return st
}
