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
		Kind: OverrideKindBlock,
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
		Kind: OverrideKindBlock,
		Start: now.Add(-2 * time.Hour), End: now.Add(-1 * time.Hour),
	})
	_, _ = s.InsertOverride(ScheduleOverride{
		Kind: OverrideKindBlock,
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
		Kind: OverrideKindBlock,
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
