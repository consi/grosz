package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/consi/grosz/internal/events"
)

// DailyIdle holds the daily idle energy consumption and cost.
type DailyIdle struct {
	ID        int64   `json:"id"`
	Date      string  `json:"date"`
	EnergyKWh float64 `json:"energyKwh"`
	Cost      float64 `json:"cost"`
}

// UpsertDailyIdle inserts or updates the daily idle entry for a given date.
func (s *Store) UpsertDailyIdle(date string, energyKWh, cost float64) error {
	_, err := s.db.Exec(
		`INSERT INTO daily_idle (date, energy_kwh, cost) VALUES (?, ?, ?)
		 ON CONFLICT(date) DO UPDATE SET energy_kwh = excluded.energy_kwh, cost = excluded.cost`,
		date, energyKWh, cost,
	)
	return err
}

// DailyIdleByDateRange returns daily idle entries within a date range.
func (s *Store) DailyIdleByDateRange(from, to time.Time) ([]DailyIdle, error) {
	rows, err := s.db.Query(
		`SELECT id, date, energy_kwh, cost FROM daily_idle
		 WHERE date >= ? AND date < ?
		 ORDER BY date DESC`,
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
	)
	if err != nil {
		return nil, fmt.Errorf("daily idle by range: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []DailyIdle
	for rows.Next() {
		var d DailyIdle
		if err := rows.Scan(&d.ID, &d.Date, &d.EnergyKWh, &d.Cost); err != nil {
			return nil, fmt.Errorf("scan daily idle: %w", err)
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// SumDailyIdle returns the total idle energy and cost for a date range.
func (s *Store) SumDailyIdle(from, to time.Time) (energyKWh, cost float64, err error) {
	err = s.db.QueryRow(
		`SELECT COALESCE(SUM(energy_kwh), 0), COALESCE(SUM(cost), 0)
		 FROM daily_idle WHERE date >= ? AND date < ?`,
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
	).Scan(&energyKWh, &cost)
	return
}

// TimeWindow is a half-open [Start, End) interval.
type TimeWindow struct {
	Start time.Time
	End   time.Time
}

// SnapshotDailyIdle computes and stores the idle energy for a given day.
//
// Idle is the meter delta over time windows where no charging session is
// active. While a session is open, every watt drawn from the grid is
// attributed to the car, even if the house is consuming some of it — that's
// the user-stated convention ("if the car charges, all of the electricity
// is assumed to be consumed by a car").
//
// For today (dayEnd in the future) the window is clipped to now, so partial-
// day refreshes don't roll an empty tail into the calculation.
func (s *Store) SnapshotDailyIdle(day time.Time) error {
	date := day.Format("2006-01-02")
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	dayEnd := dayStart.Add(24 * time.Hour)
	now := time.Now()
	if dayEnd.After(now) {
		dayEnd = now
	}
	if !dayEnd.After(dayStart) {
		return nil // future day, nothing to snapshot yet
	}

	// Skip when no meter data exists for this day at all — preserves any
	// previous snapshot (e.g. an earlier day finalized before retention purge).
	rowCount, err := s.MeterReadingsCount(dayStart, dayEnd)
	if err != nil {
		return fmt.Errorf("meter rows for %s: %w", date, err)
	}
	if rowCount < 2 {
		return nil
	}

	windows, err := s.nonChargingWindows(dayStart, dayEnd)
	if err != nil {
		return fmt.Errorf("non-charging windows for %s: %w", date, err)
	}

	var idleKWh, idleCost float64
	for _, w := range windows {
		delta, err := s.MeterDeltaKWh(w.Start, w.End)
		if err != nil {
			return err
		}
		if delta <= 0 {
			continue
		}
		idleKWh += delta
		idleCost += s.CalculateSessionCost(w.Start, w.End, delta)
	}

	if err := s.UpsertDailyIdle(date, idleKWh, idleCost); err != nil {
		return err
	}

	events.Info(s, events.SourceStore, events.ActionIdleSnapshotDaily,
		map[string]any{"date": date, "windows": len(windows)},
		map[string]any{"idleKWh": idleKWh, "idleCost": idleCost},
	)
	return nil
}

// nonChargingWindows returns the sub-intervals of [dayStart, dayEnd) that
// are not covered by any charging session. Session windows are clipped to
// the day; active sessions (no stop_time) are treated as still running at
// dayEnd. Overlapping sessions are merged before subtraction.
func (s *Store) nonChargingWindows(dayStart, dayEnd time.Time) ([]TimeWindow, error) {
	rows, err := s.db.Query(
		`SELECT start_time, stop_time FROM charging_sessions
		 WHERE start_time < ?
		   AND (stop_time IS NULL OR stop_time > ?)
		 ORDER BY start_time ASC`,
		dayEnd.UTC().Format(time.RFC3339),
		dayStart.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions for non-charging windows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []TimeWindow
	for rows.Next() {
		var startStr string
		var stopStr sql.NullString
		if err := rows.Scan(&startStr, &stopStr); err != nil {
			return nil, fmt.Errorf("scan session window: %w", err)
		}
		start, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			continue
		}
		stop := dayEnd
		if stopStr.Valid {
			parsed, err := time.Parse(time.RFC3339, stopStr.String)
			if err == nil {
				stop = parsed
			}
		}
		// Clip to the day.
		if start.Before(dayStart) {
			start = dayStart
		}
		if stop.After(dayEnd) {
			stop = dayEnd
		}
		if !stop.After(start) {
			continue
		}
		sessions = append(sessions, TimeWindow{Start: start, End: stop})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session windows: %w", err)
	}

	merged := mergeWindows(sessions)
	return invertWindows(dayStart, dayEnd, merged), nil
}

// mergeWindows collapses overlapping/adjacent windows. Input must be sorted
// by Start; output is also sorted by Start.
func mergeWindows(in []TimeWindow) []TimeWindow {
	if len(in) == 0 {
		return nil
	}
	out := []TimeWindow{in[0]}
	for _, w := range in[1:] {
		last := &out[len(out)-1]
		if !w.Start.After(last.End) {
			if w.End.After(last.End) {
				last.End = w.End
			}
			continue
		}
		out = append(out, w)
	}
	return out
}

// invertWindows returns the gaps within [start, end) that are not covered
// by any input window. Inputs must be sorted by Start, non-overlapping, and
// fully contained in [start, end).
func invertWindows(start, end time.Time, sessions []TimeWindow) []TimeWindow {
	var out []TimeWindow
	cursor := start
	for _, w := range sessions {
		if w.Start.After(cursor) {
			out = append(out, TimeWindow{Start: cursor, End: w.Start})
		}
		if w.End.After(cursor) {
			cursor = w.End
		}
	}
	if cursor.Before(end) {
		out = append(out, TimeWindow{Start: cursor, End: end})
	}
	return out
}
