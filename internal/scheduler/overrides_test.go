package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

func TestBuildForcePeriods_PricesFromRates(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		mkRate(now, 1, 0.40),
		mkRate(now.Add(time.Hour), 1, 0.20),
	}
	forces := []store.ScheduleOverride{
		{Kind: store.OverrideKindForce, PowerW: 11000, Start: now.Add(15 * time.Minute), End: now.Add(45 * time.Minute)},
	}
	got := buildForcePeriods(forces, rates)
	require.Len(t, got, 1)
	assert.Equal(t, sourceUserForce, got[0].Source)
	assert.InDelta(t, 0.40, got[0].Price, 0.001)
	assert.InDelta(t, 11000, got[0].Power, 0.001)
}

func TestBuildForcePeriods_NoRatesYieldsZeroPrice(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	forces := []store.ScheduleOverride{
		{Kind: store.OverrideKindForce, PowerW: 11000, Start: now, End: now.Add(time.Hour)},
	}
	got := buildForcePeriods(forces, nil)
	require.Len(t, got, 1)
	assert.InDelta(t, 0, got[0].Price, 0.001)
}

func TestTotalForceEnergy(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	periods := []SchedulePeriod{
		{Start: now, End: now.Add(time.Hour), Power: 11000},
		{Start: now.Add(2 * time.Hour), End: now.Add(150 * time.Minute), Power: 11000},
	}
	got := totalForceEnergy(periods)
	assert.InDelta(t, 11+5.5, got, 0.001)
}

func TestNextDeadlineAfter(t *testing.T) {
	loc := time.UTC
	t1 := time.Date(2026, 5, 4, 5, 0, 0, 0, loc)
	got := nextDeadlineAfter(t1, 7, 0, loc)
	assert.Equal(t, time.Date(2026, 5, 4, 7, 0, 0, 0, loc), got)

	t2 := time.Date(2026, 5, 4, 8, 0, 0, 0, loc)
	got = nextDeadlineAfter(t2, 7, 0, loc)
	assert.Equal(t, time.Date(2026, 5, 5, 7, 0, 0, 0, loc), got)
}

// Integration: recompute with a force override produces a schedule containing
// the force as a user_force period, even when SoC > skipAboveSoC would normally
// skip auto-selection.
func TestRecompute_ForceSurvivesSoCSkip(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.current_soc", "75")
	_ = st.Set("scheduler.departure_time", "07:00")

	// All positive prices: auto would normally skip at SoC 75 > 70.
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.30},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.40},
	}
	s.tariff = &mockTariff{rates: rates}

	// Force charging window at now+30min → now+90min (sub-hour, crosses two tariff hours).
	_, err := st.InsertOverride(store.ScheduleOverride{
		Kind: store.OverrideKindForce, PowerW: 11000,
		Start: now.Add(30 * time.Minute), End: now.Add(90 * time.Minute),
	})
	require.NoError(t, err)

	s.recompute()

	require.NotNil(t, s.current, "force override must produce a schedule despite SoC skip")
	require.NotEmpty(t, s.current.Slots)
	var foundForce bool
	for _, slot := range s.current.Slots {
		for _, p := range slot.Periods {
			if p.Source == sourceUserForce {
				foundForce = true
				assert.True(t, p.Start.Equal(now.Add(30*time.Minute)), "start mismatch: got %v want %v", p.Start, now.Add(30*time.Minute))
				assert.True(t, p.End.Equal(now.Add(90*time.Minute)), "end mismatch: got %v want %v", p.End, now.Add(90*time.Minute))
				assert.InDelta(t, 11000, p.Power, 0.01)
			}
		}
	}
	assert.True(t, foundForce, "schedule must contain the user_force period")
}

// Integration: recompute with a block override removes those hours from
// auto-selection but keeps other affordable hours.
func TestRecompute_BlockExcludesHourFromAutoSelection(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.current_soc", "65")
	_ = st.Set("scheduler.safe_soc", "70") // ~1h needed
	_ = st.Set("scheduler.departure_time", now.Add(24*time.Hour).Format("15:04"))

	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.10}, // cheapest
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.30},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: 0.50},
	}
	s.tariff = &mockTariff{rates: rates}

	// Block the cheapest hour.
	_, err := st.InsertOverride(store.ScheduleOverride{
		Kind:  store.OverrideKindBlock,
		Start: now, End: now.Add(time.Hour),
	})
	require.NoError(t, err)

	s.recompute()

	require.NotNil(t, s.current)
	require.NotEmpty(t, s.current.Slots)
	for _, slot := range s.current.Slots {
		for _, p := range slot.Periods {
			assert.NotEqual(t, 0.10, p.Price, "blocked hour must not be selected")
			assert.False(t, p.Start.Equal(now) && p.End.Equal(now.Add(time.Hour)),
				"blocked hour fragment must not appear")
		}
	}
}

func TestManualSelectionEscapesLegacyCancelledDay(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Available"}
	s, st := newTestScheduler(t, mock)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, st.Set("scheduler.safe_soc", "70"))
	s.tariff = &mockTariff{rates: []tariff.Rate{{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: .3}}}
	s.recompute()
	require.NotNil(t, s.current)
	slot := s.current.Slots[0]
	require.True(t, s.CancelSlot(slot.Date))
	period := slot.Periods[0]
	_, err := st.ApplyOverride(store.ScheduleOverride{Kind: store.OverrideKindForce, Start: period.Start, End: period.End, PowerW: 11000}, nil)
	require.NoError(t, err)
	s.recompute()
	require.NotNil(t, s.current)
	require.NotEmpty(t, s.current.ActivePeriods())
	assert.False(t, s.current.Slots[0].Cancelled)
	assert.True(t, s.current.ActivePeriods()[0].IsForce())
	assert.Zero(t, mock.stops)
}

func TestResetPausesAutoButAllowsManualWindows(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Available"}
	s, st := newTestScheduler(t, mock)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, st.Set("scheduler.safe_soc", "70"))
	s.tariff = &mockTariff{rates: []tariff.Rate{{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: .3}}}
	s.recompute()
	require.NotNil(t, s.current)
	require.NoError(t, st.ResetSchedule())
	s.ClearSchedule()
	s.recompute()
	assert.Nil(t, s.current)
	assert.Equal(t, "user_cancelled", s.skipReasonKey)
	_, err := st.ApplyOverride(store.ScheduleOverride{Kind: store.OverrideKindForce, Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), PowerW: 11000}, nil)
	require.NoError(t, err)
	s.recompute()
	require.NotNil(t, s.current)
	require.Len(t, s.current.ActivePeriods(), 1)
	assert.True(t, s.current.ActivePeriods()[0].IsForce())
	assert.Equal(t, "user_cancelled", s.skipReasonKey)
	assert.Zero(t, mock.stops)
}
