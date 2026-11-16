package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

var waw, _ = time.LoadLocation("Europe/Warsaw")

// hourly rates from `from` with the given prices.
func hourly(from time.Time, prices ...float64) []tariff.Rate {
	out := make([]tariff.Rate, len(prices))
	for i, p := range prices {
		s := from.Add(time.Duration(i) * time.Hour)
		out[i] = tariff.Rate{Start: s, End: s.Add(time.Hour), Price: p}
	}
	return out
}

func baseInput(now time.Time, rates []tariff.Rate, soc float64) PlanInput {
	return PlanInput{
		Now: now, Loc: waw, Rates: rates, SoC: soc, BatteryKWh: 52, Efficiency: 0.9, PowerW: 10800,
		NormalTarget: 80, CarLimit: 80, Floor: 15, Safe: 50, DepartureH: 7,
		PluggedNow: true, DailyKWh: 6,
	}
}

func starts(s *Schedule) []int {
	var out []int
	for _, p := range s.ActivePeriods() {
		out = append(out, p.Start.In(waw).Hour())
	}
	return out
}

// 18:00, prices until tomorrow 24:00 known (midday tomorrow cheapest).
func eveningRates() (time.Time, []tariff.Rate) {
	now := time.Date(2026, 11, 16, 18, 0, 0, 0, waw)
	night := []float64{1.6, 1.5, 0.99, 1.02, 0.97, 1.11, 1.13, 1.12, 1.14, 1.16, 1.10, 1.35, 1.3}            // 18..06
	day := []float64{1.2, 1.1, 1.0, 0.40, 0.35, 0.38, 0.6, 0.9, 1.3, 1.6, 1.7, 1.6, 1.5, 1.4, 1.2, 1.1, 1.0} // 07..23
	return now, hourly(now, append(night, day...)...)
}

func TestPlan_BelowSafeChargesBeforeDeparture(t *testing.T) {
	now, rates := eveningRates()
	s, mode := Plan(baseInput(now, rates, 45))
	require.NotNil(t, s)
	assert.Equal(t, ModeCharge, mode)
	for _, p := range s.ActivePeriods() {
		assert.True(t, p.Start.Before(time.Date(2026, 11, 17, 7, 0, 0, 0, waw)), "must finish before departure")
		assert.Equal(t, ReasonNeeded, p.Reason)
	}
	assert.Equal(t, 80.0, s.Target)
}

func TestPlan_SafeSoCWaitsForCheaperMidday(t *testing.T) {
	now, rates := eveningRates()
	s, mode := Plan(baseInput(now, rates, 65))
	require.NotNil(t, s)
	assert.Equal(t, ModeWait, mode)
	assert.Equal(t, []int{11}, starts(s), "tomorrow's cheap midday hour instead of tonight")
}

func TestPlan_TripTargetMustBeReadyByDeparture(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 75)
	in.CarLimit = 100
	s, mode := Plan(in)
	require.NotNil(t, s)
	assert.Equal(t, ModeTrip, mode)
	assert.Equal(t, 100.0, s.Target)
	for _, p := range s.ActivePeriods() {
		assert.True(t, p.Start.Before(time.Date(2026, 11, 17, 7, 0, 0, 0, waw)))
		assert.Equal(t, ReasonTrip, p.Reason)
	}
}

func TestPlan_RaisedForFillIsNotATrip(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 65)
	in.CarLimit, in.FillRaised = 100, true
	_, mode := Plan(in)
	assert.Equal(t, ModeWait, mode)
}

func TestPlan_BelowFloorChargesImmediately(t *testing.T) {
	now, rates := eveningRates()
	s, mode := Plan(baseInput(now, rates, 10))
	require.NotNil(t, s)
	assert.Equal(t, ModeFloor, mode)
	assert.Equal(t, 18, starts(s)[0], "starts now despite 1.60 PLN")
}

func TestPlan_NegativeAlwaysChargesEvenWhenFull(t *testing.T) {
	now := time.Date(2026, 5, 10, 9, 0, 0, 0, waw)
	in := baseInput(now, hourly(now, 0.2, -0.1, 0.3), 79.5)
	s, mode := Plan(in)
	require.NotNil(t, s)
	assert.Equal(t, ModeFull, mode)
	assert.Equal(t, []int{10}, starts(s))
	assert.Equal(t, ReasonNegative, s.ActivePeriods()[0].Reason)
}

func TestPlan_NegativePrefersTheBestHourWhenRoomIsLimited(t *testing.T) {
	now := time.Date(2026, 5, 10, 11, 0, 0, 0, waw)
	// Room ~1 hour: today's -0.05 vs tomorrow's -0.40 at 12:00.
	prices := make([]float64, 26)
	for i := range prices {
		prices[i] = 0.5
	}
	prices[1] = -0.05 // today 12:00
	prices[25] = -0.40
	in := baseInput(now, hourly(now, prices...), 70)
	in.DailyKWh = 0
	s, _ := Plan(in)
	require.NotNil(t, s)
	var neg []time.Time
	for _, p := range s.ActivePeriods() {
		if p.Reason == ReasonNegative {
			neg = append(neg, p.Start)
		}
	}
	require.Len(t, neg, 1)
	assert.Equal(t, now.Add(25*time.Hour), neg[0], "keep the headroom for tomorrow's better price")
}

func TestPlan_CheapFillAboveTarget(t *testing.T) {
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, waw)
	in := baseInput(now, hourly(now, 0.3, 0.6), 80)
	in.CheapFillPrice, in.CheapFillSoC = 0.45, 100
	s, _ := Plan(in)
	require.NotNil(t, s)
	p := s.ActivePeriods()[0]
	assert.Equal(t, ReasonCheapFill, p.Reason)
	assert.Equal(t, 100.0, p.FillSoC)
	assert.Len(t, s.ActivePeriods(), 1)
}

func TestPlan_UnpluggedUsesPlugPattern(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 65)
	in.PluggedNow = false
	// Usually plugged only at night (20:00-07:00).
	in.PlugPattern = map[[2]int]float64{}
	for wd := 0; wd < 7; wd++ {
		for h := 0; h < 24; h++ {
			v := 0.0
			if h >= 20 || h < 7 {
				v = 0.9
			}
			in.PlugPattern[[2]int{wd, h}] = v
		}
	}
	s, _ := Plan(in)
	require.NotNil(t, s)
	for _, h := range starts(s) {
		assert.True(t, h >= 20 || h < 7, "midday is cheaper but the car is usually away: hour %d", h)
	}
}

// The reported case: 21:00 (0.987) and 23:00 (0.974) were picked with a
// 1.024 gap at 22:00. Bridging costs ~0.4 PLN, so it becomes one block.
func TestPlan_GapFillMakesOneBlock(t *testing.T) {
	now := time.Date(2026, 11, 16, 19, 0, 0, 0, waw)
	in := baseInput(now, hourly(now, 1.54, 1.60, 0.987, 1.024, 0.974, 1.114, 1.127), 61)
	in.Safe = 70
	in.DepartureH = 2
	s, _ := Plan(in)
	require.NotNil(t, s)
	h := starts(s)
	require.Len(t, h, 2)
	assert.Equal(t, 1, h[1]-h[0], "adjacent hours: %v", h)
}

func TestPlan_StickyKeepsPreviousPlanWhenNotMeaningfullyCheaper(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 45)
	first, _ := Plan(in)
	require.NotNil(t, first)
	// A tiny price change elsewhere and an SoC tick must not reshuffle.
	rates2 := append([]tariff.Rate(nil), rates...)
	rates2[5].Price -= 0.02
	in2 := baseInput(now.Add(5*time.Minute), rates2, 46)
	in2.Prev = first
	second, _ := Plan(in2)
	require.NotNil(t, second)
	assert.Equal(t, starts(first), starts(second))
}

func TestPlan_OverridesChangeTheSignature(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 45)
	first, _ := Plan(in)
	in2 := baseInput(now, rates, 45)
	in2.Prev = first
	in2.Blocks = []store.ScheduleOverride{{Kind: store.OverrideKindBlock, Start: first.ActivePeriods()[0].Start, End: first.ActivePeriods()[0].End}}
	second, _ := Plan(in2)
	require.NotNil(t, second)
	assert.NotEqual(t, starts(first)[0], starts(second)[0], "a new block window must re-plan")
}

func TestPlan_MaxPriceOnlyWhenWaiting(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 45)
	in.MaxPrice = 0.5
	s, _ := Plan(in)
	require.NotNil(t, s, "below safe: max price is ignored")
}

func TestPlan_FullWithPositivePricesPlansNothing(t *testing.T) {
	now, rates := eveningRates()
	s, mode := Plan(baseInput(now, rates, 80))
	assert.Nil(t, s)
	assert.Equal(t, ModeFull, mode)
}

// Regression 2026-11-16: an unpriced (placeholder) hour removed from the
// price list must not survive in the plan through stickiness.
func TestPlan_StickyDropsHoursNoLongerPriced(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 45)
	first, _ := Plan(in)
	require.NotNil(t, first)
	gone := first.ActivePeriods()[0].Start
	var rates2 []tariff.Rate
	for _, r := range rates {
		if !r.Start.Equal(gone) {
			rates2 = append(rates2, r)
		}
	}
	in2 := baseInput(now, rates2, 45)
	in2.Prev = first
	second, _ := Plan(in2)
	require.NotNil(t, second)
	for _, p := range second.ActivePeriods() {
		assert.NotEqual(t, gone, p.Start)
	}
}

// Must-charge with the car away and no usual plug-in hours tonight: still
// show the cheapest hours before departure instead of an empty plan.
func TestPlan_MustChargeFallsBackWhenUsualHoursMissing(t *testing.T) {
	now, rates := eveningRates()
	in := baseInput(now, rates, 45)
	in.PluggedNow = false
	in.PlugPattern = map[[2]int]float64{{0, 0}: 0} // history exists, never plugged
	s, mode := Plan(in)
	require.NotNil(t, s)
	assert.Equal(t, ModeCharge, mode)
	assert.NotEmpty(t, s.ActivePeriods())
}
