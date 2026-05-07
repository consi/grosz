package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ConsumptionWindowDays is the trailing window used for moving-average
// efficiency metrics (kWh/100km and Wh/km).
const ConsumptionWindowDays = 30

// Session represents a charging session (transaction).
type Session struct {
	ID            int64      `json:"id"`
	ChargeBox     string     `json:"chargeBox"`
	ConnectorID   int        `json:"connectorId"`
	TransactionID int        `json:"transactionId"`
	IdTag         string     `json:"idTag,omitempty"`
	StartTime     time.Time  `json:"startTime"`
	StopTime      *time.Time `json:"stopTime,omitempty"`
	MeterStart    float64    `json:"meterStart"`
	MeterStop     float64    `json:"meterStop,omitempty"`
	Energy        float64    `json:"energy"`
	Cost          float64    `json:"cost,omitempty"`
	Status        string     `json:"status"`                // "active", "completed", "error"
	Distance      float64    `json:"distance,omitempty"`    // km driven between previous session's stop and this session's start
	KWhPer100km   float64    `json:"kwhPer100km,omitempty"` // trailing 30-day moving average kWh/100km
}

// ConsumptionWindow returns total charging energy (kWh) and odometer distance
// (km) over the trailing ConsumptionWindowDays window ending at anchor. The
// window is half-open (from, anchor] so the anchor itself is included.
// ok is true only when both numerator and denominator are positive.
func (s *Store) ConsumptionWindow(anchor time.Time) (energy, distance float64, ok bool, err error) {
	from := anchor.Add(-time.Duration(ConsumptionWindowDays) * 24 * time.Hour)
	fromStr := from.UTC().Format(time.RFC3339)
	toStr := anchor.UTC().Format(time.RFC3339)

	if err = s.db.QueryRow(
		`SELECT COALESCE(SUM(energy), 0) FROM charging_sessions
		 WHERE start_time > ? AND start_time <= ? AND status = 'completed'`,
		fromStr, toStr,
	).Scan(&energy); err != nil {
		return 0, 0, false, fmt.Errorf("consumption window energy: %w", err)
	}

	if err = s.db.QueryRow(
		`SELECT COALESCE(MAX(mileage) - MIN(mileage), 0) FROM odometer_readings
		 WHERE timestamp > ? AND timestamp <= ?`,
		fromStr, toStr,
	).Scan(&distance); err != nil {
		return 0, 0, false, fmt.Errorf("consumption window distance: %w", err)
	}
	if distance < 0 {
		distance = 0
	}
	ok = energy > 0 && distance > 0
	return energy, distance, ok, nil
}

// StartSession inserts a new active charging session.
func (s *Store) StartSession(sess Session) error {
	_, err := s.db.Exec(
		`INSERT INTO charging_sessions (charge_box, connector_id, transaction_id, id_tag, start_time, meter_start, status)
		 VALUES (?, ?, ?, ?, ?, ?, 'active')`,
		sess.ChargeBox,
		sess.ConnectorID,
		sess.TransactionID,
		sess.IdTag,
		sess.StartTime.UTC().Format(time.RFC3339),
		sess.MeterStart,
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// StopSession completes a charging session and persists the per-session
// distance (km since previous session) and the trailing 30-day kWh/100km.
func (s *Store) StopSession(txnID int, stopTime time.Time, meterStop, energy, cost float64) error {
	result, err := s.db.Exec(
		`UPDATE charging_sessions SET stop_time = ?, meter_stop = ?, energy = ?, cost = ?, status = 'completed'
		 WHERE transaction_id = ? AND status = 'active'`,
		stopTime.UTC().Format(time.RFC3339),
		meterStop,
		energy,
		cost,
		txnID,
	)
	if err != nil {
		return fmt.Errorf("stop session: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}

	// Look up the row we just stopped so we know its id and start_time.
	var sessID int64
	var startStr string
	if err := s.db.QueryRow(
		`SELECT id, start_time FROM charging_sessions WHERE transaction_id = ? AND status = 'completed'`,
		txnID,
	).Scan(&sessID, &startStr); err != nil {
		return nil // best-effort: persistence of derived metrics never fails the stop
	}
	startTime, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return nil
	}

	// Per-session distance: km between previous completed session's stop and this session's start.
	var distance float64
	var prevStopStr sql.NullString
	_ = s.db.QueryRow(
		`SELECT MAX(stop_time) FROM charging_sessions
		 WHERE status = 'completed' AND stop_time IS NOT NULL AND id <> ? AND stop_time < ?`,
		sessID, startTime.UTC().Format(time.RFC3339),
	).Scan(&prevStopStr)
	if prevStopStr.Valid {
		if prevStop, err := time.Parse(time.RFC3339, prevStopStr.String); err == nil {
			distance, _ = s.OdometerDelta(prevStop, startTime)
		}
	}

	// Trailing 30-day kWh/100km anchored at stop time.
	var kwhPer100 sql.NullFloat64
	if winEnergy, winDist, ok, _ := s.ConsumptionWindow(stopTime); ok {
		kwhPer100 = sql.NullFloat64{Float64: (winEnergy / winDist) * 100, Valid: true}
	}

	if _, err := s.db.Exec(
		`UPDATE charging_sessions SET distance_km = ?, kwh_per_100km = ? WHERE id = ?`,
		distance, kwhPer100, sessID,
	); err != nil {
		return nil
	}
	return nil
}

// SessionsOverlapping reports whether any session intersects [start, end).
// An active session (no stop_time) overlaps as long as its start_time is
// before end. Used by the scheduler to detect "missed" planned periods —
// when a scheduled charging hour elapsed without any session touching it.
func (s *Store) SessionsOverlapping(start, end time.Time) (bool, error) {
	startStr := start.UTC().Format(time.RFC3339)
	endStr := end.UTC().Format(time.RFC3339)
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM charging_sessions
		 WHERE start_time < ?
		   AND (stop_time IS NULL OR stop_time > ?)`,
		endStr, startStr,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("sessions overlapping: %w", err)
	}
	return count > 0, nil
}

// ActiveSession returns the currently active session, if any.
func (s *Store) ActiveSession() (*Session, error) {
	return s.scanSession(
		s.db.QueryRow(
			`SELECT id, charge_box, connector_id, transaction_id, id_tag, start_time, stop_time, meter_start, meter_stop, energy, cost, status, distance_km, kwh_per_100km
			 FROM charging_sessions WHERE status = 'active' LIMIT 1`,
		),
	)
}

// SessionHistory returns sessions, newest first, with pagination.
func (s *Store) SessionHistory(limit, offset int) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, charge_box, connector_id, transaction_id, id_tag, start_time, stop_time, meter_start, meter_stop, energy, cost, status, distance_km, kwh_per_100km
		 FROM charging_sessions ORDER BY id DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sessions := make([]Session, 0)
	for rows.Next() {
		sess, err := s.scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// SessionsByDateRange returns sessions within a date range, newest first.
func (s *Store) SessionsByDateRange(from, to time.Time, limit, offset int) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, charge_box, connector_id, transaction_id, id_tag, start_time, stop_time, meter_start, meter_stop, energy, cost, status, distance_km, kwh_per_100km
		 FROM charging_sessions
		 WHERE start_time >= ? AND start_time < ?
		 ORDER BY id DESC LIMIT ? OFFSET ?`,
		from.UTC().Format(time.RFC3339),
		to.UTC().Format(time.RFC3339),
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions by range: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sessions := make([]Session, 0)
	for rows.Next() {
		sess, err := s.scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// SessionReport holds aggregated session statistics.
type SessionReport struct {
	TotalSessions  int     `json:"totalSessions"`
	TotalEnergy    float64 `json:"totalEnergy"`    // kWh (charging only)
	TotalCost      float64 `json:"totalCost"`      // PLN (charging only)
	AvgCostPerKWh  float64 `json:"avgCostPerKwh"`  // PLN/kWh
	TotalDuration  float64 `json:"totalDuration"`  // hours
	IdleEnergy     float64 `json:"idleEnergy"`     // kWh (non-charging consumption)
	IdleCost       float64 `json:"idleCost"`       // PLN
	ExternalCosts  float64 `json:"externalCosts"`  // PLN (manually added)
	GrandTotalCost float64 `json:"grandTotalCost"` // charging + idle + external
	Distance       float64 `json:"distance"`       // km (odometer delta)
	CostPer100km   float64 `json:"costPer100km"`   // PLN/100km (range-based)
	KWhPer100km    float64 `json:"kwhPer100km"`    // trailing 30-day moving average kWh/100km
}

// SessionReportByRange computes aggregated stats for sessions in a date range,
// including idle (non-charging) consumption from meter readings.
func (s *Store) SessionReportByRange(from, to time.Time) (*SessionReport, error) {
	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(energy), 0), COALESCE(SUM(cost), 0),
		        COALESCE(SUM((julianday(stop_time) - julianday(start_time)) * 24), 0)
		 FROM charging_sessions
		 WHERE start_time >= ? AND start_time < ? AND status = 'completed'`,
		from.UTC().Format(time.RFC3339),
		to.UTC().Format(time.RFC3339),
	)

	var r SessionReport
	if err := row.Scan(&r.TotalSessions, &r.TotalEnergy, &r.TotalCost, &r.TotalDuration); err != nil {
		return nil, fmt.Errorf("session report: %w", err)
	}
	if r.TotalEnergy > 0 {
		r.AvgCostPerKWh = r.TotalCost / r.TotalEnergy
	}

	// Idle = sum of meter deltas in non-charging windows. For ranges that
	// extend past the meter_readings retention horizon, the older portion
	// falls back to the daily_idle snapshot (which is already kept on the
	// new formula by the snapshot loop).
	earliestMeter, hasMeter := s.earliestMeterTimestamp()
	liveStart := from
	if hasMeter && earliestMeter.After(liveStart) {
		liveStart = earliestMeter
	}

	if liveStart.Before(to) {
		windows, err := s.nonChargingWindows(liveStart, to)
		if err != nil {
			return nil, fmt.Errorf("idle windows: %w", err)
		}
		for _, w := range windows {
			delta, err := s.MeterDeltaKWh(w.Start, w.End)
			if err != nil {
				return nil, err
			}
			if delta <= 0 {
				continue
			}
			r.IdleEnergy += delta
			r.IdleCost += s.CalculateSessionCost(w.Start, w.End, delta)
		}
	}

	// Fold in the snapshot-only portion (days before retention) when present.
	if hasMeter && earliestMeter.After(from) {
		oldEnergy, oldCost, _ := s.SumDailyIdle(from, earliestMeter)
		r.IdleEnergy += oldEnergy
		r.IdleCost += oldCost
	} else if !hasMeter {
		// No meter data at all in the range — fall back entirely.
		r.IdleEnergy, r.IdleCost, _ = s.SumDailyIdle(from, to)
	}

	// External costs
	r.ExternalCosts, _ = s.ExternalCostSumByDateRange(from, to)
	r.GrandTotalCost = r.TotalCost + r.IdleCost + r.ExternalCosts

	// Distance (range-based) and PLN/100km (range-based accounting figure).
	r.Distance, _ = s.OdometerDelta(from, to)
	if r.Distance > 0 {
		r.CostPer100km = r.GrandTotalCost / (r.Distance / 100)
	}

	// kWh/100km uses trailing 30-day moving average anchored at `to`, so the
	// efficiency indicator is independent of the report range.
	if winEnergy, winDist, ok, _ := s.ConsumptionWindow(to); ok {
		r.KWhPer100km = (winEnergy / winDist) * 100
	}

	return &r, nil
}

// SessionCount returns total session count, optionally in a date range.
func (s *Store) SessionCount(from, to *time.Time) (int, error) {
	var count int
	var err error
	if from != nil && to != nil {
		err = s.db.QueryRow(
			"SELECT COUNT(*) FROM charging_sessions WHERE start_time >= ? AND start_time < ?",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339),
		).Scan(&count)
	} else {
		err = s.db.QueryRow("SELECT COUNT(*) FROM charging_sessions").Scan(&count)
	}
	return count, err
}

func (s *Store) scanSession(row *sql.Row) (*Session, error) {
	var sess Session
	var startStr string
	var stopStr sql.NullString
	var idTag sql.NullString
	var meterStop, energy, cost sql.NullFloat64
	var distance, kwhPer100 sql.NullFloat64

	err := row.Scan(
		&sess.ID, &sess.ChargeBox, &sess.ConnectorID, &sess.TransactionID,
		&idTag, &startStr, &stopStr, &sess.MeterStart,
		&meterStop, &energy, &cost, &sess.Status,
		&distance, &kwhPer100,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}

	sess.StartTime, _ = time.Parse(time.RFC3339, startStr)
	if idTag.Valid {
		sess.IdTag = idTag.String
	}
	if stopStr.Valid {
		t, _ := time.Parse(time.RFC3339, stopStr.String)
		sess.StopTime = &t
	}
	if meterStop.Valid {
		sess.MeterStop = meterStop.Float64
	}
	if energy.Valid {
		sess.Energy = energy.Float64
	}
	if cost.Valid {
		sess.Cost = cost.Float64
	}
	if distance.Valid {
		sess.Distance = distance.Float64
	}
	if kwhPer100.Valid {
		sess.KWhPer100km = kwhPer100.Float64
	}
	return &sess, nil
}

func (s *Store) scanSessionRow(row interface{ Scan(dest ...any) error }) (Session, error) {
	return s.scanSessionFromScanner(row)
}

func (s *Store) scanSessionFromScanner(scanner interface{ Scan(dest ...any) error }) (Session, error) {
	var sess Session
	var startStr string
	var stopStr sql.NullString
	var idTag sql.NullString
	var meterStop, energy, cost sql.NullFloat64
	var distance, kwhPer100 sql.NullFloat64

	err := scanner.Scan(
		&sess.ID, &sess.ChargeBox, &sess.ConnectorID, &sess.TransactionID,
		&idTag, &startStr, &stopStr, &sess.MeterStart,
		&meterStop, &energy, &cost, &sess.Status,
		&distance, &kwhPer100,
	)
	if err != nil {
		return sess, fmt.Errorf("scan session: %w", err)
	}

	sess.StartTime, _ = time.Parse(time.RFC3339, startStr)
	if idTag.Valid {
		sess.IdTag = idTag.String
	}
	if stopStr.Valid {
		t, _ := time.Parse(time.RFC3339, stopStr.String)
		sess.StopTime = &t
	}
	if meterStop.Valid {
		sess.MeterStop = meterStop.Float64
	}
	if energy.Valid {
		sess.Energy = energy.Float64
	}
	if cost.Valid {
		sess.Cost = cost.Float64
	}
	if distance.Valid {
		sess.Distance = distance.Float64
	}
	if kwhPer100.Valid {
		sess.KWhPer100km = kwhPer100.Float64
	}

	return sess, nil
}
