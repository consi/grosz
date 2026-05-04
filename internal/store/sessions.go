package store

import (
	"database/sql"
	"fmt"
	"time"
)

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
	Status        string     `json:"status"` // "active", "completed", "error"
	Distance      float64    `json:"distance,omitempty"`      // km driven since previous charge (computed)
	KWhPer100km   float64    `json:"kwhPer100km,omitempty"`   // consumption (computed)
}

// EnrichWithOdometer computes distance and consumption for each session
// based on odometer deltas between consecutive sessions.
// Sessions must be sorted newest-first (as returned by query).
func (s *Store) EnrichWithOdometer(sessions []Session) {
	for i := range sessions {
		if sessions[i].Status != "completed" || sessions[i].Energy <= 0 {
			continue
		}
		// Previous session in time is at i+1 (list is newest-first)
		var prevStop time.Time
		if i+1 < len(sessions) && sessions[i+1].StopTime != nil {
			prevStop = *sessions[i+1].StopTime
		}
		if prevStop.IsZero() {
			continue
		}
		dist, _ := s.OdometerDelta(prevStop, sessions[i].StartTime)
		if dist > 0 {
			sessions[i].Distance = dist
			sessions[i].KWhPer100km = (sessions[i].Energy / dist) * 100
		}
	}
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

// StopSession completes a charging session.
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
	return nil
}

// ActiveSession returns the currently active session, if any.
func (s *Store) ActiveSession() (*Session, error) {
	return s.scanSession(
		s.db.QueryRow(
			`SELECT id, charge_box, connector_id, transaction_id, id_tag, start_time, stop_time, meter_start, meter_stop, energy, cost, status
			 FROM charging_sessions WHERE status = 'active' LIMIT 1`,
		),
	)
}

// SessionHistory returns sessions, newest first, with pagination.
func (s *Store) SessionHistory(limit, offset int) ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, charge_box, connector_id, transaction_id, id_tag, start_time, stop_time, meter_start, meter_stop, energy, cost, status
		 FROM charging_sessions ORDER BY id DESC LIMIT ? OFFSET ?`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

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
		`SELECT id, charge_box, connector_id, transaction_id, id_tag, start_time, stop_time, meter_start, meter_stop, energy, cost, status
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
	defer rows.Close()

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
	CostPer100km   float64 `json:"costPer100km"`   // PLN/100km
	WhPerKm        float64 `json:"whPerKm"`        // energy efficiency
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

	// Compute total meter consumption for the period
	var totalMeterWh float64
	meterRow := s.db.QueryRow(
		`SELECT COALESCE(MAX(energy_wh) - MIN(energy_wh), 0)
		 FROM meter_readings
		 WHERE timestamp >= ? AND timestamp < ?`,
		from.UTC().Format(time.RFC3339),
		to.UTC().Format(time.RFC3339),
	)
	_ = meterRow.Scan(&totalMeterWh)
	totalMeterKWh := totalMeterWh / 1000.0

	// Idle = total meter consumption minus charging
	if totalMeterKWh > r.TotalEnergy {
		r.IdleEnergy = totalMeterKWh - r.TotalEnergy
		r.IdleCost = s.CalculateSessionCost(from, to, r.IdleEnergy)
	}

	// Fall back to persisted daily_idle when meter data is purged
	if r.IdleEnergy == 0 {
		r.IdleEnergy, r.IdleCost, _ = s.SumDailyIdle(from, to)
	}

	// External costs
	r.ExternalCosts, _ = s.ExternalCostSumByDateRange(from, to)
	r.GrandTotalCost = r.TotalCost + r.IdleCost + r.ExternalCosts

	// Distance and efficiency
	r.Distance, _ = s.OdometerDelta(from, to)
	if r.Distance > 0 {
		r.CostPer100km = r.GrandTotalCost / (r.Distance / 100)
		r.WhPerKm = (r.TotalEnergy * 1000) / r.Distance
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

	err := row.Scan(
		&sess.ID, &sess.ChargeBox, &sess.ConnectorID, &sess.TransactionID,
		&idTag, &startStr, &stopStr, &sess.MeterStart,
		&meterStop, &energy, &cost, &sess.Status,
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

	err := scanner.Scan(
		&sess.ID, &sess.ChargeBox, &sess.ConnectorID, &sess.TransactionID,
		&idTag, &startStr, &stopStr, &sess.MeterStart,
		&meterStop, &energy, &cost, &sess.Status,
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

	return sess, nil
}
