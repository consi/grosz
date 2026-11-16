package scheduler

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/tariff"
)

// failingCharger wraps mockCharger and can fail SetChargingProfile on demand.
type failingCharger struct {
	mockCharger
	failSet bool
	setErr  error // default: a refusal as Zappi sends it
}

func (f *failingCharger) SetChargingProfile(cpID string, connectorID int, profile *types.ChargingProfile) error {
	if f.failSet {
		f.mu.Lock()
		f.profileAttempts++
		f.mu.Unlock()
		if f.setErr != nil {
			return f.setErr
		}
		return errors.New("ocpp message (1): InternalError - Not enough slots")
	}
	return f.mockCharger.SetChargingProfile(cpID, connectorID, profile)
}

// plannedAt is a plan with one block [start, end).
func plannedAt(start, end time.Time) *Schedule {
	return &Schedule{Slots: []ScheduleSlot{{
		Date:    start.Format("2006-01-02"),
		Periods: []SchedulePeriod{{Start: start, End: end, Power: 11000, Price: 0.30}},
	}}}
}

func pluggedScheduler(t *testing.T) (*Scheduler, *mockCharger) {
	t.Helper()
	mock := &mockCharger{connected: true, status: "Preparing"}
	s, _ := newTestScheduler(t, mock)
	return s, mock
}

func profileCount(m *mockCharger) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.profiles
}

// --- divergence ---

func TestDivergence(t *testing.T) {
	now := time.Date(2026, 11, 16, 21, 10, 0, 0, time.UTC)
	block := []chargeBlock{{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000}}
	_, rec := buildProfile(block, now)

	at, ok := divergence(installedProfile{}, block, now)
	assert.True(t, ok, "unknown profile always diverges")
	assert.Equal(t, now, at)

	at, ok = divergence(rec, block, now)
	assert.True(t, ok)
	assert.Equal(t, rec.To, at, "same blocks agree until the hold expires")

	moved := []chargeBlock{{Start: now.Add(5 * time.Hour), End: now.Add(6 * time.Hour), Power: 11000}}
	at, ok = divergence(rec, moved, now)
	assert.True(t, ok)
	assert.Equal(t, now.Add(2*time.Hour), at, "first moment the charger would act differently")

	at, _ = divergence(rec, nil, now.Add(4*time.Hour))
	assert.Equal(t, rec.To, at, "after the last block the profile holds 0 W like an empty plan")

	at, ok = divergence(rec, nil, rec.To.Add(-time.Hour))
	assert.True(t, ok)
	assert.Equal(t, rec.To, at, "hold expires: charger would run unlimited")

	at, ok = divergence(installedProfile{State: "none"}, nil, now)
	assert.True(t, ok, "cleared profile lets the car charge, plan says 0")
	assert.Equal(t, now, at)
}

// --- applyProfile ---

func TestApplyProfile_WritesWhenUnknown(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)

	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Equal(t, 1, profileCount(mock))
	assert.Equal(t, "plan", s.installed().State)

	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Equal(t, 1, profileCount(mock), "no rewrite when the charger already agrees")
	assert.Equal(t, 0, mock.clears)
}

func TestApplyProfile_NoWriteWithoutCar(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Available"}
	s, _ := newTestScheduler(t, mock)

	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Equal(t, 0, profileCount(mock), "nothing to steer without a car")
	assert.Equal(t, 0, mock.clears, "no clear on unplug either")
}

func TestApplyProfile_NoWhenDisconnectedOrNoCpID(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	mock := &mockCharger{connected: false, status: "Preparing"}
	s, st := newTestScheduler(t, mock)
	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	mock.connected = true
	_ = st.Delete("zappi.charge_box_id")
	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Equal(t, 0, profileCount(mock))
}

// A change that only matters later is written profileWriteLead before it
// matters; replans in between collapse into that one write.
func TestApplyProfile_FutureChangeDeferredAndCoalesced(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)
	s.applyProfile(plannedAt(now.Add(3*time.Hour), now.Add(4*time.Hour)))
	require.Equal(t, 1, profileCount(mock))

	for i := 0; i < 5; i++ { // the plan keeps moving, all beyond the lead
		withNow(t, now.Add(time.Duration(i*10)*time.Minute))
		s.applyProfile(plannedAt(now.Add(time.Duration(5+i)*time.Hour), now.Add(time.Duration(6+i)*time.Hour)))
	}
	assert.Equal(t, 1, profileCount(mock), "not needed until the old block would start")

	withNow(t, now.Add(3*time.Hour-profileWriteLead))
	s.applyProfile(plannedAt(now.Add(9*time.Hour), now.Add(10*time.Hour)))
	assert.Equal(t, 2, profileCount(mock), "written once, just before the charger would act wrongly")
}

func TestApplyProfile_ImmediateChangeIsWritten(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)
	s.applyProfile(plannedAt(now, now.Add(2*time.Hour)))
	withNow(t, now.Add(30*time.Minute))
	s.applyProfile(plannedAt(now.Add(5*time.Hour), now.Add(6*time.Hour))) // charging now must stop
	assert.Equal(t, 2, profileCount(mock))
}

// One profile is plan and hold: no daily rewrite while plugged in, only when
// the 0 W tail is about to expire.
func TestApplyProfile_HoldWrittenOnceNotDaily(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)
	s.applyProfile(nil)
	require.Equal(t, 1, profileCount(mock))
	to := s.installed().To

	withNow(t, now.Add(25*time.Hour))
	s.applyProfile(nil)
	assert.Equal(t, 1, profileCount(mock), "next day: still held")

	withNow(t, to.Add(-profileWriteLead))
	s.applyProfile(nil)
	assert.Equal(t, 2, profileCount(mock), "renewed just before it expires")
}

func TestApplyProfile_BudgetSpacingAndRestart(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)

	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	withNow(t, now.Add(5*time.Minute))
	s.applyProfile(plannedAt(now, now.Add(10*time.Minute))) // diverges now
	assert.Equal(t, 1, profileCount(mock), "spacing: no second write within 20 min")

	withNow(t, now.Add(profileWriteSpacing+time.Minute))
	s.applyProfile(plannedAt(now, now.Add(10*time.Minute)))
	assert.Equal(t, 2, profileCount(mock))

	withNow(t, now.Add(2*profileWriteSpacing+2*time.Minute))
	s.applyProfile(nil)
	assert.Equal(t, 2, profileCount(mock), "budget: 2 per hour")

	// Restart on the same store: budget and installed profile survive.
	s2 := &Scheduler{charger: mock, store: s.store, events: s.events, log: s.log}
	s2.loadProfileState()
	assert.Equal(t, "plan", s2.installed().State)
	s2.applyProfile(nil)
	assert.Equal(t, 2, profileCount(mock), "budget must survive a restart")

	withNow(t, now.Add(61*time.Minute))
	s2.applyProfile(plannedAt(now.Add(time.Hour), now.Add(2*time.Hour))) // charge now
	assert.Equal(t, 3, profileCount(mock), "oldest write left the window")
}

// A refusal backs off a full hour, persisted, and counts against the budget.
func TestApplyProfile_RefusalBacksOffAndPersists(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	fc := &failingCharger{mockCharger: mockCharger{connected: true, status: "Preparing"}, failSet: true}
	s, st := newTestScheduler(t, &fc.mockCharger)
	s.charger = fc

	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Equal(t, 1, fc.profileAttempts)
	assert.Equal(t, "", s.installed().State, "still unknown after a refusal")
	assert.Len(t, s.recentProfileWrites(now), 1, "refused attempts count")

	fc.failSet = false
	s2 := &Scheduler{charger: fc, store: st, events: s.events, log: s.log}
	s2.loadProfileState()
	withNow(t, now.Add(59*time.Minute))
	s2.applyProfile(plannedAt(now, now.Add(2*time.Hour)))
	assert.Equal(t, 0, fc.profiles, "backoff survives restart")

	withNow(t, now.Add(61*time.Minute))
	s2.applyProfile(plannedAt(now, now.Add(2*time.Hour)))
	assert.Equal(t, 1, fc.profiles)
	assert.Equal(t, "0", st.GetDefault(settingProfileFailures, ""))
}

func TestApplyProfile_UndeliveredNotCounted(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	fc := &failingCharger{mockCharger: mockCharger{connected: true, status: "Preparing"}, failSet: true,
		setErr: errors.New("send SetChargingProfile: cannot send request 1, no client CP001 exists")}
	s, _ := newTestScheduler(t, &fc.mockCharger)
	s.charger = fc

	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Empty(t, s.recentProfileWrites(now), "never reached the charger")
	fc.failSet = false
	withNow(t, now.Add(profileTransportBackoff+time.Second))
	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Equal(t, 1, fc.profiles)
}

func TestApplyProfile_ResetProfileStateForcesWrite(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)
	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	s.ResetProfileState()
	withNow(t, now.Add(profileWriteSpacing+time.Minute))
	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	assert.Equal(t, 2, profileCount(mock))
}

// --- force mode / ClearSchedule ---

func TestForce_ClearsOnlyWhenProfileDeniesPower(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)
	_ = s.store.Set("charger.mode", "force")

	s.applyProfile(plannedAt(now, now.Add(time.Hour))) // charging allowed now
	s.controlCharging()
	assert.Equal(t, 0, mock.clears, "profile allows power: keep it")

	withNow(t, now.Add(2*time.Hour)) // in the 0 W hold
	s.controlCharging()
	assert.Equal(t, 1, mock.clears)
	assert.Equal(t, "none", s.installed().State)
	s.controlCharging()
	assert.Equal(t, 1, mock.clears, "cleared once")
}

func TestClearSchedule_DoesNotWrite(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s, mock := pluggedScheduler(t)
	s.applyProfile(plannedAt(now, now.Add(time.Hour)))
	s.ClearSchedule()
	assert.Equal(t, 0, mock.clears)
	assert.Equal(t, 1, profileCount(mock))
}

// --- Finishing recovery ---

func TestFinishing_RejectedStartReconnectsThenStarts(t *testing.T) {
	now := time.Now()
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Finishing", startErr: errors.New("RemoteStartTransaction: Rejected")}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.setInstalled(installedProfile{State: "none"}) // keep profile writes out of the way
	s.current = plannedAt(now.Add(-time.Minute), now.Add(time.Hour))

	s.controlCharging()
	assert.Equal(t, 1, mock.starts)
	assert.Equal(t, 1, mock.reconnects, "rejected in Finishing: drop the connection")

	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "wait for the charger to settle after reconnect")

	s.lastFinishingReconnect = time.Now().Add(-finishingReconnectSettle - time.Second)
	mock.startErr = nil
	s.controlCharging()
	assert.Equal(t, 2, mock.starts, "start right after the reconnect")

	// Still rejected later: no second reconnect within the hour.
	mock.startErr = errors.New("RemoteStartTransaction: Rejected")
	s.lastFinishingStart = time.Time{}
	s.controlCharging()
	assert.Equal(t, 3, mock.starts)
	assert.Equal(t, 1, mock.reconnects)
}

func TestFinishing_NoRecoveryOutsidePlan(t *testing.T) {
	now := time.Now()
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Finishing"}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.setInstalled(installedProfile{State: "none"})
	s.current = plannedAt(now.Add(2*time.Hour), now.Add(3*time.Hour))

	s.controlCharging()
	assert.Equal(t, 0, mock.starts)
	assert.Equal(t, 0, mock.reconnects)
}

// --- Recompute integration ---

func TestRecompute_IdenticalSchedule_NoOcppTraffic(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Preparing"}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.current_soc", "65")
	_ = st.Set("scheduler.safe_soc", "70") // ~1h needed, must charge
	_ = st.Set("scheduler.departure_time", now.Add(24*time.Hour).Format("15:04"))
	s.tariff = &mockTariff{rates: []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.30},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.50},
	}}

	s.recompute()
	assert.Equal(t, 1, profileCount(mock), "first recompute should send profile")
	s.recompute()
	assert.Equal(t, 1, profileCount(mock), "identical recompute should not send another profile")
	assert.Equal(t, 0, mock.clears, "should never clear")
}

func TestProfileBurst_ConcurrentRecomputesWriteOnce(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Preparing"}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.current_soc", "65")
	_ = st.Set("scheduler.safe_soc", "70")
	_ = st.Set("scheduler.departure_time", now.Add(24*time.Hour).Format("15:04"))
	s.tariff = &mockTariff{rates: []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.30},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.50},
	}}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.recompute()
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, profileCount(mock), "burst of recomputes should only send 1 profile")
	assert.Equal(t, 0, mock.clears)
}
