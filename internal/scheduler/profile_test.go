package scheduler

import (
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildProfile(t *testing.T) {
	now := time.Now().Truncate(time.Hour)

	sched := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:     "2026-01-01",
				Deadline: now.Add(4 * time.Hour),
				Periods: []SchedulePeriod{
					{Start: now, End: now.Add(time.Hour), Power: 11000, Price: 0.30},
					{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000, Price: 0.50},
				},
				Cost:   8.80,
				Energy: 22,
			},
		},
		Cost:     8.80,
		Energy:   22,
		Deadline: now.Add(4 * time.Hour),
	}

	profile := BuildProfile(sched, 11000)
	require.NotNil(t, profile)

	assert.Equal(t, types.ChargingProfilePurposeTxDefaultProfile, profile.ChargingProfilePurpose)
	assert.Equal(t, types.ChargingProfileKindAbsolute, profile.ChargingProfileKind)
	assert.Equal(t, types.ChargingRateUnitWatts, profile.ChargingSchedule.ChargingRateUnit)

	periods := profile.ChargingSchedule.ChargingSchedulePeriod
	require.GreaterOrEqual(t, len(periods), 3)

	// First period: charge at 11kW
	assert.Equal(t, 0, periods[0].StartPeriod)
	assert.Equal(t, float64(11000), periods[0].Limit)

	// Second period: gap (0W)
	assert.Equal(t, 3600, periods[1].StartPeriod) // 1 hour in
	assert.Equal(t, float64(0), periods[1].Limit)

	// Third period: charge again at 11kW
	assert.Equal(t, 7200, periods[2].StartPeriod) // 2 hours in
	assert.Equal(t, float64(11000), periods[2].Limit)
}

func TestBuildProfileNil(t *testing.T) {
	assert.Nil(t, BuildProfile(nil, 11000))
	assert.Nil(t, BuildProfile(&Schedule{}, 11000))
}

func TestBuildProfileSkipsCancelledSlots(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	sched := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:      "2026-01-01",
				Cancelled: true,
				Periods: []SchedulePeriod{
					{Start: now, End: now.Add(time.Hour), Power: 11000},
				},
			},
		},
	}
	assert.Nil(t, BuildProfile(sched, 11000))
}

func TestProfileHash_Deterministic(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	sched := &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: now, End: now.Add(time.Hour), Power: 11000},
				{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
			},
		}},
	}
	p1 := BuildProfile(sched, 11000)
	p2 := BuildProfile(sched, 11000)

	h1 := ProfileHash(p1)
	h2 := ProfileHash(p2)

	assert.NotEmpty(t, h1)
	assert.Equal(t, h1, h2, "same profile must produce the same hash")
}

func TestProfileHash_DifferentSchedule(t *testing.T) {
	now := time.Now().Truncate(time.Hour)

	sched1 := &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: now, End: now.Add(time.Hour), Power: 11000},
			},
		}},
	}
	sched2 := &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: now, End: now.Add(2 * time.Hour), Power: 11000},
			},
		}},
	}

	h1 := ProfileHash(BuildProfile(sched1, 11000))
	h2 := ProfileHash(BuildProfile(sched2, 11000))

	assert.NotEqual(t, h1, h2, "different schedules must produce different hashes")
}

func TestProfileHash_Nil(t *testing.T) {
	assert.Empty(t, ProfileHash(nil))
	assert.Empty(t, ProfileHash(&types.ChargingProfile{}))
}

func TestScheduleHash_StableAcrossRecomputes(t *testing.T) {
	origTimeNow := timeNow
	defer func() { timeNow = origTimeNow }()

	// A schedule with a past period and a future period
	past := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	future := time.Now().Add(2 * time.Hour).Truncate(time.Hour)

	sched := &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: past, End: past.Add(time.Hour), Power: 11000},
				{Start: future, End: future.Add(time.Hour), Power: 11000},
			},
		}},
	}

	// Hash at time T1: past period already ended, schedule still includes it
	timeNow = func() time.Time { return past.Add(90 * time.Minute) }
	h1 := ScheduleHash(sched)

	// Later recompute: past period dropped from schedule, only future remains
	schedLater := &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: future, End: future.Add(time.Hour), Power: 11000},
			},
		}},
	}

	// Hash at time T2 (only future period in schedule)
	timeNow = func() time.Time { return past.Add(100 * time.Minute) }
	h2 := ScheduleHash(schedLater)

	assert.Equal(t, h1, h2, "hash should be stable when only past periods change")
}

func TestScheduleHash_ChangesWhenFutureChanges(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).Truncate(time.Hour)

	sched1 := &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: future, End: future.Add(time.Hour), Power: 11000},
			},
		}},
	}
	sched2 := &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-01-01",
			Periods: []SchedulePeriod{
				{Start: future, End: future.Add(2 * time.Hour), Power: 11000},
			},
		}},
	}

	h1 := ScheduleHash(sched1)
	h2 := ScheduleHash(sched2)
	assert.NotEqual(t, h1, h2, "hash should change when future periods differ")
}

func TestScheduleHash_Nil(t *testing.T) {
	assert.Empty(t, ScheduleHash(nil))
	assert.Empty(t, ScheduleHash(&Schedule{}))
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
