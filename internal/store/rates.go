package store

import (
	"fmt"
	"time"
)

// Rate represents an electricity tariff rate for a time period.
type Rate struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Price float64   `json:"price"` // PLN/kWh gross
}

// SaveRates upserts tariff rates for a provider.
func (s *Store) SaveRates(provider string, rates []Rate) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO tariff_rates (provider, start_time, end_time, price)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(provider, start_time) DO UPDATE SET end_time = excluded.end_time, price = excluded.price, fetched_at = datetime('now')`,
	)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, r := range rates {
		if _, err := stmt.Exec(
			provider,
			r.Start.UTC().Format(time.RFC3339),
			r.End.UTC().Format(time.RFC3339),
			r.Price,
		); err != nil {
			return fmt.Errorf("upsert rate: %w", err)
		}
	}

	return tx.Commit()
}

// LoadRates returns cached rates for a provider that haven't ended yet, sorted by start time.
func (s *Store) LoadRates(provider string) ([]Rate, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT start_time, end_time, price FROM tariff_rates
		 WHERE provider = ? AND end_time > ?
		 ORDER BY start_time ASC`,
		provider, now,
	)
	if err != nil {
		return nil, fmt.Errorf("query rates: %w", err)
	}
	defer rows.Close()

	var rates []Rate
	for rows.Next() {
		var startStr, endStr string
		var price float64
		if err := rows.Scan(&startStr, &endStr, &price); err != nil {
			return nil, fmt.Errorf("scan rate: %w", err)
		}
		start, _ := time.Parse(time.RFC3339, startStr)
		end, _ := time.Parse(time.RFC3339, endStr)
		rates = append(rates, Rate{Start: start, End: end, Price: price})
	}
	return rates, rows.Err()
}

// RatesForPeriod returns rates that overlap with the given time range.
func (s *Store) RatesForPeriod(from, to time.Time) ([]Rate, error) {
	rows, err := s.db.Query(
		`SELECT start_time, end_time, price FROM tariff_rates
		 WHERE start_time < ? AND end_time > ?
		 ORDER BY start_time ASC`,
		to.UTC().Format(time.RFC3339),
		from.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("query rates for period: %w", err)
	}
	defer rows.Close()

	var rates []Rate
	for rows.Next() {
		var startStr, endStr string
		var price float64
		if err := rows.Scan(&startStr, &endStr, &price); err != nil {
			return nil, fmt.Errorf("scan rate: %w", err)
		}
		start, _ := time.Parse(time.RFC3339, startStr)
		end, _ := time.Parse(time.RFC3339, endStr)
		rates = append(rates, Rate{Start: start, End: end, Price: price})
	}
	return rates, rows.Err()
}

// CalculateSessionCost computes the cost of energy used between two times
// by matching each hour's rate to a proportional share of energy.
// Uses a simple linear distribution: total energy spread evenly over session duration,
// then each hour's portion priced at that hour's rate.
func (s *Store) CalculateSessionCost(start, stop time.Time, energyKWh float64) float64 {
	if energyKWh <= 0 {
		return 0
	}
	rates, err := s.RatesForPeriod(start, stop)
	if err != nil || len(rates) == 0 {
		return 0
	}

	totalHours := stop.Sub(start).Hours()
	if totalHours <= 0 {
		return 0
	}
	powerKW := energyKWh / totalHours // average power

	var cost float64
	for _, r := range rates {
		// Overlap between session and this rate period
		overlapStart := r.Start
		if start.After(overlapStart) {
			overlapStart = start
		}
		overlapEnd := r.End
		if stop.Before(overlapEnd) {
			overlapEnd = stop
		}
		hours := overlapEnd.Sub(overlapStart).Hours()
		if hours <= 0 {
			continue
		}
		cost += powerKW * hours * r.Price
	}
	return cost
}

// PurgeOldRates removes rates that ended before the given time.
func (s *Store) PurgeOldRates(before time.Time) error {
	_, err := s.db.Exec(
		"DELETE FROM tariff_rates WHERE end_time < ?",
		before.UTC().Format(time.RFC3339),
	)
	return err
}
