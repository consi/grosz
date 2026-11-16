package scheduler

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/consi/grosz/internal/tariff"
)

// TestReplayHistory replays the planner over exported real history and
// prints cost and battery-level metrics. It is a tuning/regression tool, not
// a unit test: it skips unless GROSZ_REPLAY_DIR points at an export with
// timeline.tsv (S=connector status, R=SoC), rates.tsv, pub.tsv (next-day
// price publication times) and limit.tsv (car charge-limit changes).
func TestReplayHistory(t *testing.T) {
	dir := os.Getenv("GROSZ_REPLAY_DIR")
	if dir == "" {
		t.Skip("GROSZ_REPLAY_DIR not set")
	}
	loc := waw
	parseT := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339Nano, strings.Replace(s, " ", "T", 1))
		if err != nil {
			t.Fatalf("time %q: %v", s, err)
		}
		return v.In(loc)
	}
	lines := func(name string) [][]string {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		var out [][]string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			out = append(out, strings.Split(sc.Text(), "\t"))
		}
		return out
	}

	type ev struct {
		at time.Time
		v  float64
	}
	var plugEv, socEv, limEv []ev
	for _, l := range lines("timeline.tsv") {
		at := parseT(l[1])
		if l[0] == "S" {
			v := 0.0
			if isPluggedIn(l[2]) || l[2] == "Finishing" {
				v = 1
			}
			plugEv = append(plugEv, ev{at, v})
		} else if f, err := strconv.ParseFloat(l[2], 64); err == nil {
			socEv = append(socEv, ev{at, f})
		}
	}
	for _, l := range lines("limit.tsv") {
		f, _ := strconv.ParseFloat(l[1], 64)
		limEv = append(limEv, ev{parseT(l[0]), f})
	}
	price := map[time.Time]float64{}
	var rates []tariff.Rate
	for _, l := range lines("rates.tsv") {
		at := parseT(l[0])
		p, _ := strconv.ParseFloat(l[1], 64)
		price[at] = p
		rates = append(rates, tariff.Rate{Start: at, End: at.Add(time.Hour), Price: p})
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i].Start.Before(rates[j].Start) })
	pub := map[string]time.Time{}
	for _, l := range lines("pub.tsv") {
		pub[l[0]] = parseT(l[1])
	}
	for _, s := range [][]ev{plugEv, socEv, limEv} {
		sort.Slice(s, func(i, j int) bool { return s[i].at.Before(s[j].at) })
	}
	at := func(s []ev, t time.Time, def float64) float64 {
		i := sort.Search(len(s), func(i int) bool { return s[i].at.After(t) }) - 1
		if i < 0 {
			return def
		}
		return s[i].v
	}

	start := socEv[0].at.Truncate(time.Hour).Add(time.Hour)
	if rates[0].Start.After(start) {
		start = rates[0].Start
	}
	end := socEv[len(socEv)-1].at.Truncate(time.Hour)
	const step = 15 * time.Minute
	var steps []time.Time
	for t := start; t.Before(end); t = t.Add(step) {
		steps = append(steps, t)
	}
	plug := make([]bool, len(steps))
	for k, t := range steps {
		plug[k] = at(plugEv, t, 0) == 1
	}
	// Driving: running-min SoC drops while unplugged.
	drive := make([]float64, len(steps))
	si := sort.Search(len(socEv), func(i int) bool { return !socEv[i].at.Before(start) })
	ref, haveRef := 0.0, false
	for k, t := range steps {
		for si < len(socEv) && socEv[si].at.Before(t.Add(step)) {
			v := socEv[si].v
			if plug[k] {
				ref, haveRef = v, true
			} else {
				if haveRef && v < ref {
					drive[k] += ref - v
				}
				if !haveRef || v < ref {
					ref, haveRef = v, true
				}
			}
			si++
		}
		if plug[k] {
			haveRef = false
		}
	}

	knownUntil := func(t time.Time) time.Time {
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		p, ok := pub[d.Format("2006-01-02")]
		if !ok {
			p = d.Add(13 * time.Hour)
		}
		if !t.Before(p) {
			return d.AddDate(0, 0, 2)
		}
		return d.AddDate(0, 0, 1)
	}
	pattern := func(k int) map[[2]int]float64 {
		lo := k - 28*96
		if lo < 0 {
			lo = 0
		}
		if k-lo < 7*96 {
			return nil
		}
		on, tot := map[[2]int]float64{}, map[[2]int]float64{}
		for i := lo; i < k; i++ {
			key := [2]int{int(steps[i].Weekday()), steps[i].Hour()}
			tot[key]++
			if plug[i] {
				on[key]++
			}
		}
		out := map[[2]int]float64{}
		for key, n := range tot {
			out[key] = on[key] / n
		}
		return out
	}

	soc := at(socEv, start, 80)
	var cost, kwh, low20 float64
	var unplugSoC []float64
	var prev *Schedule
	var pat map[[2]int]float64
	patDay := -1
	const wallKW, eff, capKWh = 11.7, 0.91, 52.0
	for k, t := range steps {
		if t.YearDay() != patDay {
			patDay, pat = t.YearDay(), pattern(k)
		}
		if k > 0 && plug[k-1] && !plug[k] {
			unplugSoC = append(unplugSoC, soc)
		}
		soc -= drive[k]
		if soc < 0 {
			soc = 0
		}
		if soc < 20 {
			low20 += 0.25
		}
		if !plug[k] {
			continue
		}
		ku := knownUntil(t)
		var known []tariff.Rate
		for _, r := range rates {
			if r.Start.Before(ku) {
				known = append(known, r)
			}
		}
		lim := at(limEv, t, 80)
		a := k - 14*96
		if a < 0 {
			a = 0
		}
		daily := 6.0
		if k > a {
			sum := 0.0
			for _, d := range drive[a:k] {
				sum += d
			}
			daily = sum / 100 * capKWh / (float64(k-a) / 96)
		}
		if t.Minute() == 0 || prev == nil || (k > 0 && !plug[k-1]) {
			prev, _ = Plan(PlanInput{
				Now: t, Loc: loc, Rates: known, SoC: soc, BatteryKWh: capKWh, Efficiency: eff, PowerW: wallKW * 1000,
				NormalTarget: 80, CarLimit: lim, Floor: 15, Safe: 50, DepartureH: 7,
				CheapFillPrice: 0.45, CheapFillSoC: 100, PluggedNow: true, PlugPattern: pat, DailyKWh: daily, Prev: prev,
			})
		}
		capSoC := lim
		charging := false
		for _, p := range prev.ActivePeriods() {
			if !t.Before(p.Start) && t.Before(p.End) {
				charging = true
				if p.FillSoC > capSoC {
					capSoC = p.FillSoC
				}
			}
		}
		if charging && soc < capSoC-0.05 {
			e := min(wallKW*0.25, (capSoC-soc)/100*capKWh/eff)
			soc += e * eff / capKWh * 100
			kwh += e
			cost += e * price[t.Truncate(time.Hour)]
		}
	}
	sort.Float64s(unplugSoC)
	below60 := 0
	sum := 0.0
	for _, v := range unplugSoC {
		sum += v
		if v < 60 {
			below60++
		}
	}
	t.Logf("days %.0f  cost %.0f PLN  %.0f kWh  avg %.3f  unplug mean %.0f p10 %.0f  <60 %.0f%%  hours<20 %.0f",
		end.Sub(start).Hours()/24, cost, kwh, cost/kwh, sum/float64(len(unplugSoC)),
		unplugSoC[len(unplugSoC)/10], 100*float64(below60)/float64(len(unplugSoC)), low20)
}
