package scheduler

import (
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func planPeriods(t *testing.T, sched *Schedule) [][2]float64 {
	t.Helper()
	p := BuildPlanProfile(sched)
	require.NotNil(t, p)
	assert.Equal(t, types.ChargingProfilePurposeTxDefaultProfile, p.ChargingProfilePurpose)
	assert.Equal(t, types.ChargingRateUnitWatts, p.ChargingSchedule.ChargingRateUnit)
	var out [][2]float64
	for _, cp := range p.ChargingSchedule.ChargingSchedulePeriod {
		out = append(out, [2]float64{float64(cp.StartPeriod), cp.Limit})
	}
	return out
}

func TestBuildPlanProfileNil(t *testing.T) {
	assert.Nil(t, BuildPlanProfile(nil))
	assert.Nil(t, BuildPlanProfile(&Schedule{}))
}

func TestBuildPlanProfileConsolidatesAdjacentSamePower(t *testing.T) {
	now := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	withNow(t, now)
	sched := &Schedule{Slots: []ScheduleSlot{{Periods: []SchedulePeriod{
		{Start: now, End: now.Add(time.Hour), Power: 11000},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Power: 11000},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
	}}}}
	assert.Equal(t, [][2]float64{{0, 11000}, {3 * 3600, 0}}, planPeriods(t, sched))
}

func TestBuildPlanProfilePreservesPowerChangeAtBoundary(t *testing.T) {
	now := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	withNow(t, now)
	sched := &Schedule{Slots: []ScheduleSlot{{Periods: []SchedulePeriod{
		{Start: now, End: now.Add(time.Hour), Power: 11000},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Power: 7000},
	}}}}
	assert.Equal(t, [][2]float64{{0, 11000}, {3600, 7000}, {7200, 0}}, planPeriods(t, sched))
}

func TestBuildPlanProfileSkipsCancelledAndPastPeriods(t *testing.T) {
	now := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	withNow(t, now)
	sched := &Schedule{Slots: []ScheduleSlot{
		{Periods: []SchedulePeriod{{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour), Power: 11000}}},
		{Cancelled: true, Periods: []SchedulePeriod{{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Power: 11000}}},
		{Periods: []SchedulePeriod{{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Power: 11000}}},
	}}
	assert.Equal(t, [][2]float64{{0, 0}, {3 * 3600, 11000}, {4 * 3600, 0}}, planPeriods(t, sched))
}

func TestBuildPlanProfileCapsBlocks(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	withNow(t, now)
	var periods []SchedulePeriod
	for i := 0; i < 2*maxProfileBlocks+4; i += 2 { // every other hour → separate blocks
		periods = append(periods, SchedulePeriod{Start: now.Add(time.Duration(i) * time.Hour), End: now.Add(time.Duration(i+1) * time.Hour), Power: 11000})
	}
	p := BuildPlanProfile(&Schedule{Slots: []ScheduleSlot{{Periods: periods}}})
	require.NotNil(t, p)
	assert.LessOrEqual(t, len(p.ChargingSchedule.ChargingSchedulePeriod), 2*maxProfileBlocks+1)
}

func TestPlanHash_Nil(t *testing.T) {
	assert.Empty(t, planHash(nil))
	assert.Empty(t, planHash(&Schedule{}))
}

func TestPlanHash_ChangesWhenFutureChanges(t *testing.T) {
	now := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	withNow(t, now)
	a := &Schedule{Slots: []ScheduleSlot{{Periods: []SchedulePeriod{{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Power: 11000}}}}}
	b := &Schedule{Slots: []ScheduleSlot{{Periods: []SchedulePeriod{{Start: now.Add(time.Hour), End: now.Add(3 * time.Hour), Power: 11000}}}}}
	assert.NotEqual(t, planHash(a), planHash(b))
	// Hourly split vs one long period → same blocks → same hash (no rewrite).
	c := &Schedule{Slots: []ScheduleSlot{{Periods: []SchedulePeriod{
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Power: 11000},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
	}}}}
	assert.Equal(t, planHash(b), planHash(c))
}

func TestIsChargeTime(t *testing.T) {
	now := time.Now()
	sched := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date: "2026-01-01",
				Periods: []SchedulePeriod{
					{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Power: 11000},
				},
			},
		},
	}
	assert.True(t, IsChargeTime(sched))

	sched2 := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date: "2026-01-01",
				Periods: []SchedulePeriod{
					{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Power: 11000},
				},
			},
		},
	}
	assert.False(t, IsChargeTime(sched2))

	// Cancelled slot should not count
	sched3 := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:      "2026-01-01",
				Cancelled: true,
				Periods: []SchedulePeriod{
					{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Power: 11000},
				},
			},
		},
	}
	assert.False(t, IsChargeTime(sched3))

	assert.False(t, IsChargeTime(nil))

	// Period that has not yet started must not count
	sched4 := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date: "2026-01-01",
				Periods: []SchedulePeriod{
					{Start: now.Add(30 * time.Second), End: now.Add(time.Hour), Power: 11000},
				},
			},
		},
	}
	assert.False(t, IsChargeTime(sched4))
}

func TestActiveSlot_NilSchedule(t *testing.T) {
	assert.Nil(t, activeSlot(nil))
}

func TestActiveSlot_NoSlots(t *testing.T) {
	assert.Nil(t, activeSlot(&Schedule{}))
}

func TestActiveSlot_InWindow(t *testing.T) {
	now := time.Now()
	sched := &Schedule{Slots: []ScheduleSlot{{
		Date: "2026-01-01",
		Periods: []SchedulePeriod{
			{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Power: 11000},
		},
	}}}
	got := activeSlot(sched)
	require.NotNil(t, got)
	assert.Equal(t, "2026-01-01", got.Date)
	// Returned pointer must reference the slot inside sched, not a copy.
	assert.Same(t, &sched.Slots[0], got)
}

func TestActiveSlot_BeforePeriod(t *testing.T) {
	now := time.Now()
	sched := &Schedule{Slots: []ScheduleSlot{{
		Date: "2026-01-01",
		Periods: []SchedulePeriod{
			{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
		},
	}}}
	assert.Nil(t, activeSlot(sched))
}

func TestActiveSlot_JustBeforeStart(t *testing.T) {
	now := time.Now()
	// Period starts 30s in the future. Without startMargin, this must NOT be active.
	sched := &Schedule{Slots: []ScheduleSlot{{
		Date: "2026-01-01",
		Periods: []SchedulePeriod{
			{Start: now.Add(30 * time.Second), End: now.Add(time.Hour), Power: 11000},
		},
	}}}
	assert.Nil(t, activeSlot(sched))
}

func TestActiveSlot_AfterEnd(t *testing.T) {
	now := time.Now()
	sched := &Schedule{Slots: []ScheduleSlot{{
		Date: "2026-01-01",
		Periods: []SchedulePeriod{
			{Start: now.Add(-2 * time.Hour), End: now.Add(-1 * time.Hour), Power: 11000},
		},
	}}}
	assert.Nil(t, activeSlot(sched))
}

func TestActiveSlot_CancelledIgnored(t *testing.T) {
	now := time.Now()
	sched := &Schedule{Slots: []ScheduleSlot{{
		Date:      "2026-01-01",
		Cancelled: true,
		Periods: []SchedulePeriod{
			{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Power: 11000},
		},
	}}}
	assert.Nil(t, activeSlot(sched))
}

func TestActiveSlot_ZeroPowerIgnored(t *testing.T) {
	now := time.Now()
	sched := &Schedule{Slots: []ScheduleSlot{{
		Date: "2026-01-01",
		Periods: []SchedulePeriod{
			{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Power: 0},
		},
	}}}
	assert.Nil(t, activeSlot(sched))
}

func TestActiveSlot_PicksMatchingFromMultipleSlots(t *testing.T) {
	now := time.Now()
	sched := &Schedule{Slots: []ScheduleSlot{
		{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour), Power: 11000},
			},
		},
		{
			Date: "2026-01-02",
			Periods: []SchedulePeriod{
				{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Power: 11000},
			},
		},
		{
			Date: "2026-01-03",
			Periods: []SchedulePeriod{
				{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
			},
		},
	}}
	got := activeSlot(sched)
	require.NotNil(t, got)
	assert.Equal(t, "2026-01-02", got.Date)
}
