package scheduler

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/consi/grosz/internal/events"
)

// Zappi limits memory writes to 24 per 60 minutes and rejects any further
// SetChargingProfile with "InternalError - Not enough slots" until the
// window clears (myenergi support article "Can I use OCPP charging profiles
// with zappi?"). History on the live system showed that once ~4 writes had
// been made in an hour almost every further write was rejected, and because
// failures were retried on every recompute the charger stayed locked out
// for days. So: one plan profile, written only when the future charge-block
// structure changes, under a strict persisted budget with backoff.
const (
	profileWriteBudget   = 8                // max SetCharging/ClearCharging writes per window
	profileWriteWindow   = 60 * time.Minute //
	profileBackoffBase   = 10 * time.Minute // first backoff after a rejection
	profileBackoffMax    = 60 * time.Minute
	maxProfileBlocks     = 10               // ≤ 2*10+1 schedule periods (Zappi has 84 slots)
	profileTrailingGrace = 30 * time.Minute // keep limit 0 after the last block until the hold profile replaces it

	settingProfileHash   = "scheduler.profile_hash"   // block hash of the profile the charger accepted
	settingProfileWrites = "scheduler.profile_writes" // unix seconds of recent writes, comma separated
)

// chargeBlock is a contiguous run of active periods with the same power.
type chargeBlock struct {
	Start, End time.Time
	Power      float64
}

// futureBlocks consolidates the schedule's active periods that end after now
// into contiguous same-power blocks, ordered by start.
func futureBlocks(sched *Schedule, now time.Time) []chargeBlock {
	var out []chargeBlock
	for _, p := range sched.ActivePeriods() {
		if p.Power <= 0 || !p.End.After(now) {
			continue
		}
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

// planHash identifies what the charger must do from now on. A block that has
// already started hashes without its start, so the hash (and therefore the
// need to write) does not change as time passes or as the in-progress hour's
// bookkeeping shifts, only when a future boundary or power changes.
func planHash(sched *Schedule) string {
	if sched == nil {
		return ""
	}
	now := timeNow()
	blocks := futureBlocks(sched, now)
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) > maxProfileBlocks {
		blocks = blocks[:maxProfileBlocks]
	}
	var b strings.Builder
	for _, bl := range blocks {
		start := bl.Start.Unix()
		if !bl.Start.After(now) {
			start = 0
		}
		fmt.Fprintf(&b, "%d-%d@%.0f;", start, bl.End.Unix(), bl.Power)
	}
	h := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", h[:8])
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

// BuildPlanProfile converts the schedule's future charge blocks into one
// absolute TxDefaultProfile: block power during blocks, 0 W in the gaps and
// before the first block. While a transaction is open, the zero periods make
// the charger pause (SuspendedEVSE) instead of ending the transaction, which
// Zappi would otherwise leave in Finishing until the cable is re-plugged.
func BuildPlanProfile(sched *Schedule) *types.ChargingProfile {
	now := timeNow()
	blocks := futureBlocks(sched, now)
	if len(blocks) == 0 {
		return nil
	}
	if len(blocks) > maxProfileBlocks {
		blocks = blocks[:maxProfileBlocks]
	}

	anchor := blocks[0].Start
	if hour := now.Truncate(time.Hour); hour.Before(anchor) {
		anchor = hour
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

	return &types.ChargingProfile{
		ChargingProfileId:      1,
		StackLevel:             0,
		ChargingProfilePurpose: types.ChargingProfilePurposeTxDefaultProfile,
		ChargingProfileKind:    types.ChargingProfileKindAbsolute,
		ValidFrom:              types.NewDateTime(anchor),
		ValidTo:                types.NewDateTime(cursor.Add(profileTrailingGrace)),
		ChargingSchedule: &types.ChargingSchedule{
			ChargingRateUnit:       types.ChargingRateUnitWatts,
			ChargingSchedulePeriod: periods,
			StartSchedule:          types.NewDateTime(anchor),
		},
	}
}

// holdHash identifies the daily "hold at 0 W" profile.
func holdHash(now time.Time) string { return "hold:" + now.Format("2006-01-02") }

// desiredHash is the profile the charger should hold for sched: the plan,
// the hold profile when nothing is planned but a car is plugged in, or ""
// (no profile) otherwise.
func (s *Scheduler) desiredHash(sched *Schedule) string {
	if h := planHash(sched); h != "" {
		return h
	}
	if s.carPlugged() {
		return holdHash(timeNow())
	}
	return ""
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

// profileInSync reports whether the charger holds the profile for sched
// (the plan, or the hold profile when nothing is planned).
func (s *Scheduler) profileInSync(sched *Schedule) bool {
	h := s.desiredHash(sched)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return h != "" && h == s.lastProfileHash
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

// profileWriteAllowed checks budget and backoff. reason is "" when allowed.
func (s *Scheduler) profileWriteAllowed(now time.Time) (bool, string) {
	s.mu.RLock()
	backoff := s.profileBackoffUntil
	s.mu.RUnlock()
	if now.Before(backoff) {
		return false, "backoff"
	}
	if len(s.recentProfileWrites(now)) >= profileWriteBudget {
		return false, "budget"
	}
	return true, ""
}

// applyProfile makes the charger hold the plan profile for sched. Cheap and
// idempotent: it only writes when the plan's block hash differs from the
// last accepted one, and never beyond the write budget. Called after every
// recompute and from the 30 s control tick (which acts as the retry path).
func (s *Scheduler) applyProfile(sched *Schedule) {
	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	if cpID == "" {
		return
	}
	// Serialize writers: recompute and the control tick can race, and two
	// concurrent writes of the same plan would burn the budget twice.
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	hash := s.desiredHash(sched)
	hold := strings.HasPrefix(hash, "hold:")
	s.mu.RLock()
	last := s.lastProfileHash
	s.mu.RUnlock()

	if hash == last {
		return
	}
	// Not connected (e.g. right after a restart): wait for the charger
	// instead of burning budget and backing off on "no client" errors.
	if _, _, connected := s.charger.ConnectorStatus(cpID, 1); !connected {
		return
	}
	now := timeNow()
	if ok, why := s.profileWriteAllowed(now); !ok {
		s.noteProfileDeferred(cpID, why, hash)
		return
	}

	if hash == "" {
		// Nothing left to charge: drop the old plan so it can't throttle a
		// later manual / force session.
		s.recordProfileWrite(now)
		if err := s.charger.ClearChargingProfile(cpID); err != nil {
			s.profileWriteFailed(cpID, err)
			return
		}
		s.profileWriteOK("")
		s.events.Info(events.ActionApplyProfile, map[string]any{"cpID": cpID},
			map[string]any{"cleared": true})
		return
	}

	profile := BuildPlanProfile(sched)
	if hold {
		profile = buildHoldProfile(now)
	}
	if profile == nil {
		return
	}
	s.recordProfileWrite(now)
	if err := s.charger.SetChargingProfile(cpID, 1, profile); err != nil {
		s.profileWriteFailed(cpID, err)
		return
	}
	s.profileWriteOK(hash)

	blocks := futureBlocks(sched, now)
	s.log.Info("charging profile applied", "hash", hash, "blocks", len(blocks),
		"periods", len(profile.ChargingSchedule.ChargingSchedulePeriod))
	s.events.Info(events.ActionApplyProfile,
		map[string]any{"cpID": cpID, "blocks": len(blocks),
			"periods":        len(profile.ChargingSchedule.ChargingSchedulePeriod),
			"writesLastHour": len(s.recentProfileWrites(now))},
		map[string]any{"applied": true, "hash": hash},
	)
}

func (s *Scheduler) profileWriteOK(hash string) {
	s.mu.Lock()
	s.lastProfileHash = hash
	s.lastProfileSent = timeNow()
	s.profileFailures = 0
	s.profileBackoffUntil = time.Time{}
	s.profileDeferredLogged = ""
	s.mu.Unlock()
	_ = s.store.Set(settingProfileHash, hash)
}

func (s *Scheduler) profileWriteFailed(cpID string, err error) {
	s.mu.Lock()
	s.profileFailures++
	backoff := profileBackoffBase << (s.profileFailures - 1)
	if backoff > profileBackoffMax || backoff <= 0 {
		backoff = profileBackoffMax
	}
	s.profileBackoffUntil = timeNow().Add(backoff)
	failures := s.profileFailures
	s.mu.Unlock()

	s.log.Warn("failed to set charging profile", "err", err, "backoff", backoff, "failures", failures)
	s.events.Warn(events.ActionApplyProfile, map[string]any{"cpID": cpID},
		map[string]any{"error": err.Error(), "backoffMinutes": int(backoff.Minutes()), "failures": failures})
}

// noteProfileDeferred records (once per pending hash) that a needed write is
// being held back, so the log explains why the charger may be out of sync.
func (s *Scheduler) noteProfileDeferred(cpID, why, hash string) {
	s.mu.Lock()
	already := s.profileDeferredLogged == hash
	s.profileDeferredLogged = hash
	until := s.profileBackoffUntil
	s.mu.Unlock()
	if already {
		return
	}
	s.log.Info("profile write deferred", "reason", why, "until", until)
	s.events.Info(events.ActionApplyProfile, map[string]any{"cpID": cpID},
		map[string]any{"deferred": why, "writesLastHour": len(s.recentProfileWrites(timeNow()))})
}

// NoteExternalProfileWrite records a profile write made outside the
// scheduler (OCPP debug console): it counts against the Zappi write budget
// and the scheduler re-applies its own profile on the next tick.
func (s *Scheduler) NoteExternalProfileWrite() {
	s.recordProfileWrite(timeNow())
	s.mu.Lock()
	s.lastProfileHash = ""
	s.mu.Unlock()
	_ = s.store.Set(settingProfileHash, "")
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
		InSync:         s.profileInSync(cur) || (s.desiredHash(cur) == "" && s.lastProfileHashSnapshot() == ""),
		WritesLastHour: len(s.recentProfileWrites(now)),
		WriteBudget:    profileWriteBudget,
		Paused:         paused,
	}
	if now.Before(backoff) {
		st.BackoffUntil = &backoff
	}
	return st
}

func (s *Scheduler) lastProfileHashSnapshot() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastProfileHash
}
