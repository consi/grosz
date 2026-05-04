package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectorState_GetMissingReturnsNil(t *testing.T) {
	s := testStore(t)

	cs, err := s.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	assert.Nil(t, cs)
}

func TestConnectorState_UpsertAndGet(t *testing.T) {
	s := testStore(t)

	statusAt := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	lastNotif := time.Date(2026, 4, 1, 10, 0, 5, 0, time.UTC)

	require.NoError(t, s.UpsertConnectorState(ConnectorState{
		ChargeBox:              "CP001",
		ConnectorID:            1,
		Status:                 "Preparing",
		StatusAt:               statusAt,
		ErrorCode:              "NoError",
		TransactionID:          42,
		IdTag:                  "grosz",
		LastStatusNotification: lastNotif,
	}))

	cs, err := s.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs)
	assert.Equal(t, "CP001", cs.ChargeBox)
	assert.Equal(t, 1, cs.ConnectorID)
	assert.Equal(t, "Preparing", cs.Status)
	assert.True(t, cs.StatusAt.Equal(statusAt))
	assert.Equal(t, "NoError", cs.ErrorCode)
	assert.Equal(t, 42, cs.TransactionID)
	assert.Equal(t, "grosz", cs.IdTag)
	assert.True(t, cs.LastStatusNotification.Equal(lastNotif))
}

func TestConnectorState_UpsertOverwrites(t *testing.T) {
	s := testStore(t)

	t1 := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, s.UpsertConnectorState(ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Available", StatusAt: t1, LastStatusNotification: t1,
	}))

	t2 := time.Date(2026, 4, 1, 11, 0, 0, 0, time.UTC)
	require.NoError(t, s.UpsertConnectorState(ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Charging", StatusAt: t2,
		TransactionID: 99, IdTag: "tag", LastStatusNotification: t2,
	}))

	cs, err := s.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs)
	assert.Equal(t, "Charging", cs.Status)
	assert.Equal(t, 99, cs.TransactionID)
	assert.Equal(t, "tag", cs.IdTag)
	assert.True(t, cs.StatusAt.Equal(t2))
}

func TestConnectorState_AllReturnsMultiple(t *testing.T) {
	s := testStore(t)

	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, s.UpsertConnectorState(ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Available", StatusAt: now, LastStatusNotification: now,
	}))
	require.NoError(t, s.UpsertConnectorState(ConnectorState{
		ChargeBox: "CP002", ConnectorID: 1, Status: "Charging", StatusAt: now, LastStatusNotification: now,
	}))
	require.NoError(t, s.UpsertConnectorState(ConnectorState{
		ChargeBox: "CP001", ConnectorID: 2, Status: "Faulted", StatusAt: now, LastStatusNotification: now,
	}))

	all, err := s.AllConnectorStates()
	require.NoError(t, err)
	assert.Len(t, all, 3)
}

func TestConnectorState_TouchStatusNotificationCreatesRow(t *testing.T) {
	s := testStore(t)

	at := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, s.TouchStatusNotification("CP001", 1, at))

	cs, err := s.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs)
	assert.True(t, cs.LastStatusNotification.Equal(at))
	assert.Empty(t, cs.Status, "status remains empty until first real upsert")
}

func TestConnectorState_TouchStatusNotificationPreservesStatus(t *testing.T) {
	s := testStore(t)

	t1 := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	require.NoError(t, s.UpsertConnectorState(ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Charging", StatusAt: t1,
		TransactionID: 42, IdTag: "grosz", LastStatusNotification: t1,
	}))

	t2 := t1.Add(10 * time.Minute)
	require.NoError(t, s.TouchStatusNotification("CP001", 1, t2))

	cs, err := s.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs)
	assert.Equal(t, "Charging", cs.Status, "Touch must not clobber status")
	assert.Equal(t, 42, cs.TransactionID, "Touch must not clobber transaction id")
	assert.True(t, cs.LastStatusNotification.Equal(t2), "Touch must update timestamp")
	assert.True(t, cs.StatusAt.Equal(t1), "Touch must not change status_at")
}
