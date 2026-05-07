package scheduler

import (
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/tariff"
)

func TestComputeScheduleCheapestHours(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	deadline := now.Add(24 * time.Hour)

	// 24 hours with varied prices
	rates := make([]tariff.Rate, 24)
	prices := []float64{
		0.80, 0.75, 0.70, 0.65, // 0-3: moderate
		0.30, 0.25, 0.20, 0.22, // 4-7: cheap
		0.90, 0.95, 1.00, 0.85, // 8-11: expensive
		0.60, 0.55, 0.50, 0.45, // 12-15: moderate
		0.70, 0.75, 0.80, 0.85, // 16-19: moderate-high
		0.40, 0.35, 0.30, 0.50, // 20-23: cheap-moderate
	}
	for i := 0; i < 24; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: prices[i],
		}
	}

	cfg := Config{
		TargetEnergy:   32, // kWh — with 3% headroom: ceil(32*1.03/11) = 3 hours
		Deadline:       deadline,
		MaxPower:       11000, // W = 11 kW
		MinPower:       1380,
		ChargeHeadroom: 3,
	}

	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched)
	require.Len(t, sched.Slots, 1)

	slot := sched.Slots[0]
	assert.Len(t, slot.Periods, 3)

	// Should pick the 3 cheapest: 0.20, 0.22, 0.25
	for _, p := range slot.Periods {
		assert.LessOrEqual(t, p.Price, 0.30, "should pick cheapest hours")
		assert.Equal(t, float64(11000), p.Power)
	}

	// Verify sorted chronologically
	for i := 1; i < len(slot.Periods); i++ {
		assert.True(t, slot.Periods[i].Start.After(slot.Periods[i-1].Start))
	}
}

func TestComputeScheduleSlowerCharger(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	deadline := now.Add(12 * time.Hour)

	rates := make([]tariff.Rate, 12)
	for i := 0; i < 12; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: float64(i+1) * 0.10, // 0.10 to 1.20
		}
	}

	cfg := Config{
		TargetEnergy:   13.5, // kWh — with 3% headroom: ceil(13.5*1.03/3.6) = 4 hours
		Deadline:       deadline,
		MaxPower:       3600, // W = 3.6 kW (single phase)
		ChargeHeadroom: 3,
	}

	sched := ComputeSchedule(rates, cfg, 3600)
	require.NotNil(t, sched)
	require.Len(t, sched.Slots, 1)

	slot := sched.Slots[0]
	assert.Len(t, slot.Periods, 4)

	// Should pick hours 0,1,2,3 (cheapest)
	assert.Equal(t, 0.10, slot.Periods[0].Price)
	assert.Equal(t, 0.40, slot.Periods[3].Price)
}

func TestComputeScheduleDeadlinePressure(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	deadline := now.Add(6 * time.Hour)

	rates := make([]tariff.Rate, 6)
	for i := 0; i < 6; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: 1.00, // all expensive
		}
	}

	cfg := Config{
		TargetEnergy:   60, // kWh — needs all 6 hours at 11kW = 66 kWh
		Deadline:       deadline,
		MaxPower:       11000,
		ChargeHeadroom: 3,
	}

	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched)
	require.Len(t, sched.Slots, 1)

	// Must use all 6 hours (ceil(60/11) = 6)
	assert.Len(t, sched.Slots[0].Periods, 6)
}

func TestComputeScheduleNoRates(t *testing.T) {
	cfg := Config{TargetEnergy: 30, Deadline: time.Now().Add(24 * time.Hour), MaxPower: 11000, ChargeHeadroom: 3}
	sched := ComputeSchedule(nil, cfg, 11000)
	assert.Nil(t, sched)
}

func TestComputeScheduleZeroTarget(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{{Start: now, End: now.Add(time.Hour), Price: 0.5}}
	cfg := Config{TargetEnergy: 0, Deadline: now.Add(24 * time.Hour), MaxPower: 11000, ChargeHeadroom: 3}
	sched := ComputeSchedule(rates, cfg, 11000)
	assert.Nil(t, sched)
}

func TestComputeScheduleNegativePrices(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.50},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: -0.10},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: 0.30},
		{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: -0.05},
	}

	// Need 32 kWh — with 3% headroom: ceil(32*1.03/11) = 3 hours
	// Should pick the 3 cheapest: -0.10, -0.05, 0.30 (chronological order)
	cfg := Config{
		TargetEnergy:   32,
		Deadline:       now.Add(5 * time.Hour),
		MaxPower:       11000,
		ChargeHeadroom: 3,
	}
	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched)
	require.Len(t, sched.Slots, 1)
	assert.Len(t, sched.Slots[0].Periods, 3)
	assert.Equal(t, -0.10, sched.Slots[0].Periods[0].Price)
	assert.Equal(t, 0.30, sched.Slots[0].Periods[1].Price)
	assert.Equal(t, -0.05, sched.Slots[0].Periods[2].Price)
}

func TestComputeScheduleNegativePricesLimitedCapacity(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: -0.05},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: -0.20},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: -0.15},
		{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: -0.10},
	}

	// Only 21 kWh capacity — with 3% headroom: ceil(21*1.03/11) = 2 hours
	// Should pick the 2 most negative: -0.20 and -0.15
	cfg := Config{
		TargetEnergy:   21,
		Deadline:       now.Add(5 * time.Hour),
		MaxPower:       11000,
		ChargeHeadroom: 3,
	}
	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched)
	require.Len(t, sched.Slots, 1)
	assert.Len(t, sched.Slots[0].Periods, 2)
	assert.Equal(t, -0.20, sched.Slots[0].Periods[0].Price)
	assert.Equal(t, -0.15, sched.Slots[0].Periods[1].Price)
	assert.True(t, sched.Cost < 0, "cost should be negative (earnings)")
}

func TestComputeScheduleNegativePricesZeroTarget(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.50},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: -0.10},
	}

	// No energy needed (battery at target SoC) — no schedule even with negative prices.
	// SoC limits are respected; the battery has no headroom to absorb energy.
	cfg := Config{
		TargetEnergy:   0,
		Deadline:       now.Add(3 * time.Hour),
		MaxPower:       11000,
		ChargeHeadroom: 3,
	}
	sched := ComputeSchedule(rates, cfg, 11000)
	assert.Nil(t, sched)
}

func TestComputeScheduleCost(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.50},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.30},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: 0.80},
	}

	cfg := Config{
		TargetEnergy:   21, // with 3% headroom: ceil(21*1.03/11) = 2 hours
		Deadline:       now.Add(4 * time.Hour),
		MaxPower:       11000,
		ChargeHeadroom: 3,
	}

	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched)
	require.Len(t, sched.Slots, 1)
	assert.Len(t, sched.Slots[0].Periods, 2)

	// Should pick 0.30 and 0.50 hours
	expectedCost := math.Round((0.30*11+0.50*11)*100) / 100
	assert.Equal(t, expectedCost, sched.Cost)
}

func TestComputeScheduleMultiDay(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	// Deadline at 4 hours from now — first window has only 4 hours
	firstDeadline := now.Add(4 * time.Hour)

	// 48 hours of rates spanning two day windows
	rates := make([]tariff.Rate, 48)
	for i := 0; i < 48; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: 0.50 + float64(i%12)*0.05, // repeating price pattern
		}
	}

	cfg := Config{
		TargetEnergy:   50, // with 3% headroom: ceil(50*1.03/11) = 5 hours, exceeds first 4h window
		Deadline:       firstDeadline,
		MaxPower:       11000,
		ChargeHeadroom: 3,
	}

	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched)
	assert.GreaterOrEqual(t, len(sched.Slots), 2, "should spill into second day slot")

	// Energy should be at least the target (headroom may add slightly more)
	assert.GreaterOrEqual(t, sched.Energy, 50.0, "total energy should cover target")

	// Each slot should have cheapest hour(s) for its window
	for _, slot := range sched.Slots {
		assert.NotEmpty(t, slot.Periods)
		assert.NotEmpty(t, slot.Date)
	}
}

// TestComputeScheduleSpillsToTomorrowWhenTodayBlockedByMaxPrice covers the
// late/missed plug-in case: today's window has rates but all are above
// MaxPrice, so today's slot is empty and the full target must spill into
// tomorrow's deadline window.
func TestComputeScheduleSpillsToTomorrowWhenTodayBlockedByMaxPrice(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	firstDeadline := now.Add(4 * time.Hour)

	rates := make([]tariff.Rate, 28)
	for i := 0; i < 4; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: 1.50, // all above MaxPrice
		}
	}
	for i := 4; i < 28; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: 0.40,
		}
	}

	cfg := Config{
		TargetEnergy:   22, // ceil(22*1.03/11) = 3 hours
		Deadline:       firstDeadline,
		MaxPower:       11000,
		MaxPrice:       1.00,
		ChargeHeadroom: 3,
	}

	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched, "should produce a schedule by spilling into tomorrow")
	require.Len(t, sched.Slots, 1, "today's window had no eligible rates; only tomorrow's slot should appear")
	assert.GreaterOrEqual(t, sched.Slots[0].Energy, 22.0, "tomorrow's slot should cover the full target")
	assert.True(t, sched.Slots[0].Deadline.After(firstDeadline), "spilled slot should target the next deadline")
}

// TestComputeScheduleCapsAtTwoCycles ensures we don't iterate beyond
// tomorrow even when remainingEnergy is still positive after two windows.
func TestComputeScheduleCapsAtTwoCycles(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	firstDeadline := now.Add(2 * time.Hour)

	// Rates for 4 days, all expensive enough that one slot can't fit a huge target.
	rates := make([]tariff.Rate, 96)
	for i := 0; i < 96; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: 0.50,
		}
	}

	cfg := Config{
		TargetEnergy:   500, // unrealistic huge target; would otherwise iterate 4 days
		Deadline:       firstDeadline,
		MaxPower:       11000,
		ChargeHeadroom: 3,
	}

	sched := ComputeSchedule(rates, cfg, 11000)
	require.NotNil(t, sched)
	assert.LessOrEqual(t, len(sched.Slots), 2, "should never plan beyond tomorrow")
}

// --- mergeActiveSlotPreservingActive ---

func mockNow(t *testing.T, at time.Time) {
	t.Helper()
	orig := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = orig })
}

// Mid-session, the next adjacent recomputed hour is appended as a separate
// hourly period — the active period's bounds are never extended. OCPP-level
// consolidation (BuildProfile) is what makes adjacent same-power periods
// charge as one continuous block.
func TestMergeActiveSlotKeepsHourlyAdjacent(t *testing.T) {
	day := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	now := day.Add(2*time.Hour + 30*time.Minute) // 02:30, mid-active-period
	mockNow(t, now)

	active := SchedulePeriod{
		Start: day.Add(2 * time.Hour),
		End:   day.Add(3 * time.Hour),
		Power: 11000, Price: 0.30,
	}
	inProgress := ScheduleSlot{
		Date:     "2026-05-06",
		Deadline: day.Add(7 * time.Hour),
		Periods:  []SchedulePeriod{active},
	}

	// Recomputed today picks 03:00–04:00 (adjacent to active.End=03:00).
	recomputed := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:     "2026-05-06",
				Deadline: day.Add(7 * time.Hour),
				Periods: []SchedulePeriod{
					{Start: day.Add(3 * time.Hour), End: day.Add(4 * time.Hour), Power: 11000, Price: 0.40},
				},
			},
		},
	}

	merged := mergeActiveSlotPreservingActive(recomputed, cloneSlot(inProgress))
	require.NotNil(t, merged)
	require.Len(t, merged.Slots, 1)
	require.Len(t, merged.Slots[0].Periods, 2, "hourly periods stay separate")

	a := merged.Slots[0].Periods[0]
	assert.Equal(t, active.Start, a.Start, "active Start unchanged")
	assert.Equal(t, active.End, a.End, "active End unchanged (no extension)")
	assert.Equal(t, float64(11000), a.Power)
	assert.InDelta(t, 0.30, a.Price, 0.001, "active price unchanged")

	b := merged.Slots[0].Periods[1]
	assert.Equal(t, day.Add(3*time.Hour), b.Start)
	assert.Equal(t, day.Add(4*time.Hour), b.End)
	assert.InDelta(t, 0.40, b.Price, 0.001, "appended period keeps its own price")
}

// Mid-session with no adjacent recomputed hour — non-adjacent later periods
// are appended as separate windows; active period left intact.
func TestMergeActiveSlotAppendsNonAdjacent(t *testing.T) {
	day := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	now := day.Add(2*time.Hour + 30*time.Minute)
	mockNow(t, now)

	active := SchedulePeriod{
		Start: day.Add(2 * time.Hour),
		End:   day.Add(3 * time.Hour),
		Power: 11000, Price: 0.30,
	}
	inProgress := ScheduleSlot{
		Date:     "2026-05-06",
		Deadline: day.Add(7 * time.Hour),
		Periods:  []SchedulePeriod{active},
	}

	recomputed := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:     "2026-05-06",
				Deadline: day.Add(7 * time.Hour),
				Periods: []SchedulePeriod{
					{Start: day.Add(5 * time.Hour), End: day.Add(6 * time.Hour), Power: 11000, Price: 0.20},
				},
			},
		},
	}

	merged := mergeActiveSlotPreservingActive(recomputed, cloneSlot(inProgress))
	require.NotNil(t, merged)
	require.Len(t, merged.Slots, 1)
	require.Len(t, merged.Slots[0].Periods, 2)

	assert.Equal(t, active.Start, merged.Slots[0].Periods[0].Start)
	assert.Equal(t, active.End, merged.Slots[0].Periods[0].End, "active End preserved (no adjacent)")
	assert.Equal(t, day.Add(5*time.Hour), merged.Slots[0].Periods[1].Start)
}

// Mid-session, recompute today is empty (nothing eligible). Active period
// preserved verbatim; tomorrow's slot stays.
func TestMergeActiveSlotEmptyRecomputeToday(t *testing.T) {
	day := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	now := day.Add(2*time.Hour + 30*time.Minute)
	mockNow(t, now)

	active := SchedulePeriod{
		Start: day.Add(2 * time.Hour),
		End:   day.Add(3 * time.Hour),
		Power: 11000, Price: 0.30,
	}
	inProgress := ScheduleSlot{
		Date:     "2026-05-06",
		Deadline: day.Add(7 * time.Hour),
		Periods:  []SchedulePeriod{active},
	}

	tomorrow := ScheduleSlot{
		Date:     "2026-05-07",
		Deadline: day.Add(31 * time.Hour),
		Periods: []SchedulePeriod{
			{Start: day.Add(26 * time.Hour), End: day.Add(28 * time.Hour), Power: 11000, Price: 0.25},
		},
	}
	recomputed := &Schedule{Slots: []ScheduleSlot{tomorrow}}

	merged := mergeActiveSlotPreservingActive(recomputed, cloneSlot(inProgress))
	require.NotNil(t, merged)
	require.Len(t, merged.Slots, 2)

	today := merged.Slots[0]
	require.Len(t, today.Periods, 1)
	assert.Equal(t, active, today.Periods[0], "active preserved verbatim when no recomputed today-slot")

	tom := merged.Slots[1]
	assert.Equal(t, "2026-05-07", tom.Date)
	require.Len(t, tom.Periods, 1)
}

// When the recomputed period overlaps the active period (would-be conflict),
// it must be filtered out — the active period owns its time range exclusively.
// Adjacent recomputed periods are appended as separate hourly periods.
func TestMergeActiveSlotIgnoresOverlappingRecomputedPeriods(t *testing.T) {
	day := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	now := day.Add(2*time.Hour + 30*time.Minute)
	mockNow(t, now)

	active := SchedulePeriod{
		Start: day.Add(2 * time.Hour),
		End:   day.Add(4 * time.Hour),
		Power: 11000, Price: 0.30,
	}
	inProgress := ScheduleSlot{
		Date:     "2026-05-06",
		Deadline: day.Add(7 * time.Hour),
		Periods:  []SchedulePeriod{active},
	}

	// Recomputed picks 03:00-04:00 (overlaps active) and 04:00-05:00 (adjacent).
	recomputed := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:     "2026-05-06",
				Deadline: day.Add(7 * time.Hour),
				Periods: []SchedulePeriod{
					{Start: day.Add(3 * time.Hour), End: day.Add(4 * time.Hour), Power: 11000, Price: 0.10},
					{Start: day.Add(4 * time.Hour), End: day.Add(5 * time.Hour), Power: 11000, Price: 0.40},
				},
			},
		},
	}

	merged := mergeActiveSlotPreservingActive(recomputed, cloneSlot(inProgress))
	require.NotNil(t, merged)
	require.Len(t, merged.Slots, 1)
	require.Len(t, merged.Slots[0].Periods, 2, "overlapping dropped; adjacent appended as separate period")
	a := merged.Slots[0].Periods[0]
	assert.Equal(t, active.Start, a.Start)
	assert.Equal(t, active.End, a.End, "active End unchanged")
	b := merged.Slots[0].Periods[1]
	assert.Equal(t, day.Add(4*time.Hour), b.Start)
	assert.Equal(t, day.Add(5*time.Hour), b.End)
	assert.InDelta(t, 0.40, b.Price, 0.001)
}

// The user's reported orphan scenario: a previous recompute extended the
// active period into adjacent hours, leaving in.Periods with both the
// extended period AND the original hourly period of the absorbed hour.
// The new hourly model should produce two clean hourly periods, no orphan.
func TestMergeActiveSlotNoOrphanWhenInHasMultipleHours(t *testing.T) {
	day := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	now := day.Add(14*time.Hour + 30*time.Minute) // 14:30
	mockNow(t, now)

	inProgress := ScheduleSlot{
		Date:     "2026-05-06",
		Deadline: day.Add(31 * time.Hour),
		Periods: []SchedulePeriod{
			{Start: day.Add(14 * time.Hour), End: day.Add(15 * time.Hour), Power: 11000, Price: 0.30},
			{Start: day.Add(15 * time.Hour), End: day.Add(16 * time.Hour), Power: 11000, Price: 0.40},
		},
	}

	recomputed := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:     "2026-05-06",
				Deadline: day.Add(31 * time.Hour),
				Periods: []SchedulePeriod{
					{Start: day.Add(14 * time.Hour), End: day.Add(15 * time.Hour), Power: 11000, Price: 0.30},
					{Start: day.Add(15 * time.Hour), End: day.Add(16 * time.Hour), Power: 11000, Price: 0.40},
				},
			},
		},
	}

	merged := mergeActiveSlotPreservingActive(recomputed, cloneSlot(inProgress))
	require.NotNil(t, merged)
	require.Len(t, merged.Slots, 1)
	require.Len(t, merged.Slots[0].Periods, 2, "no orphan: hourly periods stay separate")
	assert.Equal(t, day.Add(14*time.Hour), merged.Slots[0].Periods[0].Start)
	assert.Equal(t, day.Add(15*time.Hour), merged.Slots[0].Periods[0].End)
	assert.Equal(t, day.Add(15*time.Hour), merged.Slots[0].Periods[1].Start)
	assert.Equal(t, day.Add(16*time.Hour), merged.Slots[0].Periods[1].End)
	// Energy: 2 hours × 11 kW = 22 kWh (not 33 — that was the orphan double-count bug).
	assert.InDelta(t, 22.0, merged.Slots[0].Energy, 0.01)
}

// A previous schedule had post-active hours that the latest recompute no
// longer picks. Those stale post-active hours are dropped — recompute owns
// the post-active future.
func TestMergeActiveSlotDropsStalePostActive(t *testing.T) {
	day := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	now := day.Add(14*time.Hour + 30*time.Minute) // 14:30
	mockNow(t, now)

	inProgress := ScheduleSlot{
		Date:     "2026-05-06",
		Deadline: day.Add(31 * time.Hour),
		Periods: []SchedulePeriod{
			{Start: day.Add(14 * time.Hour), End: day.Add(15 * time.Hour), Power: 11000, Price: 0.30},
			{Start: day.Add(15 * time.Hour), End: day.Add(16 * time.Hour), Power: 11000, Price: 0.40},
			{Start: day.Add(16 * time.Hour), End: day.Add(17 * time.Hour), Power: 11000, Price: 0.50},
		},
	}

	// Recompute now only picks 15-16; 16-17 should be dropped from the result.
	recomputed := &Schedule{
		Slots: []ScheduleSlot{
			{
				Date:     "2026-05-06",
				Deadline: day.Add(31 * time.Hour),
				Periods: []SchedulePeriod{
					{Start: day.Add(15 * time.Hour), End: day.Add(16 * time.Hour), Power: 11000, Price: 0.40},
				},
			},
		},
	}

	merged := mergeActiveSlotPreservingActive(recomputed, cloneSlot(inProgress))
	require.NotNil(t, merged)
	require.Len(t, merged.Slots, 1)
	require.Len(t, merged.Slots[0].Periods, 2, "stale 16-17 dropped; recompute owns post-active future")
	assert.Equal(t, day.Add(14*time.Hour), merged.Slots[0].Periods[0].Start)
	assert.Equal(t, day.Add(15*time.Hour), merged.Slots[0].Periods[0].End)
	assert.Equal(t, day.Add(15*time.Hour), merged.Slots[0].Periods[1].Start)
	assert.Equal(t, day.Add(16*time.Hour), merged.Slots[0].Periods[1].End)
}

func TestNextDeadline(t *testing.T) {
	d := nextDeadline("07:00")
	assert.Equal(t, 7, d.Hour())
	assert.Equal(t, 0, d.Minute())
	assert.True(t, d.After(time.Now()) || d.Equal(time.Now()))
}

// --- persistence ---

// saveCurrent encodes s.current to settings; loadPersistedSchedule restores
// it. After a "restart" (new Scheduler against the same store) the schedule
// must come back intact.
func TestPersistedScheduleSurvivesRestart(t *testing.T) {
	mock := &mockCharger{}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	original := &Schedule{
		Slots: []ScheduleSlot{{
			Date:     now.Format("2006-01-02"),
			Deadline: now.Add(8 * time.Hour),
			Periods: []SchedulePeriod{
				{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000, Price: 0.30},
				{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Power: 11000, Price: 0.40},
			},
			Cost:   7.70,
			Energy: 22,
		}},
		Cost:     7.70,
		Energy:   22,
		Deadline: now.Add(8 * time.Hour),
	}
	s.mu.Lock()
	s.current = original
	s.mu.Unlock()
	s.saveCurrent()

	// Simulate restart: build a fresh Scheduler against the same store.
	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()

	require.NotNil(t, restored.current, "schedule must be restored")
	require.Len(t, restored.current.Slots, 1)
	require.Len(t, restored.current.Slots[0].Periods, 2)
	assert.Equal(t, original.Slots[0].Periods[0].Start.Unix(), restored.current.Slots[0].Periods[0].Start.Unix())
	assert.Equal(t, original.Slots[0].Periods[1].End.Unix(), restored.current.Slots[0].Periods[1].End.Unix())
	assert.Equal(t, 11000.0, restored.current.Slots[0].Periods[0].Power)
}

// A persisted slot whose every period is in the past should not be restored.
func TestPersistedSchedulePrunesPastSlots(t *testing.T) {
	mock := &mockCharger{}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	original := &Schedule{
		Slots: []ScheduleSlot{
			// Past slot — entirely in the past.
			{
				Date: now.Add(-24 * time.Hour).Format("2006-01-02"),
				Periods: []SchedulePeriod{
					{Start: now.Add(-25 * time.Hour), End: now.Add(-24 * time.Hour), Power: 11000},
				},
			},
			// Future slot — keep.
			{
				Date: now.Format("2006-01-02"),
				Periods: []SchedulePeriod{
					{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
				},
			},
		},
	}
	s.mu.Lock()
	s.current = original
	s.mu.Unlock()
	s.saveCurrent()

	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()

	require.NotNil(t, restored.current)
	require.Len(t, restored.current.Slots, 1, "past slot pruned, future slot kept")
	assert.Equal(t, now.Format("2006-01-02"), restored.current.Slots[0].Date)
}

// A persisted schedule with NO live periods should result in s.current == nil.
func TestPersistedScheduleAllPastResultsNil(t *testing.T) {
	mock := &mockCharger{}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	original := &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Add(-24 * time.Hour).Format("2006-01-02"),
			Periods: []SchedulePeriod{
				{Start: now.Add(-25 * time.Hour), End: now.Add(-24 * time.Hour), Power: 11000},
			},
		}},
	}
	s.mu.Lock()
	s.current = original
	s.mu.Unlock()
	s.saveCurrent()

	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()

	assert.Nil(t, restored.current, "all-past schedule must not be restored")
}

// ClearSchedule must persist the empty state so a stale schedule isn't
// restored on next startup.
func TestClearSchedulePersistsEmpty(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Available", txnID: 0}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	s.mu.Lock()
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{
				{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
			},
		}},
	}
	s.mu.Unlock()
	s.saveCurrent()

	// Verify it persisted.
	require.NotEmpty(t, st.GetDefault(persistedScheduleKey, ""))

	s.ClearSchedule()

	// Now should be empty.
	assert.Empty(t, st.GetDefault(persistedScheduleKey, ""), "ClearSchedule must clear persisted blob")

	// And a "restart" should not restore anything.
	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()
	assert.Nil(t, restored.current)
}

// Garbage in the persisted blob must not crash; loadPersistedSchedule
// should log a warning and continue with no schedule.
func TestPersistedScheduleHandlesCorruptBlob(t *testing.T) {
	mock := &mockCharger{}
	_, st := newTestScheduler(t, mock)
	require.NoError(t, st.Set(persistedScheduleKey, "{not valid json"))

	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()
	assert.Nil(t, restored.current)
}
