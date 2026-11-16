package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type mockLimiter struct{ calls []int }

func (m *mockLimiter) SetSocTarget(t int) error { m.calls = append(m.calls, t); return nil }

func fillPlan(now time.Time) *Schedule {
	return &Schedule{Slots: []ScheduleSlot{{Periods: []SchedulePeriod{
		{Start: now.Add(-10 * time.Minute), End: now.Add(50 * time.Minute), Power: 10800, Reason: ReasonCheapFill, FillSoC: 100},
	}}}}
}

func TestCarLimit_RaisedForCheapFillAndRestored(t *testing.T) {
	now := time.Now()
	withNow(t, now)
	s, st := newTestScheduler(t, &mockCharger{connected: true})
	ml := &mockLimiter{}
	s.SetCarLimiter(ml)
	_ = st.Set("vehicle.soc_target", "80")

	s.manageCarLimit(fillPlan(now), true, false)
	assert.Equal(t, []int{100}, ml.calls)
	assert.Equal(t, "80", st.GetDefault(settingFillRaisedFrom, ""))

	// Cooldown protects the Renault quota.
	s.manageCarLimit(nil, true, false)
	assert.Len(t, ml.calls, 1)

	withNow(t, now.Add(11*time.Minute))
	s.manageCarLimit(nil, true, false)
	assert.Equal(t, []int{100, 80}, ml.calls)
	assert.Equal(t, "", st.GetDefault(settingFillRaisedFrom, ""))
}

func TestCarLimit_TripLimitResetOnUnplug(t *testing.T) {
	s, st := newTestScheduler(t, &mockCharger{connected: true})
	ml := &mockLimiter{}
	s.SetCarLimiter(ml)
	_ = st.Set("vehicle.soc_target", "100") // raised by the user: a trip
	_ = st.Set(settingTripReadyBy, time.Now().Add(time.Hour).Format(time.RFC3339))

	s.manageCarLimit(nil, true, false)
	assert.Empty(t, ml.calls, "never lowers a user's trip limit while plugged in")

	s.manageCarLimit(nil, false, true)
	assert.Equal(t, []int{80}, ml.calls)
	assert.Equal(t, "", st.GetDefault(settingTripReadyBy, ""))
}

func TestCarLimit_NormalUnplugLeavesLimitAlone(t *testing.T) {
	s, st := newTestScheduler(t, &mockCharger{connected: true})
	ml := &mockLimiter{}
	s.SetCarLimiter(ml)
	_ = st.Set("vehicle.soc_target", "80")
	s.manageCarLimit(nil, false, true)
	assert.Empty(t, ml.calls)
}
