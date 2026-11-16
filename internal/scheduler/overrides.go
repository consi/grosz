package scheduler

import (
	"math"
	"sort"
	"time"

	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

// splitOverrides separates loaded overrides by Kind. Used by recompute.
func splitOverrides(all []store.ScheduleOverride) (forces, blocks []store.ScheduleOverride) {
	for _, o := range all {
		switch o.Kind {
		case store.OverrideKindForce:
			forces = append(forces, o)
		case store.OverrideKindBlock:
			blocks = append(blocks, o)
		}
	}
	return forces, blocks
}

// buildForcePeriods materializes user-force overrides as SchedulePeriod
// entries with cost priced against the supplied tariff rates. Each period's
// Price is the volume-weighted average PLN/kWh over its window; Price=0 if
// no rates cover any of the window.
func buildForcePeriods(forces []store.ScheduleOverride, rates []tariff.Rate) []SchedulePeriod {
	out := make([]SchedulePeriod, 0, len(forces))
	for _, f := range forces {
		cost, energy := priceOverPeriod(rates, f.Start, f.End, f.PowerW)
		var price float64
		if energy > 0 {
			price = cost / energy
		}
		out = append(out, SchedulePeriod{
			Start:  f.Start,
			End:    f.End,
			Power:  f.PowerW,
			Price:  price,
			Source: sourceUserForce,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// totalForceEnergy sums the energy (kWh) of all force periods.
func totalForceEnergy(periods []SchedulePeriod) float64 {
	var sum float64
	for _, p := range periods {
		sum += p.Power / 1000 * p.End.Sub(p.Start).Hours()
	}
	return sum
}

// recalcSlotTotals recomputes Cost and Energy for a slot from its periods.
func recalcSlotTotals(slot *ScheduleSlot) {
	var cost, energy float64
	for _, p := range slot.Periods {
		hrs := p.End.Sub(p.Start).Hours()
		e := p.Power / 1000 * hrs
		energy += e
		cost += p.Price * e
	}
	slot.Cost = math.Round(cost*100) / 100
	slot.Energy = math.Round(energy*100) / 100
}

// nextDeadlineAfter returns the next occurrence of HH:MM at or after t in loc.
// If t is exactly at HH:MM today, returns the same instant; if t is past HH:MM,
// returns tomorrow's HH:MM.
func nextDeadlineAfter(t time.Time, h, m int, loc *time.Location) time.Time {
	t = t.In(loc)
	candidate := time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, loc)
	if !candidate.After(t) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}
