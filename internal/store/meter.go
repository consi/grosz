package store

import (
	"fmt"
	"time"
)

// MeterReading is a single snapshot from the energy meter.
type MeterReading struct {
	Timestamp time.Time `json:"timestamp"`
	PowerW    float64   `json:"powerW"`
	EnergyWh  float64   `json:"energyWh"` // cumulative
}

// HourlyEnergy is aggregated energy consumption for one hour.
type HourlyEnergy struct {
	Hour     time.Time `json:"hour"`
	EnergyWh float64   `json:"energyWh"` // consumed in this hour
	PowerW   float64   `json:"powerW"`   // average power
}

// InsertMeterReading stores a reading and purges old data.
func (s *Store) InsertMeterReading(r MeterReading) error {
	_, err := s.db.Exec(
		`INSERT INTO meter_readings (timestamp, power_w, energy_wh) VALUES (?, ?, ?)`,
		r.Timestamp.UTC().Format(time.RFC3339),
		r.PowerW,
		r.EnergyWh,
	)
	if err != nil {
		return fmt.Errorf("insert meter reading: %w", err)
	}
	return nil
}

// PurgeMeterReadings deletes readings older than the given duration.
func (s *Store) PurgeMeterReadings(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM meter_readings WHERE timestamp < ?`, cutoff)
	return err
}

// HourlyConsumption computes energy consumed per hour from cumulative readings.
// Returns data for the last `hours` hours.
func (s *Store) HourlyConsumption(hours int) ([]HourlyEnergy, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)

	rows, err := s.db.Query(`
		WITH hourly AS (
			SELECT
				strftime('%Y-%m-%dT%H:00:00Z', timestamp) AS hour,
				energy_wh,
				power_w,
				ROW_NUMBER() OVER (PARTITION BY strftime('%Y-%m-%dT%H', timestamp) ORDER BY timestamp ASC) AS rn_asc,
				ROW_NUMBER() OVER (PARTITION BY strftime('%Y-%m-%dT%H', timestamp) ORDER BY timestamp DESC) AS rn_desc
			FROM meter_readings
			WHERE timestamp >= ?
		)
		SELECT
			hour,
			MAX(CASE WHEN rn_desc = 1 THEN energy_wh END) -
			MAX(CASE WHEN rn_asc  = 1 THEN energy_wh END) AS consumed_wh,
			AVG(power_w) AS avg_power
		FROM hourly
		GROUP BY hour
		ORDER BY hour
	`, since)
	if err != nil {
		return nil, fmt.Errorf("query hourly consumption: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]HourlyEnergy, 0)
	for rows.Next() {
		var he HourlyEnergy
		var hourStr string
		if err := rows.Scan(&hourStr, &he.EnergyWh, &he.PowerW); err != nil {
			return nil, fmt.Errorf("scan hourly: %w", err)
		}
		he.Hour, _ = time.Parse(time.RFC3339, hourStr)
		result = append(result, he)
	}
	return result, rows.Err()
}

// PhaseReading is a single per-phase power snapshot.
type PhaseReading struct {
	Timestamp time.Time `json:"timestamp"`
	Phase1W   float64   `json:"phase1W"`
	Phase2W   float64   `json:"phase2W"`
	Phase3W   float64   `json:"phase3W"`
}

// InsertPhaseReading stores per-phase power readings.
func (s *Store) InsertPhaseReading(r PhaseReading) error {
	_, err := s.db.Exec(
		`INSERT INTO phase_readings (timestamp, phase1_w, phase2_w, phase3_w) VALUES (?, ?, ?, ?)`,
		r.Timestamp.UTC().Format(time.RFC3339),
		r.Phase1W, r.Phase2W, r.Phase3W,
	)
	if err != nil {
		return fmt.Errorf("insert phase reading: %w", err)
	}
	return nil
}

// RecentPhaseReadings returns phase readings for the last N minutes.
func (s *Store) RecentPhaseReadings(minutes int) ([]PhaseReading, error) {
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).UTC().Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT timestamp, phase1_w, phase2_w, phase3_w FROM phase_readings WHERE timestamp >= ? ORDER BY timestamp ASC`,
		since,
	)
	if err != nil {
		return nil, fmt.Errorf("query phase readings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]PhaseReading, 0)
	for rows.Next() {
		var r PhaseReading
		var ts string
		if err := rows.Scan(&ts, &r.Phase1W, &r.Phase2W, &r.Phase3W); err != nil {
			return nil, fmt.Errorf("scan phase reading: %w", err)
		}
		r.Timestamp, _ = time.Parse(time.RFC3339, ts)
		result = append(result, r)
	}
	return result, rows.Err()
}

// PurgePhaseReadings deletes phase readings older than the given duration.
func (s *Store) PurgePhaseReadings(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM phase_readings WHERE timestamp < ?`, cutoff)
	return err
}

// ChartMarker represents a timestamped event for the price chart.
type ChartMarker struct {
	Timestamp time.Time `json:"time"`
	Type      string    `json:"type"` // "start", "stop", "plug", "unplug"
}

// InsertChartMarker stores a chart marker event at the given timestamp.
// Pass time.Now() for events that fire at "now"; pass the actual event time
// for stop/start markers that should align with the underlying transition.
func (s *Store) InsertChartMarker(typ string, ts time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO chart_markers (timestamp, type) VALUES (?, ?)`,
		ts.UTC().Format(time.RFC3339), typ,
	)
	return err
}

// RecentChartMarkers returns markers from the last N hours.
func (s *Store) RecentChartMarkers(hours int) ([]ChartMarker, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT timestamp, type FROM chart_markers WHERE timestamp >= ? ORDER BY timestamp ASC`, since,
	)
	if err != nil {
		return nil, fmt.Errorf("query chart markers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var markers []ChartMarker
	for rows.Next() {
		var m ChartMarker
		var ts string
		if err := rows.Scan(&ts, &m.Type); err != nil {
			return nil, err
		}
		m.Timestamp, _ = time.Parse(time.RFC3339, ts)
		markers = append(markers, m)
	}
	return markers, rows.Err()
}

// PurgeChartMarkers deletes markers older than the given duration.
func (s *Store) PurgeChartMarkers(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM chart_markers WHERE timestamp < ?`, cutoff)
	return err
}

// MeterDeltaKWh returns energy consumed in [start, end), measured as the
// max-min of the cumulative energy_wh column. Returns 0 when fewer than
// two readings cover the window — the caller cannot tell consumption from
// no-data without that distinction, but for idle accounting "no rows" and
// "no consumption" both yield 0.
func (s *Store) MeterDeltaKWh(start, end time.Time) (float64, error) {
	var lo, hi float64
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(MIN(energy_wh), 0), COALESCE(MAX(energy_wh), 0)
		 FROM meter_readings
		 WHERE timestamp >= ? AND timestamp < ?`,
		start.UTC().Format(time.RFC3339),
		end.UTC().Format(time.RFC3339),
	).Scan(&n, &lo, &hi)
	if err != nil {
		return 0, fmt.Errorf("meter delta query: %w", err)
	}
	if n < 2 {
		return 0, nil
	}
	delta := hi - lo
	if delta < 0 {
		delta = 0
	}
	return delta / 1000.0, nil
}

// MeterReadingsCount returns the number of meter rows in [start, end). Used to
// distinguish "we have data and it shows zero" from "no data exists for this
// range" — the daily snapshot keeps the existing row in the latter case.
func (s *Store) MeterReadingsCount(start, end time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM meter_readings
		 WHERE timestamp >= ? AND timestamp < ?`,
		start.UTC().Format(time.RFC3339),
		end.UTC().Format(time.RFC3339),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("meter readings count: %w", err)
	}
	return n, nil
}

// earliestMeterTimestamp returns the timestamp of the oldest meter reading
// still in the table. ok=false when no rows exist. Used by reports to know
// where the live-meter regime ends and the daily_idle fallback begins.
func (s *Store) earliestMeterTimestamp() (time.Time, bool) {
	var ts string
	err := s.db.QueryRow(
		`SELECT timestamp FROM meter_readings ORDER BY id ASC LIMIT 1`,
	).Scan(&ts)
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// LatestMeterReading returns the most recent reading.
func (s *Store) LatestMeterReading() (*MeterReading, error) {
	var r MeterReading
	var ts string
	err := s.db.QueryRow(
		`SELECT timestamp, power_w, energy_wh FROM meter_readings ORDER BY id DESC LIMIT 1`,
	).Scan(&ts, &r.PowerW, &r.EnergyWh)
	if err != nil {
		return nil, err
	}
	r.Timestamp, _ = time.Parse(time.RFC3339, ts)
	return &r, nil
}
