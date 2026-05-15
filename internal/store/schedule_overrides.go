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

	var existingID int64
	err = tx.QueryRow(
		`SELECT id FROM schedule_overrides
		 WHERE start_time < ? AND end_time > ?
		 LIMIT 1`,
		endStr, startStr,
	).Scan(&existingID)
	if err == nil {
		return 0, ErrOverlap
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("overlap check: %w", err)
	}

	res, err := tx.Exec(
		`INSERT INTO schedule_overrides (kind, start_time, end_time, power_w)
		 VALUES (?, ?, ?, ?)`,
		o.Kind, startStr, endStr, o.PowerW,
	)
	if err != nil {
		return 0, fmt.Errorf("insert override: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last id: %w", err)
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
