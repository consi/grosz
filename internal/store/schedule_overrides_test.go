package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsertAndLoadOverrides(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	id1, err := s.InsertOverride(ScheduleOverride{
		Kind:   OverrideKindForce,
		Start:  now.Add(1 * time.Hour),
		End:    now.Add(2 * time.Hour),
		PowerW: 11000,
	})
	require.NoError(t, err)
	assert.Greater(t, id1, int64(0))

	id2, err := s.InsertOverride(ScheduleOverride{
		Kind:  OverrideKindBlock,
		Start: now.Add(3 * time.Hour),
		End:   now.Add(4 * time.Hour),
	})
	require.NoError(t, err)
	assert.Greater(t, id2, int64(0))

	loaded, err := s.LoadOverrides(now)
	require.NoError(t, err)
	require.Len(t, loaded, 2)
	assert.Equal(t, OverrideKindForce, loaded[0].Kind)
	assert.InDelta(t, 11000, loaded[0].PowerW, 0.001)
	assert.Equal(t, OverrideKindBlock, loaded[1].Kind)
}

func TestInsertOverrideRejectsOverlap(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	_, err := s.InsertOverride(ScheduleOverride{
		Kind:   OverrideKindForce,
		Start:  now.Add(1 * time.Hour),
		End:    now.Add(3 * time.Hour),
		PowerW: 11000,
	})
	require.NoError(t, err)

	cases := []struct {
		name        string
		start, end  time.Duration
		expectError bool
	}{
		{"identical", 1 * time.Hour, 3 * time.Hour, true},
		{"contained", 90 * time.Minute, 150 * time.Minute, true},
		{"overlaps_start", 30 * time.Minute, 90 * time.Minute, true},
		{"overlaps_end", 150 * time.Minute, 4 * time.Hour, true},
		{"adjacent_before", 0, 1 * time.Hour, false},
		{"adjacent_after", 3 * time.Hour, 4 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.InsertOverride(ScheduleOverride{
				Kind:  OverrideKindBlock,
				Start: now.Add(tc.start),
				End:   now.Add(tc.end),
			})
			if tc.expectError {
				assert.ErrorIs(t, err, ErrOverlap)
			} else {
				assert.NoError(t, err)
				_ = s.PurgeOldOverrides(now.Add(10 * time.Hour))
				_, _ = s.InsertOverride(ScheduleOverride{
					Kind:   OverrideKindForce,
					Start:  now.Add(1 * time.Hour),
					End:    now.Add(3 * time.Hour),
					PowerW: 11000,
				})
			}
		})
	}
}

func TestInsertOverrideValidation(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()

	_, err := s.InsertOverride(ScheduleOverride{Kind: "wrong", Start: now, End: now.Add(time.Hour)})
	require.Error(t, err)

	_, err = s.InsertOverride(ScheduleOverride{Kind: OverrideKindForce, Start: now, End: now})
	require.Error(t, err)

	_, err = s.InsertOverride(ScheduleOverride{Kind: OverrideKindForce, Start: now, End: now.Add(time.Hour), PowerW: 0})
	require.Error(t, err)
}

func TestDeleteOverride(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()

	id, err := s.InsertOverride(ScheduleOverride{
		Kind:  OverrideKindBlock,
		Start: now.Add(time.Hour), End: now.Add(2 * time.Hour),
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteOverride(id))

	err = s.DeleteOverride(id)
	assert.True(t, errors.Is(err, sql.ErrNoRows))

	loaded, err := s.LoadOverrides(now)
	require.NoError(t, err)
	assert.Empty(t, loaded)
}

func TestLoadOverridesFiltersByEnd(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()

	_, _ = s.InsertOverride(ScheduleOverride{
		Kind:  OverrideKindBlock,
		Start: now.Add(-2 * time.Hour), End: now.Add(-1 * time.Hour),
	})
	_, _ = s.InsertOverride(ScheduleOverride{
		Kind:  OverrideKindBlock,
		Start: now, End: now.Add(time.Hour),
	})

	loaded, err := s.LoadOverrides(now)
	require.NoError(t, err)
	assert.Len(t, loaded, 1)
}

func TestPurgeOldOverrides(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()

	_, _ = s.InsertOverride(ScheduleOverride{
		Kind:  OverrideKindBlock,
		Start: now.Add(-3 * time.Hour), End: now.Add(-2 * time.Hour),
	})
	_, _ = s.InsertOverride(ScheduleOverride{
		Kind: OverrideKindForce, PowerW: 11000,
		Start: now.Add(time.Hour), End: now.Add(2 * time.Hour),
	})

	require.NoError(t, s.PurgeOldOverrides(now.Add(-1*time.Hour)))

	loaded, err := s.LoadOverrides(now.Add(-10 * time.Hour))
	require.NoError(t, err)
	assert.Len(t, loaded, 1)
	assert.Equal(t, OverrideKindForce, loaded[0].Kind)
}

func TestSaveOverrideAtomicUpdate(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	first := ScheduleOverride{Kind: OverrideKindForce, Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), PowerW: 11000}
	id, err := s.InsertOverride(first)
	require.NoError(t, err)
	first.ID = id
	_, err = s.InsertOverride(ScheduleOverride{Kind: OverrideKindBlock, Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour)})
	require.NoError(t, err)
	moved := first
	moved.End = now.Add(210 * time.Minute)
	_, err = s.SaveOverride(moved, nil)
	require.ErrorIs(t, err, ErrOverlap)
	loaded, err := s.LoadOverrides(now)
	require.NoError(t, err)
	require.Len(t, loaded, 2)
	assert.Equal(t, first.Start, loaded[0].Start)
	assert.Equal(t, first.End, loaded[0].End, "failed edit must leave old window intact")
	moved.End = now.Add(150 * time.Minute)
	updatedID, err := s.SaveOverride(moved, nil)
	require.NoError(t, err)
	assert.Equal(t, id, updatedID)
	loaded, err = s.LoadOverrides(now)
	require.NoError(t, err)
	assert.Equal(t, moved.End, loaded[0].End)
	assert.Equal(t, id, loaded[0].ID)
}

func TestSaveOverrideReplacesAutomaticPeriod(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end time.Duration
		blocks     int
	}{
		{"shrink", 90 * time.Minute, 150 * time.Minute, 2},
		{"move", 4 * time.Hour, 5 * time.Hour, 1},
		{"extend", 30 * time.Minute, 210 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			original := ScheduleOverride{Start: now.Add(time.Hour), End: now.Add(3 * time.Hour)}
			_, err := s.SaveOverride(ScheduleOverride{Kind: OverrideKindForce, Start: now.Add(tc.start), End: now.Add(tc.end), PowerW: 11000}, &original)
			require.NoError(t, err)
			loaded, err := s.LoadOverrides(now)
			require.NoError(t, err)
			require.Len(t, loaded, tc.blocks+1)
			var blocked time.Duration
			for _, o := range loaded {
				if o.Kind == OverrideKindBlock {
					blocked += o.End.Sub(o.Start)
				}
			}
			overlap := minTime(original.End, now.Add(tc.end)).Sub(maxTime(original.Start, now.Add(tc.start)))
			if overlap < 0 {
				overlap = 0
			}
			assert.Equal(t, original.End.Sub(original.Start)-overlap, blocked)
		})
	}
}

func TestSaveOverrideRollsBackOriginalBlocksOnConflict(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	_, err := s.InsertOverride(ScheduleOverride{Kind: OverrideKindBlock, Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour)})
	require.NoError(t, err)
	_, err = s.SaveOverride(ScheduleOverride{Kind: OverrideKindForce, Start: now.Add(4 * time.Hour), End: now.Add(5 * time.Hour), PowerW: 11000}, &ScheduleOverride{Start: now.Add(time.Hour), End: now.Add(3 * time.Hour)})
	require.ErrorIs(t, err, ErrOverlap)
	loaded, err := s.LoadOverrides(now)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
}

func TestApplyOverrideReplacesCoveredRange(t *testing.T) {
	for _, kind := range []string{OverrideKindBlock, OverrideKindForce} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			_, err := s.InsertOverride(ScheduleOverride{Kind: kind, Start: now, End: now.Add(3 * time.Hour), PowerW: 11000})
			require.NoError(t, err)
			id, err := s.ApplyOverride(ScheduleOverride{Kind: OverrideKindForce, Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), PowerW: 22000}, nil)
			require.NoError(t, err)
			windows, err := s.LoadOverrides(now.Add(-time.Hour))
			require.NoError(t, err)
			require.Len(t, windows, 3)
			assert.Equal(t, now, windows[0].Start)
			assert.Equal(t, now.Add(time.Hour), windows[0].End)
			assert.Equal(t, id, windows[1].ID)
			assert.Equal(t, OverrideKindForce, windows[1].Kind)
			assert.Equal(t, 22000.0, windows[1].PowerW)
			assert.Equal(t, now.Add(2*time.Hour), windows[2].Start)
			assert.Equal(t, now.Add(3*time.Hour), windows[2].End)
			_, err = s.ApplyOverride(ScheduleOverride{ID: 999, Kind: OverrideKindForce, Start: now, End: now.Add(3 * time.Hour), PowerW: 22000}, nil)
			require.ErrorIs(t, err, sql.ErrNoRows)
			unchanged, err := s.LoadOverrides(now.Add(-time.Hour))
			require.NoError(t, err)
			assert.Equal(t, windows, unchanged)
		})
	}
}

func TestResetScheduleClearsAllCancellationState(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	require.NoError(t, s.Set("scheduler.persisted_schedule", `{"slots":[{"cancelled":true}]}`))
	require.NoError(t, s.Set("charger.max_power", "22000"))
	_, err := s.InsertOverride(ScheduleOverride{Kind: OverrideKindBlock, Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)})
	require.NoError(t, err)
	_, err = s.InsertOverride(ScheduleOverride{Kind: OverrideKindForce, Start: now, End: now.Add(time.Hour), PowerW: 11000})
	require.NoError(t, err)
	require.NoError(t, s.ResetSchedule())
	var count int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(*) FROM schedule_overrides`).Scan(&count))
	assert.Zero(t, count)
	assert.True(t, s.GetBool("scheduler.paused", false))
	assert.Empty(t, s.GetDefault("scheduler.persisted_schedule", "missing"))
	assert.Equal(t, "22000", s.GetDefault("charger.max_power", ""))
}
