package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Event represents an OCPP message event.
type Event struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Direction string    `json:"direction"` // "recv" or "send"
	ChargeBox string    `json:"chargeBox"`
	Action    string    `json:"action"`
	Payload   any       `json:"payload"`
}

// RecordEvent persists an OCPP event to the database.
func (s *Store) RecordEvent(e Event) error {
	payloadJSON, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	_, err = s.db.Exec(
		"INSERT INTO ocpp_events (timestamp, direction, charge_box, action, payload) VALUES (?, ?, ?, ?, ?)",
		e.Timestamp.UTC().Format(time.RFC3339Nano),
		e.Direction,
		e.ChargeBox,
		e.Action,
		string(payloadJSON),
	)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// RecentEvents returns the most recent n events, newest first, with offset for pagination.
func (s *Store) RecentEvents(n, offset int) ([]Event, error) {
	rows, err := s.db.Query(
		"SELECT id, timestamp, direction, charge_box, action, payload FROM ocpp_events ORDER BY id DESC LIMIT ? OFFSET ?",
		n, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

// EventsByAction returns events filtered by action name, newest first, with offset.
func (s *Store) EventsByAction(action string, limit, offset int) ([]Event, error) {
	rows, err := s.db.Query(
		"SELECT id, timestamp, direction, charge_box, action, payload FROM ocpp_events WHERE action = ? ORDER BY id DESC LIMIT ? OFFSET ?",
		action, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query events by action: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

// EventCount returns the total number of events, optionally filtered by action.
func (s *Store) EventCount(action string) (int, error) {
	var count int
	var err error
	if action != "" {
		err = s.db.QueryRow("SELECT COUNT(*) FROM ocpp_events WHERE action = ?", action).Scan(&count)
	} else {
		err = s.db.QueryRow("SELECT COUNT(*) FROM ocpp_events").Scan(&count)
	}
	if err != nil {
		return 0, err
	}
	return count, nil
}

// LatestEnergyForTransaction parses ocpp_events to find the most recent
// Energy.Active.Import.Register reading for the given OCPP transaction.
// Used by the suspend-finalize fallback and the startup backfill, where the
// in-memory measurements are unavailable (e.g. after a websocket reconnect
// wiped state, or after a process restart). Prefers an unphased reading,
// falls back to phase=N (Zappi's convention), then to L1+L2+L3 sum.
//
// Returns kWh, the message timestamp, and ok=false if nothing found.
func (s *Store) LatestEnergyForTransaction(txnID int) (float64, time.Time, bool) {
	rows, err := s.db.Query(
		`SELECT timestamp, payload FROM ocpp_events
		 WHERE action = 'MeterValues'
		   AND payload LIKE ?
		 ORDER BY id DESC LIMIT 64`,
		fmt.Sprintf(`%%"transactionId":%d%%`, txnID),
	)
	if err != nil {
		return 0, time.Time{}, false
	}
	defer rows.Close()

	for rows.Next() {
		var tsStr, payload string
		if err := rows.Scan(&tsStr, &payload); err != nil {
			continue
		}
		var mv struct {
			TransactionID int `json:"transactionId"`
			MeterValue    []struct {
				Timestamp    string `json:"timestamp"`
				SampledValue []struct {
					Value     string `json:"value"`
					Measurand string `json:"measurand"`
					Phase     string `json:"phase"`
					Unit      string `json:"unit"`
				} `json:"sampledValue"`
			} `json:"meterValue"`
		}
		if err := json.Unmarshal([]byte(payload), &mv); err != nil {
			continue
		}
		if mv.TransactionID != txnID {
			// payload LIKE matches loosely — verify the actual field.
			continue
		}
		kwh, mvTS, ok := extractEnergyKWh(mv.MeterValue, tsStr)
		if ok {
			return kwh, mvTS, true
		}
	}
	return 0, time.Time{}, false
}

// extractEnergyKWh walks a MeterValues payload and pulls the latest
// Energy.Active.Import.Register reading, converting Wh → kWh. Returns the
// reading's wall-clock timestamp (falls back to the message timestamp).
func extractEnergyKWh(meterValue []struct {
	Timestamp    string `json:"timestamp"`
	SampledValue []struct {
		Value     string `json:"value"`
		Measurand string `json:"measurand"`
		Phase     string `json:"phase"`
		Unit      string `json:"unit"`
	} `json:"sampledValue"`
}, msgTS string) (float64, time.Time, bool) {
	const target = "Energy.Active.Import.Register"
	var unphased, phaseN float64
	var l1, l2, l3 float64
	var hasUnphased, hasN, hasL1, hasL2, hasL3 bool
	var bestTS string

	for _, mv := range meterValue {
		if mv.Timestamp != "" {
			bestTS = mv.Timestamp
		}
		for _, sv := range mv.SampledValue {
			if sv.Measurand != target {
				continue
			}
			val, err := strconv.ParseFloat(sv.Value, 64)
			if err != nil {
				continue
			}
			switch sv.Phase {
			case "":
				unphased = val
				hasUnphased = true
			case "N":
				phaseN = val
				hasN = true
			case "L1":
				l1 = val
				hasL1 = true
			case "L2":
				l2 = val
				hasL2 = true
			case "L3":
				l3 = val
				hasL3 = true
			}
		}
	}

	at := parseEventTime(bestTS, msgTS)

	if hasUnphased {
		return unphased / 1000.0, at, true
	}
	if hasN {
		return phaseN / 1000.0, at, true
	}
	if hasL1 && hasL2 && hasL3 {
		return (l1 + l2 + l3) / 1000.0, at, true
	}
	return 0, time.Time{}, false
}

func parseEventTime(primary, fallback string) time.Time {
	for _, s := range []string{primary, fallback} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// PurgeOldEvents deletes events older than maxAge.
func (s *Store) PurgeOldEvents(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	_, err := s.db.Exec("DELETE FROM ocpp_events WHERE timestamp < ?", cutoff)
	return err
}

// EventsSince returns events after the given ID, oldest first (for SSE catch-up).
func (s *Store) EventsSince(afterID int64, limit int) ([]Event, error) {
	rows, err := s.db.Query(
		"SELECT id, timestamp, direction, charge_box, action, payload FROM ocpp_events WHERE id > ? ORDER BY id ASC LIMIT ?",
		afterID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query events since: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

func scanEvents(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]Event, error) {
	events := make([]Event, 0)
	for rows.Next() {
		var e Event
		var ts, payload string
		if err := rows.Scan(&e.ID, &ts, &e.Direction, &e.ChargeBox, &e.Action, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			t = time.Now()
		}
		e.Timestamp = t

		var raw json.RawMessage
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			e.Payload = payload
		} else {
			e.Payload = raw
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
