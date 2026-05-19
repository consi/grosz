package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PstrykConsumption is one hourly consumption frame fetched from Pstryk.
type PstrykConsumption struct {
	Hour     time.Time `json:"hour"`     // UTC, on the hour
	EnergyWh float64   `json:"energyWh"` // consumed in this hour
}

// UpsertPstrykConsumption stores or replaces hourly consumption rows. Pstryk
// may revise values for ~48 h after the hour closes, so we always upsert.
func (s *Store) UpsertPstrykConsumption(frames []PstrykConsumption) error {
	if len(frames) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`INSERT INTO pstryk_consumption (hour, energy_wh, fetched_at)
		 VALUES (?, ?, datetime('now'))
		 ON CONFLICT(hour) DO UPDATE SET
		   energy_wh  = excluded.energy_wh,
		   fetched_at = excluded.fetched_at`,
	)
	if err != nil {
		return fmt.Errorf("prepare upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, f := range frames {
		if _, err := stmt.Exec(f.Hour.UTC().Format(time.RFC3339), f.EnergyWh); err != nil {
			return fmt.Errorf("upsert pstryk row: %w", err)
		}
	}
	return tx.Commit()
}

// PstrykHourlyConsumption returns the last N hours of consumption, ascending.
func (s *Store) PstrykHourlyConsumption(hours int) ([]HourlyEnergy, error) {
	if hours <= 0 {
		hours = 48
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT hour, energy_wh FROM pstryk_consumption
		 WHERE hour >= ?
		 ORDER BY hour ASC`,
		since,
	)
	if err != nil {
		return nil, fmt.Errorf("query pstryk hourly: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]HourlyEnergy, 0)
	for rows.Next() {
		var he HourlyEnergy
		var hourStr string
		if err := rows.Scan(&hourStr, &he.EnergyWh); err != nil {
			return nil, fmt.Errorf("scan pstryk hourly: %w", err)
		}
		he.Hour, _ = time.Parse(time.RFC3339, hourStr)
		// Wh consumed over one hour equals the hour's average W.
		he.PowerW = he.EnergyWh
		result = append(result, he)
	}
	return result, rows.Err()
}

// LatestPstrykHour returns the most recent stored hour. ok=false when the
// table is empty — caller uses this to decide between bootstrap and gap-fill.
func (s *Store) LatestPstrykHour() (time.Time, bool) {
	return s.pstrykHourEdge("DESC")
}

// EarliestPstrykHour returns the oldest stored hour. Used by the backfill
// loop to decide whether existing data already covers the desired window or
// if we need to extend the fetch further into the past.
func (s *Store) EarliestPstrykHour() (time.Time, bool) {
	return s.pstrykHourEdge("ASC")
}

func (s *Store) pstrykHourEdge(order string) (time.Time, bool) {
	var hourStr string
	err := s.db.QueryRow(
		`SELECT hour FROM pstryk_consumption ORDER BY hour ` + order + ` LIMIT 1`,
	).Scan(&hourStr)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, hourStr)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// PstrykEnergyKWh returns total energy consumed in [start, end) using stored
// hourly rows. Partial hours at the window edges are counted proportionally
// (e.g., 30 min of an hour contributes half the row's value).
func (s *Store) PstrykEnergyKWh(start, end time.Time) (float64, error) {
	if !end.After(start) {
		return 0, nil
	}
	// Pull every row that could overlap the window: any hour h such that
	// h < end AND h+1h > start. SQL keys are stored as h, so we fetch the
	// closed range [floor(start, 1h) - 1h, end) and filter in Go.
	queryStart := start.UTC().Truncate(time.Hour).Add(-time.Hour).Format(time.RFC3339)
	queryEnd := end.UTC().Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT hour, energy_wh FROM pstryk_consumption
		 WHERE hour >= ? AND hour < ?
		 ORDER BY hour ASC`,
		queryStart, queryEnd,
	)
	if err != nil {
		return 0, fmt.Errorf("query pstryk energy: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var totalWh float64
	for rows.Next() {
		var hourStr string
		var wh float64
		if err := rows.Scan(&hourStr, &wh); err != nil {
			return 0, fmt.Errorf("scan pstryk energy: %w", err)
		}
		h, err := time.Parse(time.RFC3339, hourStr)
		if err != nil {
			continue
		}
		hEnd := h.Add(time.Hour)
		ovStart := h
		if start.After(ovStart) {
			ovStart = start
		}
		ovEnd := hEnd
		if end.Before(ovEnd) {
			ovEnd = end
		}
		if !ovEnd.After(ovStart) {
			continue
		}
		frac := ovEnd.Sub(ovStart).Seconds() / time.Hour.Seconds()
		totalWh += wh * frac
	}
	return totalWh / 1000.0, rows.Err()
}

// PstrykRowsCount returns the number of pstryk_consumption rows whose hour
// falls in [start, end). Lets callers distinguish "no consumption" from
// "no data yet" before deciding to rebuild a daily-idle snapshot.
func (s *Store) PstrykRowsCount(start, end time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pstryk_consumption
		 WHERE hour >= ? AND hour < ?`,
		start.UTC().Format(time.RFC3339),
		end.UTC().Format(time.RFC3339),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("pstryk rows count: %w", err)
	}
	return n, nil
}
