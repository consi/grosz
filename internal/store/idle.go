package store

import (
	"fmt"
	"time"
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
	defer rows.Close()

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

// SnapshotDailyIdle computes and stores the idle energy for a given day.
// Idle = total meter consumption - charging session energy.
func (s *Store) SnapshotDailyIdle(day time.Time) error {
	date := day.Format("2006-01-02")
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location())
	dayEnd := dayStart.Add(24 * time.Hour)

	// Total meter consumption for the day
	var totalMeterWh float64
	err := s.db.QueryRow(
		`SELECT COALESCE(MAX(energy_wh) - MIN(energy_wh), 0)
		 FROM meter_readings
		 WHERE timestamp >= ? AND timestamp < ?`,
		dayStart.UTC().Format(time.RFC3339),
		dayEnd.UTC().Format(time.RFC3339),
	).Scan(&totalMeterWh)
	if err != nil {
		return fmt.Errorf("meter query for %s: %w", date, err)
	}

	totalMeterKWh := totalMeterWh / 1000.0
	if totalMeterKWh <= 0 {
		return nil // no meter data for this day
	}

	// Total charging energy for the day
	var chargingKWh float64
	_ = s.db.QueryRow(
		`SELECT COALESCE(SUM(energy), 0)
		 FROM charging_sessions
		 WHERE start_time >= ? AND start_time < ? AND status = 'completed'`,
		dayStart.UTC().Format(time.RFC3339),
		dayEnd.UTC().Format(time.RFC3339),
	).Scan(&chargingKWh)

	idleKWh := totalMeterKWh - chargingKWh
	if idleKWh < 0 {
		idleKWh = 0
	}

	idleCost := s.CalculateSessionCost(dayStart, dayEnd, idleKWh)

	return s.UpsertDailyIdle(date, idleKWh, idleCost)
}
