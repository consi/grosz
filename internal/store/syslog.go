package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// SystemEvent represents a system-level event from any component.
type SystemEvent struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"` // "scheduler", "tariff", "renault", "meter", "zappi", "ocpp"
	Action    string    `json:"action"`
	Level     string    `json:"level"` // "info", "warn", "error"
	Input     any       `json:"input"`
	Result    any       `json:"result"`
}

// RecordSystemEvent persists a system event to the database.
func (s *Store) RecordSystemEvent(e SystemEvent) error {
	inputJSON, err := json.Marshal(e.Input)
	if err != nil {
		inputJSON = []byte("{}")
	}
	resultJSON, err := json.Marshal(e.Result)
	if err != nil {
		resultJSON = []byte("{}")
	}
	if e.Level == "" {
		e.Level = "info"
	}

	_, err = s.db.Exec(
		"INSERT INTO system_events (timestamp, source, action, level, input, result) VALUES (?, ?, ?, ?, ?, ?)",
		e.Timestamp.UTC().Format(time.RFC3339Nano),
		e.Source,
		e.Action,
		e.Level,
		string(inputJSON),
		string(resultJSON),
	)
	if err != nil {
		return fmt.Errorf("insert system event: %w", err)
	}
	return nil
}

// RecentSystemEvents returns the most recent n system events, newest first.
func (s *Store) RecentSystemEvents(n, offset int) ([]SystemEvent, error) {
	rows, err := s.db.Query(
		"SELECT id, timestamp, source, action, level, input, result FROM system_events ORDER BY id DESC LIMIT ? OFFSET ?",
		n, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query system events: %w", err)
	}
	defer rows.Close()

	return scanSystemEvents(rows)
}

// SystemEventsBySource returns system events filtered by source, newest first.
func (s *Store) SystemEventsBySource(source string, limit, offset int) ([]SystemEvent, error) {
	rows, err := s.db.Query(
		"SELECT id, timestamp, source, action, level, input, result FROM system_events WHERE source = ? ORDER BY id DESC LIMIT ? OFFSET ?",
		source, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query system events by source: %w", err)
	}
	defer rows.Close()

	return scanSystemEvents(rows)
}

// SystemEventCount returns the total number of system events, optionally filtered by source.
func (s *Store) SystemEventCount(source string) (int, error) {
	var count int
	var err error
	if source != "" {
		err = s.db.QueryRow("SELECT COUNT(*) FROM system_events WHERE source = ?", source).Scan(&count)
	} else {
		err = s.db.QueryRow("SELECT COUNT(*) FROM system_events").Scan(&count)
	}
	if err != nil {
		return 0, err
	}
	return count, nil
}

// PurgeOldSystemEvents deletes system events older than maxAge.
func (s *Store) PurgeOldSystemEvents(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	_, err := s.db.Exec("DELETE FROM system_events WHERE timestamp < ?", cutoff)
	return err
}

func scanSystemEvents(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]SystemEvent, error) {
	events := make([]SystemEvent, 0)
	for rows.Next() {
		var e SystemEvent
		var ts, input, result string
		if err := rows.Scan(&e.ID, &ts, &e.Source, &e.Action, &e.Level, &input, &result); err != nil {
			return nil, fmt.Errorf("scan system event: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			t = time.Now()
		}
		e.Timestamp = t

		var rawInput json.RawMessage
		if err := json.Unmarshal([]byte(input), &rawInput); err != nil {
			e.Input = input
		} else {
			e.Input = rawInput
		}

		var rawResult json.RawMessage
		if err := json.Unmarshal([]byte(result), &rawResult); err != nil {
			e.Result = result
		} else {
			e.Result = rawResult
		}

		events = append(events, e)
	}
	return events, rows.Err()
}
