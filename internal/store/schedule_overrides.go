package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/consi/grosz/internal/events"
)

// ScheduleOverride is a user-asserted charging window or block applied on top
// of the auto-computed schedule. Forces (Kind=="force") are always honored;
// blocks (Kind=="block") exclude time from auto-selection. Overrides survive
// scheduler recomputes.
type ScheduleOverride struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	PowerW    float64   `json:"powerW"`
	CreatedAt time.Time `json:"createdAt"`
}

// OverrideKindForce marks a window where charging must occur.
const OverrideKindForce = "force"

// OverrideKindBlock marks a window where the auto-scheduler must not place charging.
const OverrideKindBlock = "block"

// ErrOverlap is returned by InsertOverride when the new override overlaps
// any existing one.
var ErrOverlap = errors.New("override overlaps existing")

// InsertOverride creates a new override after verifying it does not overlap
// any existing one. Returns the new ID.
func (s *Store) InsertOverride(o ScheduleOverride) (int64, error) {
	return s.SaveOverride(o, nil)
}

// SaveOverride atomically creates or updates a window. When moving an automatic
// period, block the portions of its original range outside the replacement.
func (s *Store) SaveOverride(o ScheduleOverride, original *ScheduleOverride) (int64, error) {
	return s.saveOverride(o, original, false)
}

// ApplyOverride makes the latest explicit intent win inside its time range.
// Overlapping windows are trimmed, retaining everything outside the selection.
func (s *Store) ApplyOverride(o ScheduleOverride, original *ScheduleOverride) (int64, error) {
	return s.saveOverride(o, original, true)
}

func (s *Store) saveOverride(o ScheduleOverride, original *ScheduleOverride, replace bool) (int64, error) {
	if !o.End.After(o.Start) {
		return 0, fmt.Errorf("end must be after start")
	}
	if o.Kind != OverrideKindForce && o.Kind != OverrideKindBlock {
		return 0, fmt.Errorf("invalid kind: %q", o.Kind)
	}
	if o.Kind == OverrideKindForce && o.PowerW <= 0 {
		return 0, fmt.Errorf("force override requires power_w > 0")
	}

	startStr := o.Start.UTC().Format(time.RFC3339)
	endStr := o.End.UTC().Format(time.RFC3339)

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if o.ID != 0 {
		var id int64
		if err := tx.QueryRow(`SELECT id FROM schedule_overrides WHERE id = ?`, o.ID).Scan(&id); err != nil {
			return 0, err
		}
	}

	checkOverlap := func(start, end time.Time) error {
		if replace {
			return trimOverrides(tx, start, end, o.ID)
		}
		var id int64
		err := tx.QueryRow(`SELECT id FROM schedule_overrides
   WHERE id != ? AND start_time < ? AND end_time > ? LIMIT 1`,
			o.ID, end.UTC().Format(time.RFC3339), start.UTC().Format(time.RFC3339)).Scan(&id)
		if err == nil {
			return ErrOverlap
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	}
	if err := checkOverlap(o.Start, o.End); err != nil {
		return 0, err
	}
	if original != nil {
		if !original.End.After(original.Start) {
			return 0, fmt.Errorf("invalid original range")
		}
		// Block only the old range outside the replacement, keeping all overrides disjoint.
		parts := [][2]time.Time{}
		if original.Start.Before(o.Start) {
			parts = append(parts, [2]time.Time{original.Start, minTime(original.End, o.Start)})
		}
		if original.End.After(o.End) {
			parts = append(parts, [2]time.Time{maxTime(original.Start, o.End), original.End})
		}
		for _, part := range parts {
			if err := checkOverlap(part[0], part[1]); err != nil {
				return 0, err
			}
			if _, err := tx.Exec(`INSERT INTO schedule_overrides (kind, start_time, end_time, power_w) VALUES ('block', ?, ?, 0)`, part[0].UTC().Format(time.RFC3339), part[1].UTC().Format(time.RFC3339)); err != nil {
				return 0, err
			}
		}
	}
	id := o.ID
	if id != 0 {
		res, err := tx.Exec(`UPDATE schedule_overrides SET kind = ?, start_time = ?, end_time = ?, power_w = ? WHERE id = ?`, o.Kind, startStr, endStr, o.PowerW, id)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, sql.ErrNoRows
		}
	} else {
		res, err := tx.Exec(`INSERT INTO schedule_overrides (kind, start_time, end_time, power_w) VALUES (?, ?, ?, ?)`, o.Kind, startStr, endStr, o.PowerW)
		if err != nil {
			return 0, fmt.Errorf("insert override: %w", err)
		}
		id, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// DeleteOverride removes an override by ID. Returns sql.ErrNoRows if missing.
func (s *Store) DeleteOverride(id int64) error {
	res, err := s.db.Exec(`DELETE FROM schedule_overrides WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete override: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// LoadOverrides returns all overrides whose End is after `after`, ordered by start_time.
func (s *Store) LoadOverrides(after time.Time) ([]ScheduleOverride, error) {
	rows, err := s.db.Query(
		`SELECT id, kind, start_time, end_time, power_w, created_at
		 FROM schedule_overrides
		 WHERE end_time > ?
		 ORDER BY start_time ASC`,
		after.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("query overrides: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ScheduleOverride
	for rows.Next() {
		var o ScheduleOverride
		var startStr, endStr, createdStr string
		if err := rows.Scan(&o.ID, &o.Kind, &startStr, &endStr, &o.PowerW, &createdStr); err != nil {
			return nil, fmt.Errorf("scan override: %w", err)
		}
		o.Start, _ = time.Parse(time.RFC3339, startStr)
		o.End, _ = time.Parse(time.RFC3339, endStr)
		if t, err := time.Parse("2006-01-02 15:04:05", createdStr); err == nil {
			o.CreatedAt = t
		} else if t, err := time.Parse(time.RFC3339, createdStr); err == nil {
			o.CreatedAt = t
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PurgeOldOverrides removes overrides whose End is before the cutoff.
func (s *Store) PurgeOldOverrides(before time.Time) error {
	res, err := s.db.Exec(
		`DELETE FROM schedule_overrides WHERE end_time < ?`,
		before.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return err
	}
	emitPurgeBefore(s, events.ActionPurgeOverrides, before, res)
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// trimOverrides removes only the covered portion, splitting an old window when needed.
func trimOverrides(tx *sql.Tx, start, end time.Time, exceptID int64) error {
	startStr, endStr := start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)
	rows, err := tx.Query(`SELECT id, kind, start_time, end_time, power_w FROM schedule_overrides WHERE id != ? AND start_time < ? AND end_time > ?`, exceptID, endStr, startStr)
	if err != nil {
		return err
	}
	var overlaps []ScheduleOverride
	for rows.Next() {
		var o ScheduleOverride
		var a, b string
		if err := rows.Scan(&o.ID, &o.Kind, &a, &b, &o.PowerW); err != nil {
			_ = rows.Close()
			return err
		}
		o.Start, err = time.Parse(time.RFC3339, a)
		if err != nil {
			_ = rows.Close()
			return err
		}
		o.End, err = time.Parse(time.RFC3339, b)
		if err != nil {
			_ = rows.Close()
			return err
		}
		overlaps = append(overlaps, o)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, o := range overlaps {
		left, right := o.Start.Before(start), o.End.After(end)
		switch {
		case left:
			_, err = tx.Exec(`UPDATE schedule_overrides SET end_time = ? WHERE id = ?`, startStr, o.ID)
			if err == nil && right {
				_, err = tx.Exec(`INSERT INTO schedule_overrides (kind,start_time,end_time,power_w) VALUES (?,?,?,?)`, o.Kind, endStr, o.End.UTC().Format(time.RFC3339), o.PowerW)
			}
		case right:
			_, err = tx.Exec(`UPDATE schedule_overrides SET start_time = ? WHERE id = ?`, endStr, o.ID)
		default:
			_, err = tx.Exec(`DELETE FROM schedule_overrides WHERE id = ?`, o.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ResetSchedule clears both generations of cancellation state and pauses
// automatic scheduling. Explicit manual windows remain available afterwards.
func (s *Store) ResetSchedule() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`DELETE FROM schedule_overrides`); err != nil {
		return err
	}
	for key, value := range map[string]string{"scheduler.paused": "true", "scheduler.persisted_schedule": ""} {
		if _, err = tx.Exec(`INSERT INTO settings (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}
