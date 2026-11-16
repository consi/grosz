package store

import (
	"fmt"
	"time"
)

// RecordPlugTransition stores a plug (true) or unplug (false) transition.
// Duplicate timestamps are ignored.
func (s *Store) RecordPlugTransition(at time.Time, plugged bool) error {
	v := 0
	if plugged {
		v = 1
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO plug_history (timestamp, plugged) VALUES (?, ?)`,
		at.UTC().Format(time.RFC3339), v)
	if err != nil {
		return fmt.Errorf("record plug transition: %w", err)
	}
	return nil
}

// PurgePlugHistory deletes transitions older than maxAge.
func (s *Store) PurgePlugHistory(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM plug_history WHERE timestamp < ?`, cutoff)
	return err
}

// PlugPattern returns, for each (weekday, hour) in loc, the fraction of time
// the car was plugged in during [from, to), and the number of days of
// history covered. Time before the first recorded transition is not counted.
func (s *Store) PlugPattern(from, to time.Time, loc *time.Location) (pattern map[[2]int]float64, days float64, err error) {
	// The state at `from` is the last transition before it.
	var prev struct {
		ok      bool
		plugged bool
	}
	var p int
	if err := s.db.QueryRow(`SELECT plugged FROM plug_history WHERE timestamp < ? ORDER BY timestamp DESC LIMIT 1`,
		from.UTC().Format(time.RFC3339)).Scan(&p); err == nil {
		prev.ok, prev.plugged = true, p == 1
	}
	rows, err := s.db.Query(`SELECT timestamp, plugged FROM plug_history WHERE timestamp >= ? AND timestamp < ? ORDER BY timestamp`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, 0, fmt.Errorf("plug pattern: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type tr struct {
		at      time.Time
		plugged bool
	}
	var trs []tr
	for rows.Next() {
		var ts string
		var v int
		if err := rows.Scan(&ts, &v); err != nil {
			return nil, 0, err
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		trs = append(trs, tr{t, v == 1})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	start := from
	state := prev.plugged
	if !prev.ok {
		if len(trs) == 0 {
			return map[[2]int]float64{}, 0, nil
		}
		start, state = trs[0].at, trs[0].plugged
		trs = trs[1:]
	}

	total := map[[2]int]float64{}
	on := map[[2]int]float64{}
	// Walk in 15-minute samples; cheap (28 days = 2688 samples).
	const step = 15 * time.Minute
	i := 0
	for t := start.Truncate(step); t.Before(to); t = t.Add(step) {
		for i < len(trs) && !trs[i].at.After(t) {
			state = trs[i].plugged
			i++
		}
		lt := t.In(loc)
		key := [2]int{int(lt.Weekday()), lt.Hour()}
		total[key]++
		if state {
			on[key]++
		}
	}
	pattern = make(map[[2]int]float64, len(total))
	for k, n := range total {
		pattern[k] = on[k] / n
	}
	return pattern, to.Sub(start).Hours() / 24, nil
}
