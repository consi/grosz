package scheduler

import (
	"math"
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

func TestNextDeadline(t *testing.T) {
	d := nextDeadline("07:00")
	assert.Equal(t, 7, d.Hour())
	assert.Equal(t, 0, d.Minute())
	assert.True(t, d.After(time.Now()) || d.Equal(time.Now()))
}
