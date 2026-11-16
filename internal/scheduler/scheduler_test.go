package scheduler

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- persistence ---

// saveCurrent encodes s.current to settings; loadPersistedSchedule restores
// it. After a "restart" (new Scheduler against the same store) the schedule
// must come back intact.
func TestPersistedScheduleSurvivesRestart(t *testing.T) {
	mock := &mockCharger{}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	original := &Schedule{
		Slots: []ScheduleSlot{{
			Date:     now.Format("2006-01-02"),
			Deadline: now.Add(8 * time.Hour),
			Periods: []SchedulePeriod{
				{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000, Price: 0.30},
				{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Power: 11000, Price: 0.40},
			},
			Cost:   7.70,
			Energy: 22,
		}},
		Cost:     7.70,
		Energy:   22,
		Deadline: now.Add(8 * time.Hour),
	}
	s.mu.Lock()
	s.current = original
	s.mu.Unlock()
	s.saveCurrent()

	// Simulate restart: build a fresh Scheduler against the same store.
	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()

	require.NotNil(t, restored.current, "schedule must be restored")
	require.Len(t, restored.current.Slots, 1)
	require.Len(t, restored.current.Slots[0].Periods, 2)
	assert.Equal(t, original.Slots[0].Periods[0].Start.Unix(), restored.current.Slots[0].Periods[0].Start.Unix())
	assert.Equal(t, original.Slots[0].Periods[1].End.Unix(), restored.current.Slots[0].Periods[1].End.Unix())
	assert.Equal(t, 11000.0, restored.current.Slots[0].Periods[0].Power)
}

// A persisted slot whose every period is in the past should not be restored.
func TestPersistedSchedulePrunesPastSlots(t *testing.T) {
	mock := &mockCharger{}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	original := &Schedule{
		Slots: []ScheduleSlot{
			// Past slot: entirely in the past.
			{
				Date: now.Add(-24 * time.Hour).Format("2006-01-02"),
				Periods: []SchedulePeriod{
					{Start: now.Add(-25 * time.Hour), End: now.Add(-24 * time.Hour), Power: 11000},
				},
			},
			// Future slot: keep.
			{
				Date: now.Format("2006-01-02"),
				Periods: []SchedulePeriod{
					{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
				},
			},
		},
	}
	s.mu.Lock()
	s.current = original
	s.mu.Unlock()
	s.saveCurrent()

	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()

	require.NotNil(t, restored.current)
	require.Len(t, restored.current.Slots, 1, "past slot pruned, future slot kept")
	assert.Equal(t, now.Format("2006-01-02"), restored.current.Slots[0].Date)
}

// A persisted schedule with NO live periods should result in s.current == nil.
func TestPersistedScheduleAllPastResultsNil(t *testing.T) {
	mock := &mockCharger{}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	original := &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Add(-24 * time.Hour).Format("2006-01-02"),
			Periods: []SchedulePeriod{
				{Start: now.Add(-25 * time.Hour), End: now.Add(-24 * time.Hour), Power: 11000},
			},
		}},
	}
	s.mu.Lock()
	s.current = original
	s.mu.Unlock()
	s.saveCurrent()

	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()

	assert.Nil(t, restored.current, "all-past schedule must not be restored")
}

// ClearSchedule must persist the empty state so a stale schedule isn't
// restored on next startup.
func TestClearSchedulePersistsEmpty(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Available", txnID: 0}
	s, st := newTestScheduler(t, mock)

	now := time.Now().Truncate(time.Hour)
	withNow(t, now)
	s.mu.Lock()
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{
				{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000},
			},
		}},
	}
	s.mu.Unlock()
	s.saveCurrent()

	// Verify it persisted.
	require.NotEmpty(t, st.GetDefault(persistedScheduleKey, ""))

	s.ClearSchedule()

	// Now should be empty.
	assert.Empty(t, st.GetDefault(persistedScheduleKey, ""), "ClearSchedule must clear persisted blob")

	// And a "restart" should not restore anything.
	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      s.log,
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()
	assert.Nil(t, restored.current)
}

// Garbage in the persisted blob must not crash; loadPersistedSchedule
// should log a warning and continue with no schedule.
func TestPersistedScheduleHandlesCorruptBlob(t *testing.T) {
	mock := &mockCharger{}
	_, st := newTestScheduler(t, mock)
	require.NoError(t, st.Set(persistedScheduleKey, "{not valid json"))

	restored := &Scheduler{
		charger:  &mockCharger{},
		store:    st,
		log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		notifyCh: make(chan struct{}, 1),
	}
	restored.loadPersistedSchedule()
	assert.Nil(t, restored.current)
}
