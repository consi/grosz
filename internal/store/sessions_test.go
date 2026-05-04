package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionLifecycle(t *testing.T) {
	s := testStore(t)

	startTime := time.Now().Truncate(time.Second)
	sess := Session{
		ChargeBox:     "CP001",
		ConnectorID:   1,
		TransactionID: 42,
		IdTag:         "grosz",
		StartTime:     startTime,
		MeterStart:    1000.5,
	}
	require.NoError(t, s.StartSession(sess))

	// Active session should be findable
	active, err := s.ActiveSession()
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, 42, active.TransactionID)
	assert.Equal(t, "active", active.Status)
	assert.Equal(t, "grosz", active.IdTag)
	assert.InDelta(t, 1000.5, active.MeterStart, 0.01)

	// Stop the session
	stopTime := startTime.Add(2 * time.Hour)
	require.NoError(t, s.StopSession(42, stopTime, 1015.3, 14.8, 7.50))

	// No more active sessions
	active, err = s.ActiveSession()
	require.NoError(t, err)
	assert.Nil(t, active)

	// Check history
	history, err := s.SessionHistory(10, 0)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "completed", history[0].Status)
	assert.InDelta(t, 14.8, history[0].Energy, 0.01)
	assert.InDelta(t, 7.50, history[0].Cost, 0.01)
}

func TestStopNonexistentSession(t *testing.T) {
	s := testStore(t)

	err := s.StopSession(999, time.Now(), 0, 0, 0)
	assert.Error(t, err)
}

func TestSessionHistoryOrder(t *testing.T) {
	s := testStore(t)

	for i := 1; i <= 3; i++ {
		require.NoError(t, s.StartSession(Session{
			ChargeBox:     "CP001",
			ConnectorID:   1,
			TransactionID: i,
			StartTime:     time.Now(),
			MeterStart:    float64(i * 100),
		}))
		require.NoError(t, s.StopSession(i, time.Now(), float64(i*100+10), 10, 5))
	}

	history, err := s.SessionHistory(10, 0)
	require.NoError(t, err)
	assert.Len(t, history, 3)
	// Newest first
	assert.Equal(t, 3, history[0].TransactionID)
	assert.Equal(t, 1, history[2].TransactionID)
}

func TestUniqueTransactionID(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP001", ConnectorID: 1, TransactionID: 1, StartTime: time.Now(),
	}))

	err := s.StartSession(Session{
		ChargeBox: "CP001", ConnectorID: 1, TransactionID: 1, StartTime: time.Now(),
	})
	assert.Error(t, err)
}
