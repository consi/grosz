package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

func mkRate(start time.Time, hours int, price float64) tariff.Rate {
	return tariff.Rate{Start: start, End: start.Add(time.Duration(hours) * time.Hour), Price: price}
}

func TestFragmentRates_NoOverrides(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		mkRate(now, 1, 0.30),
		mkRate(now.Add(time.Hour), 1, 0.40),
	}
	got := fragmentRates(rates, nil)
	assert.Equal(t, rates, got)
}

func TestFragmentRates_FullHourBlock(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		mkRate(now, 1, 0.30),
		mkRate(now.Add(time.Hour), 1, 0.40),
		mkRate(now.Add(2*time.Hour), 1, 0.50),
	}
	overrides := []store.ScheduleOverride{
		{Kind: store.OverrideKindBlock, Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)},
	}
	got := fragmentRates(rates, overrides)
	require.Len(t, got, 2)
	assert.Equal(t, now, got[0].Start)
	assert.InDelta(t, 0.30, got[0].Price, 0.001)
	assert.Equal(t, now.Add(2*time.Hour), got[1].Start)
	assert.InDelta(t, 0.50, got[1].Price, 0.001)
}

func TestFragmentRates_HalfHourBlockAtBoundary(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		mkRate(now, 1, 0.30),
	}
	overrides := []store.ScheduleOverride{
		{Kind: store.OverrideKindBlock, Start: now.Add(30 * time.Minute), End: now.Add(time.Hour)},
	}
	got := fragmentRates(rates, overrides)
	require.Len(t, got, 1)
	assert.Equal(t, now, got[0].Start)
	assert.Equal(t, now.Add(30*time.Minute), got[0].End)
	assert.InDelta(t, 0.30, got[0].Price, 0.001)
}

func TestFragmentRates_BlockCrossingHour(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		mkRate(now, 1, 0.30),
		mkRate(now.Add(time.Hour), 1, 0.40),
	}
	overrides := []store.ScheduleOverride{
		{Kind: store.OverrideKindBlock, Start: now.Add(30 * time.Minute), End: now.Add(90 * time.Minute)},
	}
	got := fragmentRates(rates, overrides)
	require.Len(t, got, 2)
	assert.Equal(t, now, got[0].Start)
	assert.Equal(t, now.Add(30*time.Minute), got[0].End)
	assert.InDelta(t, 0.30, got[0].Price, 0.001)
	assert.Equal(t, now.Add(90*time.Minute), got[1].Start)
	assert.Equal(t, now.Add(2*time.Hour), got[1].End)
	assert.InDelta(t, 0.40, got[1].Price, 0.001)
}

func TestFragmentRates_ForceStraddlesTwoHours(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		mkRate(now, 1, 0.30),
		mkRate(now.Add(time.Hour), 1, 0.40),
	}
	overrides := []store.ScheduleOverride{
		{Kind: store.OverrideKindForce, PowerW: 11000,
			Start: now.Add(45 * time.Minute), End: now.Add(75 * time.Minute)},
	}
	got := fragmentRates(rates, overrides)
	require.Len(t, got, 2)
	assert.Equal(t, now.Add(45*time.Minute), got[0].End)
	assert.Equal(t, now.Add(75*time.Minute), got[1].Start)
}

func TestFragmentRates_MultipleOverridesInOneHour(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{mkRate(now, 1, 0.30)}
	overrides := []store.ScheduleOverride{
		{Kind: store.OverrideKindBlock, Start: now.Add(10 * time.Minute), End: now.Add(20 * time.Minute)},
		{Kind: store.OverrideKindBlock, Start: now.Add(40 * time.Minute), End: now.Add(50 * time.Minute)},
	}
	got := fragmentRates(rates, overrides)
	require.Len(t, got, 3)
	assert.Equal(t, now, got[0].Start)
	assert.Equal(t, now.Add(10*time.Minute), got[0].End)
	assert.Equal(t, now.Add(20*time.Minute), got[1].Start)
	assert.Equal(t, now.Add(40*time.Minute), got[1].End)
	assert.Equal(t, now.Add(50*time.Minute), got[2].Start)
	assert.Equal(t, now.Add(time.Hour), got[2].End)
}

func TestFragmentRates_OverridePastTariffHorizon(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{mkRate(now, 1, 0.30)}
	overrides := []store.ScheduleOverride{
		{Kind: store.OverrideKindForce, PowerW: 11000,
			Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)},
	}
	got := fragmentRates(rates, overrides)
	assert.Equal(t, rates, got)
}

func TestFragmentRates_OverlappingOverrides(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{mkRate(now, 1, 0.30)}
	overrides := []store.ScheduleOverride{
		{Kind: store.OverrideKindBlock, Start: now.Add(10 * time.Minute), End: now.Add(40 * time.Minute)},
		{Kind: store.OverrideKindBlock, Start: now.Add(30 * time.Minute), End: now.Add(50 * time.Minute)},
	}
	got := fragmentRates(rates, overrides)
	require.Len(t, got, 2)
	assert.Equal(t, now.Add(10*time.Minute), got[0].End)
	assert.Equal(t, now.Add(50*time.Minute), got[1].Start)
}

func TestPriceOverPeriod_FullHour(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{mkRate(now, 1, 0.30)}

	cost, energy := priceOverPeriod(rates, now, now.Add(time.Hour), 11000)
	assert.InDelta(t, 11.0, energy, 0.001)
	assert.InDelta(t, 11.0*0.30, cost, 0.001)
}

func TestPriceOverPeriod_SubHour(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{mkRate(now, 1, 0.40)}

	cost, energy := priceOverPeriod(rates, now.Add(15*time.Minute), now.Add(45*time.Minute), 11000)
	assert.InDelta(t, 5.5, energy, 0.001)
	assert.InDelta(t, 5.5*0.40, cost, 0.001)
}

func TestPriceOverPeriod_AcrossTwoHours(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		mkRate(now, 1, 0.30),
		mkRate(now.Add(time.Hour), 1, 0.50),
	}

	cost, energy := priceOverPeriod(rates, now.Add(45*time.Minute), now.Add(75*time.Minute), 11000)
	assert.InDelta(t, 5.5, energy, 0.001)
	expected := (11.0*0.25)*0.30 + (11.0*0.25)*0.50
	assert.InDelta(t, expected, cost, 0.001)
}

func TestPriceOverPeriod_PastTariffHorizon(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{mkRate(now, 1, 0.30)}

	cost, energy := priceOverPeriod(rates, now.Add(2*time.Hour), now.Add(3*time.Hour), 11000)
	assert.InDelta(t, 11.0, energy, 0.001)
	assert.InDelta(t, 0, cost, 0.001)
}
