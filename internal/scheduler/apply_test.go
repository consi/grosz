package scheduler

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"

	"github.com/consi/grosz/internal/tariff"
)

// failingCharger wraps mockCharger and can fail SetChargingProfile on demand.
type failingCharger struct {
	mockCharger
	failSet bool
}

func (f *failingCharger) SetChargingProfile(cpID string, connectorID int, profile *types.ChargingProfile) error {
	if f.failSet {
		return errors.New("simulated OCPP timeout")
	}
	return f.mockCharger.SetChargingProfile(cpID, connectorID, profile)
}

func makeSchedule(now time.Time) *Schedule {
	return &Schedule{
		Slots: []ScheduleSlot{{
			Date:     now.Format("2006-01-02"),
			Deadline: now.Add(4 * time.Hour),
			Periods: []SchedulePeriod{
				{Start: now, End: now.Add(time.Hour), Power: 11000, Price: 0.30},
			},
			Cost:   3.30,
			Energy: 11,
		}},
		Cost:     3.30,
		Energy:   11,
		Deadline: now.Add(4 * time.Hour),
	}
}

func makeDifferentSchedule(now time.Time) *Schedule {
	return &Schedule{
		Slots: []ScheduleSlot{{
			Date:     now.Format("2006-01-02"),
			Deadline: now.Add(4 * time.Hour),
			Periods: []SchedulePeriod{
				{Start: now, End: now.Add(2 * time.Hour), Power: 11000, Price: 0.50},
			},
			Cost:   11.00,
			Energy: 22,
		}},
		Cost:     11.00,
		Energy:   22,
		Deadline: now.Add(4 * time.Hour),
	}
}

// --- applyProfile tests ---

func TestApplyProfile_SendsOnFirstCall(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 1, mock.profiles, "first call should send SetChargingProfile")
	assert.Equal(t, 0, mock.clears, "should not call ClearChargingProfile")
}

func TestApplyProfile_SkipsWhenHashUnchanged(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	sched := makeSchedule(now)
	s.applyProfile(sched)
	s.applyProfile(sched)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 1, mock.profiles, "second identical call should be skipped")
}

func TestApplyProfile_SendsWhenHashDiffers(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	// Clear cooldown so we can test hash change independently
	s.mu.Lock()
	s.lastProfileSent = time.Time{}
	s.mu.Unlock()

	s.applyProfile(makeDifferentSchedule(now))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 2, mock.profiles, "different schedule should send new profile")
}

func TestApplyProfile_NoClearBeforeSet(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	s.mu.Lock()
	s.lastProfileSent = time.Time{}
	s.mu.Unlock()

	s.applyProfile(makeDifferentSchedule(now))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 0, mock.clears, "ClearChargingProfile should never be called from applyProfile")
}

func TestApplyProfile_SendsAfterResetProfileState(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	sched := makeSchedule(now)
	s.applyProfile(sched)

	// Simulate boot: reset state
	s.ResetProfileState()

	s.applyProfile(sched)

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 2, mock.profiles, "should re-send after ResetProfileState even for same schedule")
}

func TestApplyProfile_CooldownPreventsFlood(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	// Different schedule but within cooldown window
	s.applyProfile(makeDifferentSchedule(now))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 1, mock.profiles, "cooldown should prevent second send")
}

func TestApplyProfile_SendsAfterCooldownExpires(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	// Expire cooldown manually
	s.mu.Lock()
	s.lastProfileSent = time.Now().Add(-profileCmdCooldown - time.Second)
	s.mu.Unlock()

	s.applyProfile(makeDifferentSchedule(now))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 2, mock.profiles, "should send after cooldown expires")
}

func TestApplyProfile_FailureDoesNotUpdateHash(t *testing.T) {
	fc := &failingCharger{
		mockCharger: mockCharger{connected: true},
		failSet:     true,
	}
	s, st := newTestScheduler(t, &fc.mockCharger)
	s.charger = fc
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	s.mu.RLock()
	assert.Empty(t, s.lastProfileHash, "hash should not be set on failure")
	s.mu.RUnlock()

	// Now succeed
	fc.failSet = false
	s.applyProfile(makeSchedule(now))

	s.mu.RLock()
	assert.NotEmpty(t, s.lastProfileHash, "hash should be set on success")
	s.mu.RUnlock()
}

func TestApplyProfile_NoCpID(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, _ := newTestScheduler(t, mock)
	// Delete charge_box_id
	_ = s.store.Delete("zappi.charge_box_id")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 0, mock.profiles, "should not send without charge_box_id")
}

// --- ClearSchedule tests ---

func TestClearSchedule_SkipsWhenNoProfileStored(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, _ := newTestScheduler(t, mock)

	s.ClearSchedule()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 0, mock.clears, "should not send ClearChargingProfile when no profile was set")
}

func TestClearSchedule_SendsClearWhenProfileExists(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	s.ClearSchedule()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 1, mock.clears, "should send ClearChargingProfile when profile was set")
}

func TestClearSchedule_ClearsHash(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))
	s.ClearSchedule()

	s.mu.RLock()
	assert.Empty(t, s.lastProfileHash, "hash should be cleared after ClearSchedule")
	s.mu.RUnlock()
}

func TestClearSchedule_DoubleClearOnlyCallsOnce(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))
	s.ClearSchedule()
	s.ClearSchedule()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 1, mock.clears, "second ClearSchedule should be a no-op")
}

// --- ResetProfileState tests ---

func TestResetProfileState_ClearsHashAndCooldown(t *testing.T) {
	mock := &mockCharger{connected: true}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.max_power", "11000")
	now := time.Now().Truncate(time.Hour)

	s.applyProfile(makeSchedule(now))

	s.mu.RLock()
	assert.NotEmpty(t, s.lastProfileHash)
	assert.False(t, s.lastProfileSent.IsZero())
	s.mu.RUnlock()

	s.ResetProfileState()

	s.mu.RLock()
	assert.Empty(t, s.lastProfileHash, "hash should be cleared")
	assert.True(t, s.lastProfileSent.IsZero(), "cooldown should be cleared")
	s.mu.RUnlock()
}

// --- Recompute integration tests ---

func TestRecompute_IdenticalSchedule_NoOcppTraffic(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	mock := &mockCharger{connected: true, status: "Preparing"}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.target_energy", "10")
	_ = st.Set("scheduler.deadline_time", now.Add(24*time.Hour).Format("15:04"))

	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.30},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.50},
	}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	mock.mu.Lock()
	firstProfiles := mock.profiles
	mock.mu.Unlock()
	assert.Equal(t, 1, firstProfiles, "first recompute should send profile")

	// Expire cooldown
	s.mu.Lock()
	s.lastProfileSent = time.Now().Add(-profileCmdCooldown - time.Second)
	s.mu.Unlock()

	s.recompute()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 1, mock.profiles, "identical recompute should not send another profile")
	assert.Equal(t, 0, mock.clears, "should never clear")
}

func TestRecompute_ChangedSchedule_SendsProfile(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	mock := &mockCharger{connected: true, status: "Preparing"}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.target_energy", "10")
	_ = st.Set("scheduler.deadline_time", now.Add(24*time.Hour).Format("15:04"))

	// Future-only rates so neither computed slot is in progress (which would
	// engage the preserve-active-slot path and dedup the second send).
	rates := []tariff.Rate{
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: 0.30},
		{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: 0.50},
	}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	// Change rates significantly so the schedule changes
	s.tariff = &mockTariff{rates: []tariff.Rate{
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: 0.80},
		{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: 0.10},
	}}

	// Expire cooldown
	s.mu.Lock()
	s.lastProfileSent = time.Now().Add(-profileCmdCooldown - time.Second)
	s.mu.Unlock()

	s.recompute()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 2, mock.profiles, "changed schedule should send new profile")
	assert.Equal(t, 0, mock.clears, "should never clear from recompute")
}

func TestProfileCooldown_BurstProtection(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	mock := &mockCharger{connected: true, status: "Preparing"}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.target_energy", "10")
	_ = st.Set("scheduler.deadline_time", now.Add(24*time.Hour).Format("15:04"))

	var wg sync.WaitGroup
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.30},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.50},
	}
	s.tariff = &mockTariff{rates: rates}

	// Simulate multiple rapid recomputes (like boot cascades)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.recompute()
		}()
	}
	wg.Wait()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 1, mock.profiles, "burst of recomputes should only send 1 profile")
	assert.Equal(t, 0, mock.clears, "no clears should be sent")
}
