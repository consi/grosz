package store

import (
	"database/sql"
	"fmt"
	"time"
)

// OdometerReading represents a single odometer snapshot.
type OdometerReading struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Mileage   float64   `json:"mileage"` // km
}

// InsertOdometerReading stores a new odometer reading.
func (s *Store) InsertOdometerReading(r OdometerReading) error {
	_, err := s.db.Exec(
		`INSERT INTO odometer_readings (timestamp, mileage) VALUES (?, ?)`,
		r.Timestamp.UTC().Format(time.RFC3339),
		r.Mileage,
	)
	if err != nil {
		return fmt.Errorf("insert odometer reading: %w", err)
	}
	return nil
}

// OdometerDelta returns the distance driven (km) within a time range.
func (s *Store) OdometerDelta(from, to time.Time) (float64, error) {
	var delta float64
	err := s.db.QueryRow(
		`SELECT COALESCE(MAX(mileage) - MIN(mileage), 0)
		 FROM odometer_readings
		 WHERE timestamp >= ? AND timestamp < ?`,
		from.UTC().Format(time.RFC3339),
		to.UTC().Format(time.RFC3339),
	).Scan(&delta)
	if err != nil {
		return 0, fmt.Errorf("odometer delta: %w", err)
	}
	if delta < 0 {
		delta = 0
	}
	return delta, nil
}

// LatestOdometerReading returns the most recent odometer reading.
func (s *Store) LatestOdometerReading() (*OdometerReading, error) {
	var r OdometerReading
	var tsStr string
	err := s.db.QueryRow(
		`SELECT id, timestamp, mileage FROM odometer_readings ORDER BY id DESC LIMIT 1`,
	).Scan(&r.ID, &tsStr, &r.Mileage)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest odometer: %w", err)
	}
	r.Timestamp, _ = time.Parse(time.RFC3339, tsStr)
	return &r, nil
}
