package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/consi/grosz/internal/events"
)

// SystemEvent re-exports events.SystemEvent so existing code that
// references store.SystemEvent keeps compiling during the migration.
// Prefer importing events directly in new code.
type SystemEvent = events.SystemEvent

// RecordSystemEvent persists a system event to the database.
func (s *Store) RecordSystemEvent(e events.SystemEvent) error {
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
func (s *Store) RecentSystemEvents(n, offset int) ([]events.SystemEvent, error) {
	rows, err := s.db.Query(
		"SELECT id, timestamp, source, action, level, input, result FROM system_events ORDER BY id DESC LIMIT ? OFFSET ?",
		n, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query system events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return scanSystemEvents(rows)
}

// SystemEventsBySource returns system events filtered by source, newest first.
func (s *Store) SystemEventsBySource(source string, limit, offset int) ([]events.SystemEvent, error) {
	rows, err := s.db.Query(
		"SELECT id, timestamp, source, action, level, input, result FROM system_events WHERE source = ? ORDER BY id DESC LIMIT ? OFFSET ?",
		source, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query system events by source: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// DistinctSystemEventSources returns the set of source values currently
// present in the system_events table, sorted ascending. Used by the UI
// to populate the source filter dropdown dynamically.
func (s *Store) DistinctSystemEventSources() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT source FROM system_events ORDER BY source ASC")
	if err != nil {
		return nil, fmt.Errorf("query distinct sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0)
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, fmt.Errorf("scan distinct source: %w", err)
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// PurgeOldSystemEvents deletes system events older than maxAge.
func (s *Store) PurgeOldSystemEvents(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339)
	res, err := s.db.Exec("DELETE FROM system_events WHERE timestamp < ?", cutoff)
	if err != nil {
		return err
	}
	emitPurge(s, events.ActionPurgeSystemEvents, maxAge, res)
	return nil
}

// emitPurge records a system event for a successful purge, only when at least
// one row was actually deleted. Keeps the syslog from filling with hourly
// "no-op" entries during normal operation. Lives next to the syslog impl
// because it's reused by every Purge* method on the store.
func emitPurge(s *Store, action events.Action, maxAge time.Duration, res sql.Result) {
	emitPurgeWithCutoff(s, action,
		map[string]any{"maxAgeHours": maxAge.Hours()},
		res,
	)
}

// emitPurgeBefore is the time.Time equivalent of emitPurge, for purges whose
// callers compute the cutoff explicitly rather than passing a maxAge duration
// (e.g. PurgeOldRates, PurgeOldOverrides).
func emitPurgeBefore(s *Store, action events.Action, before time.Time, res sql.Result) {
	emitPurgeWithCutoff(s, action,
		map[string]any{"before": before.UTC().Format(time.RFC3339)},
		res,
	)
}

func emitPurgeWithCutoff(s *Store, action events.Action, input map[string]any, res sql.Result) {
	if res == nil {
		return
	}
	rows, _ := res.RowsAffected()
	if rows <= 0 {
		return
	}
	events.Info(s, events.SourceStore, action, input,
		map[string]any{"rowsDeleted": rows},
	)
}

func scanSystemEvents(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]events.SystemEvent, error) {
	out := make([]events.SystemEvent, 0)
	for rows.Next() {
		var e events.SystemEvent
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

		out = append(out, e)
	}
	return out, rows.Err()
}
