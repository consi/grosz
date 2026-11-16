package scheduler

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

// Plan modes, exposed to the UI so it can explain what the planner is doing.
const (
	ModeCharge  = "charge"  // reaching the target before the next departure
	ModeWait    = "wait"    // SoC is safe: using the cheapest hours up to a day later
	ModeTrip    = "trip"    // car limit raised above the normal target: ready by the trip time
	ModeFloor   = "floor"   // below the floor: charging as soon as possible
	ModeFull    = "full"    // target reached; only negative / cheap-fill hours remain
	ModePaused  = "paused"  // automatic planning paused by the user
	ModeNoRates = "noRates" // no price data
)

// Period reasons.
const (
	ReasonNeeded    = "needed"    // required to reach the target by departure
	ReasonCheapest  = "cheapest"  // waiting mode: cheapest hours within ~a day
	ReasonTrip      = "trip"      // required for the trip target
	ReasonFloor     = "floor"     // battery below floor
	ReasonNegative  = "negative"  // negative price: paid to charge
	ReasonCheapFill = "cheapFill" // very cheap hour: fill above the normal target
	ReasonManual    = "manual"    // user force window
)

// usableProbability is the minimum historical plugged-in share for a future
// hour to be planned on when the car is not plugged in now.
const usableProbability = 0.3

// gapFillPLN is the most a plan may pay extra to close a one-hour gap
// between two charging hours (fewer pauses / profile periods).
const gapFillPLN = 1.0

// stickyPLN: keep the previous plan unless the new one is cheaper by more.
const stickyPLN = 1.0

// PlanInput is everything the planner needs. Pure data, no store access.
type PlanInput struct {
	Now        time.Time
	Loc        *time.Location
	Rates      []tariff.Rate
	SoC        float64 // %
	BatteryKWh float64
	Efficiency float64 // battery kWh gained per wall kWh
	PowerW     float64

	NormalTarget float64   // % the car normally charges to (80)
	CarLimit     float64   // % charge limit currently set in the car (0 = unknown)
	FillRaised   bool      // CarLimit was raised by grosz for a cheap fill, not by the user
	TripReadyBy  time.Time // zero = next departure

	Floor          float64 // %
	Safe           float64 // %
	DepartureH     int
	DepartureM     int
	MaxPrice       float64 // PLN/kWh, 0 = none; ignored when charging is required
	CheapFillPrice float64 // PLN/kWh, <= 0 disables positive cheap fill
	CheapFillSoC   float64 // % cap for cheap fill; <= NormalTarget disables filling above it

	PluggedNow  bool
	PlugPattern map[[2]int]float64 // (weekday, hour) -> share plugged; empty = unknown
	DailyKWh    float64            // expected battery kWh used per day

	Forces []store.ScheduleOverride
	Blocks []store.ScheduleOverride
	Paused bool

	Prev *Schedule // previous plan, for stickiness
}

type cand struct {
	start, end time.Time
	price      float64
	hours      float64 // usable hours from now
}

// Plan computes the charging plan. Returns nil when nothing is planned;
// the mode is then still reported via the second return value.
func Plan(in PlanInput) (*Schedule, string) {
	if in.Loc == nil {
		in.Loc = time.Local
	}
	if in.Efficiency <= 0 {
		in.Efficiency = 0.9
	}
	now := in.Now
	kw := in.PowerW / 1000
	d1 := nextDeadlineAfter(now, in.DepartureH, in.DepartureM, in.Loc)

	// Target: normal, a lowered car limit, or a trip (limit raised by the user).
	target := in.NormalTarget
	trip := false
	if in.CarLimit > 0 && !in.FillRaised {
		if in.CarLimit > in.NormalTarget+0.5 {
			target, trip = in.CarLimit, true
		} else if in.CarLimit < in.NormalTarget {
			target = in.CarLimit
		}
	}
	readyBy := d1
	if trip && in.TripReadyBy.After(now) {
		readyBy = in.TripReadyBy
	}

	forcePeriods := buildForcePeriods(in.Forces, in.Rates)
	for i := range forcePeriods {
		forcePeriods[i].Reason = ReasonManual
	}

	// Candidate hours: future, with blocks and force windows carved out.
	var known time.Time
	var cands []cand
	carve := append(append([]store.ScheduleOverride{}, in.Blocks...), in.Forces...)
	for _, r := range fragmentRates(in.Rates, carve) {
		if r.End.After(known) {
			known = r.End
		}
		if !r.End.After(now) {
			continue
		}
		s := r.Start
		if now.After(s) {
			s = now
		}
		cands = append(cands, cand{start: r.Start, end: r.End, price: r.Price, hours: r.End.Sub(s).Hours()})
	}

	stayUntil := d1
	if lim := now.Add(12 * time.Hour); lim.Before(stayUntil) {
		stayUntil = lim
	}
	usable := func(c cand) bool {
		if in.PluggedNow && c.start.Before(stayUntil) {
			return true
		}
		if len(in.PlugPattern) == 0 {
			return true // no history yet: don't restrict
		}
		lt := c.start.In(in.Loc)
		return in.PlugPattern[[2]int{int(lt.Weekday()), lt.Hour()}] >= usableProbability
	}

	wallFor := func(socTo float64) float64 {
		return math.Max(0, (socTo-in.SoC)/100*in.BatteryKWh/in.Efficiency)
	}
	forceKWh := totalForceEnergy(forcePeriods)
	need := math.Max(0, wallFor(target)-forceKWh)

	selected := map[time.Time]SchedulePeriod{}
	add := func(c cand, reason string, fill float64) {
		if p, ok := selected[c.start]; ok {
			if fill > p.FillSoC {
				p.FillSoC = fill
				selected[c.start] = p
			}
			return
		}
		if c.price < 0 && reason != ReasonFloor {
			reason = ReasonNegative
		}
		selected[c.start] = SchedulePeriod{Start: c.start, End: c.end, Power: in.PowerW, Price: c.price,
			Source: sourceAuto, Reason: reason, FillSoC: fill}
	}

	mode := ModeCharge
	switch {
	case len(cands) == 0 && len(forcePeriods) == 0:
		return nil, ModeNoRates
	case in.Paused:
		mode = ModePaused
	case in.SoC < in.Floor:
		mode = ModeFloor
		left := need
		for _, c := range cands { // chronological: as soon as possible
			if left <= 1e-6 {
				break
			}
			if c.hours > 0 && usable(c) {
				add(c, ReasonFloor, 0)
				left -= kw * c.hours
			}
		}
	case need < 0.5:
		mode = ModeFull
	default:
		risky := in.SoC-in.DailyKWh/in.BatteryKWh*100 < in.Floor+10
		must := in.SoC < in.Safe || trip || risky
		deadline := readyBy
		reason := ReasonNeeded
		if trip {
			mode, reason = ModeTrip, ReasonTrip
		} else if !must {
			mode, reason = ModeWait, ReasonCheapest
			deadline = d1.Add(24 * time.Hour)
			if known.Before(deadline) {
				deadline = known
			}
		}
		var pool []cand
		for _, c := range cands {
			if !c.start.Before(deadline) || c.hours <= 0 || !usable(c) {
				continue
			}
			if !must && in.MaxPrice > 0 && c.price > in.MaxPrice {
				continue
			}
			pool = append(pool, c)
		}
		sort.SliceStable(pool, func(i, j int) bool {
			if pool[i].price != pool[j].price {
				return pool[i].price < pool[j].price
			}
			return pool[i].start.Before(pool[j].start)
		})
		left := need
		for _, c := range pool {
			if left <= 1e-6 {
				break
			}
			add(c, reason, 0)
			left -= kw * c.hours
		}
		// Charging is required but the usual plug-in hours don't cover it:
		// plan the cheapest remaining hours anyway (re-planned with the real
		// hours once the car is plugged in) rather than showing nothing.
		if must && left > 1e-6 {
			var rest []cand
			for _, c := range cands {
				if _, ok := selected[c.start]; ok || !c.start.Before(deadline) || c.hours <= 0 {
					continue
				}
				rest = append(rest, c)
			}
			sort.SliceStable(rest, func(i, j int) bool { return rest[i].price < rest[j].price })
			for _, c := range rest {
				if left <= 1e-6 {
					break
				}
				add(c, reason, 0)
				left -= kw * c.hours
			}
		}
		gapFill(selected, cands, kw)
	}

	// Negative prices: always charge up to the target, most negative first,
	// leaving room for better hours later (counting driving before them).
	if mode != ModePaused {
		addRanked(cands, usable, func(c cand) bool { return c.price < 0 }, now, in, wallFor(target), kw, selected,
			func(c cand) { add(c, ReasonNegative, 0) })
		// Cheap fill above the normal target.
		if in.CheapFillPrice > 0 && in.CheapFillSoC > target {
			addRanked(cands, usable, func(c cand) bool { return c.price <= in.CheapFillPrice }, now, in,
				wallFor(in.CheapFillSoC), kw, selected,
				func(c cand) { add(c, ReasonCheapFill, in.CheapFillSoC) })
		}
	}

	auto := make([]SchedulePeriod, 0, len(selected))
	for _, p := range selected {
		auto = append(auto, p)
	}
	sort.Slice(auto, func(i, j int) bool { return auto[i].Start.Before(auto[j].Start) })

	sig := planSignature(in, target, mode)
	valid := make(map[time.Time]bool, len(cands))
	for _, c := range cands {
		valid[c.start] = true
	}
	if keep := stickyPrev(in, auto, sig, need, kw, now, valid); keep != nil {
		auto = keep
	}

	all := append(auto, forcePeriods...)
	if len(all) == 0 {
		return nil, mode
	}
	sched := assembleSchedule(all, in.DepartureH, in.DepartureM, in.Loc, kw)
	sched.Mode = mode
	sched.Target = target
	sched.ReadyBy = readyBy
	sched.NeedKWh = math.Round(need*10) / 10
	sched.Sig = sig
	return sched, mode
}

// addRanked takes matching hours, best (cheapest) first, while they fit the
// headroom to `roomKWh` plus the driving expected before each hour.
func addRanked(cands []cand, usable func(cand) bool, match func(cand) bool, now time.Time, in PlanInput,
	roomKWh, kw float64, selected map[time.Time]SchedulePeriod, take func(cand)) {
	var pool []cand
	for _, c := range cands {
		if c.hours > 0 && match(c) && usable(c) {
			pool = append(pool, c)
		}
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].price < pool[j].price })
	// Energy already planned uses up the headroom first.
	used := 0.0
	for _, p := range selected {
		s := p.Start
		if now.After(s) {
			s = now
		}
		used += kw * math.Max(0, p.End.Sub(s).Hours())
	}
	for _, c := range pool {
		if _, ok := selected[c.start]; ok {
			take(c) // may upgrade the fill cap; no extra energy
			continue
		}
		extra := in.DailyKWh / in.Efficiency * math.Max(0, c.start.Sub(now).Hours()/24)
		if used >= roomKWh+extra-1e-6 {
			continue
		}
		used += kw * c.hours
		take(c)
	}
}

// gapFill closes one-hour gaps between charging hours when that costs at
// most gapFillPLN: it moves one selected hour (either neighbour of the gap or
// the most expensive elsewhere) into the gap, keeping the energy the same,
// and only accepts swaps that reduce the number of separate blocks.
func gapFill(selected map[time.Time]SchedulePeriod, cands []cand, kw float64) {
	byStart := map[time.Time]cand{}
	for _, c := range cands {
		byStart[c.start] = c
	}
	blocks := func(m map[time.Time]SchedulePeriod) int {
		n := 0
		for t := range m {
			if _, prev := m[t.Add(-time.Hour)]; !prev {
				n++
			}
		}
		return n
	}
	for iter := 0; iter < 6; iter++ {
		var starts []time.Time
		for t := range selected {
			starts = append(starts, t)
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
		before := blocks(selected)
		bestDelta := math.Inf(1)
		var bestOut, bestGap time.Time
		for i := 0; i+1 < len(starts); i++ {
			a, b := starts[i], starts[i+1]
			if b.Sub(a) != 2*time.Hour {
				continue
			}
			// No plug-in check: charging on both sides means the car is
			// plugged in during the gap too.
			gap, ok := byStart[a.Add(time.Hour)]
			if !ok {
				continue
			}
			for _, out := range starts {
				if selected[out].Reason == ReasonNegative || selected[out].Reason == ReasonManual {
					continue
				}
				delta := (gap.price - selected[out].Price) * kw
				if delta > gapFillPLN || delta >= bestDelta {
					continue
				}
				trial := map[time.Time]SchedulePeriod{}
				for k, v := range selected {
					trial[k] = v
				}
				delete(trial, out)
				trial[gap.start] = SchedulePeriod{}
				if blocks(trial) < before {
					bestDelta, bestOut, bestGap = delta, out, gap.start
				}
			}
		}
		if bestGap.IsZero() {
			return
		}
		p := selected[bestOut]
		g := byStart[bestGap]
		delete(selected, bestOut)
		p.Start, p.End, p.Price = g.start, g.end, g.price
		selected[g.start] = p
	}
}

// stickyPrev returns the previous plan's automatic periods when inputs are
// unchanged, it still covers the need, and the new plan is not meaningfully
// cheaper. This keeps the charger's profile stable as SoC ticks.
func stickyPrev(in PlanInput, fresh []SchedulePeriod, sig string, need, kw float64, now time.Time, valid map[time.Time]bool) []SchedulePeriod {
	if in.Prev == nil || in.Prev.Sig != sig {
		return nil
	}
	freshSet := map[time.Time]bool{}
	for _, p := range fresh {
		freshSet[p.Start] = true
	}
	var prev []SchedulePeriod
	var energy, prevCost, freshCost float64
	for _, p := range in.Prev.ActivePeriods() {
		if p.IsForce() || !p.End.After(now) {
			continue
		}
		if !valid[p.Start] {
			return nil // hour no longer priced / now blocked: re-plan
		}
		start := p.Start
		if now.After(start) {
			start = now
		}
		e := kw * p.End.Sub(start).Hours()
		energy += e
		prevCost += e * p.Price
		prev = append(prev, p)
	}
	for _, p := range fresh {
		start := p.Start
		if now.After(start) {
			start = now
		}
		freshCost += kw * p.End.Sub(start).Hours() * p.Price
	}
	// Every negative / cheap-fill hour in the fresh plan must be kept.
	prevSet := map[time.Time]bool{}
	for _, p := range prev {
		prevSet[p.Start] = true
	}
	for _, p := range fresh {
		if (p.Reason == ReasonNegative || p.Reason == ReasonCheapFill) && !prevSet[p.Start] {
			return nil
		}
	}
	if energy+1e-6 < need || prevCost > freshCost+stickyPLN || len(prev) == 0 {
		return nil
	}
	return prev
}

// planSignature changes whenever an input that must re-plan from scratch
// changes (targets, mode, overrides, settings) as opposed to SoC drift.
func planSignature(in PlanInput, target float64, mode string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%.0f|%.0f|%.0f|%.2f|%.2f|%.0f|%d:%d|%v|%d",
		mode, target, in.Floor, in.Safe, in.MaxPrice, in.CheapFillPrice, in.CheapFillSoC,
		in.DepartureH, in.DepartureM, in.Paused, in.TripReadyBy.Unix())
	for _, o := range append(append([]store.ScheduleOverride{}, in.Forces...), in.Blocks...) {
		fmt.Fprintf(h, "|%s%d-%d", o.Kind, o.Start.Unix(), o.End.Unix())
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:8])
}

// assembleSchedule groups periods into one slot per departure day.
func assembleSchedule(periods []SchedulePeriod, dh, dm int, loc *time.Location, kw float64) *Schedule {
	sort.Slice(periods, func(i, j int) bool { return periods[i].Start.Before(periods[j].Start) })
	var slots []ScheduleSlot
	idx := map[string]int{}
	for _, p := range periods {
		dl := nextDeadlineAfter(p.Start, dh, dm, loc)
		date := dl.Format("2006-01-02")
		i, ok := idx[date]
		if !ok {
			slots = append(slots, ScheduleSlot{Date: date, Deadline: dl})
			i = len(slots) - 1
			idx[date] = i
		}
		slots[i].Periods = append(slots[i].Periods, p)
	}
	s := &Schedule{Slots: slots}
	var cost, energy float64
	for i := range s.Slots {
		recalcSlotTotals(&s.Slots[i])
		cost += s.Slots[i].Cost
		energy += s.Slots[i].Energy
		if s.Slots[i].Deadline.After(s.Deadline) {
			s.Deadline = s.Slots[i].Deadline
		}
	}
	s.Cost = math.Round(cost*100) / 100
	s.Energy = math.Round(energy*100) / 100
	return s
}
