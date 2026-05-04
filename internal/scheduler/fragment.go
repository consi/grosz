package scheduler

import (
	"sort"
	"time"

	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

// fragmentRates returns the input tariff rates with all override windows
// (both force and block) carved out. Each surviving fragment keeps the
// original rate's Price; durations may shrink. Rates are returned in start-time
// order. Fragments shorter than 1 second are dropped.
//
// The reason force overrides are also carved out: their hours are already
// committed to charge, so the auto-selector must not double-allocate energy
// to them.
func fragmentRates(rates []tariff.Rate, overrides []store.ScheduleOverride) []tariff.Rate {
	if len(overrides) == 0 {
		out := make([]tariff.Rate, len(rates))
		copy(out, rates)
		return out
	}

	type interval struct {
		start, end time.Time
	}
	carve := make([]interval, 0, len(overrides))
	for _, o := range overrides {
		carve = append(carve, interval{o.Start, o.End})
	}
	sort.Slice(carve, func(i, j int) bool { return carve[i].start.Before(carve[j].start) })

	merged := carve[:0]
	for _, c := range carve {
		if len(merged) == 0 || c.start.After(merged[len(merged)-1].end) {
			merged = append(merged, c)
			continue
		}
		if c.end.After(merged[len(merged)-1].end) {
			merged[len(merged)-1].end = c.end
		}
	}

	var out []tariff.Rate
	for _, r := range rates {
		segments := []interval{{r.Start, r.End}}
		for _, c := range merged {
			if !c.end.After(r.Start) || !c.start.Before(r.End) {
				continue
			}
			var next []interval
			for _, seg := range segments {
				if !c.end.After(seg.start) || !c.start.Before(seg.end) {
					next = append(next, seg)
					continue
				}
				if seg.start.Before(c.start) {
					next = append(next, interval{seg.start, c.start})
				}
				if c.end.Before(seg.end) {
					next = append(next, interval{c.end, seg.end})
				}
			}
			segments = next
		}
		for _, seg := range segments {
			if seg.end.Sub(seg.start) < time.Second {
				continue
			}
			out = append(out, tariff.Rate{Start: seg.start, End: seg.end, Price: r.Price})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// priceOverPeriod returns the cost (PLN) and energy (kWh) charged at the
// given power across [start, end), priced by overlapping tariff rates. Time
// outside any rate contributes energy but no cost (caller can detect partial
// coverage by comparing the returned energy to chargePowerKW * duration).
func priceOverPeriod(rates []tariff.Rate, start, end time.Time, chargePowerW float64) (cost, energy float64) {
	if !end.After(start) {
		return 0, 0
	}
	powerKW := chargePowerW / 1000
	totalEnergy := powerKW * end.Sub(start).Hours()
	for _, r := range rates {
		overlapStart := r.Start
		if start.After(overlapStart) {
			overlapStart = start
		}
		overlapEnd := r.End
		if end.Before(overlapEnd) {
			overlapEnd = end
		}
		hours := overlapEnd.Sub(overlapStart).Hours()
		if hours <= 0 {
			continue
		}
		cost += powerKW * hours * r.Price
	}
	return cost, totalEnergy
}
