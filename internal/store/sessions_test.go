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

func TestConsumptionWindow(t *testing.T) {
	s := testStore(t)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// Odometer readings at days 0, 10, 20, 30, 40 (45-day span).
	odo := []struct {
		day int
		km  float64
	}{
		{0, 10000},
		{10, 10500},
		{20, 11200},
		{30, 11900},
		{40, 12600},
	}
	for _, r := range odo {
		require.NoError(t, s.InsertOdometerReading(OdometerReading{
			Timestamp: now.Add(time.Duration(r.day) * 24 * time.Hour),
			Mileage:   r.km,
		}))
	}

	// Sessions at days 5, 15, 25, 35 (10 kWh each).
	for i, day := range []int{5, 15, 25, 35} {
		txn := i + 1
		start := now.Add(time.Duration(day) * 24 * time.Hour)
		require.NoError(t, s.StartSession(Session{
			ChargeBox: "CP001", ConnectorID: 1, TransactionID: txn,
			StartTime: start, MeterStart: float64(i * 1000),
		}))
		require.NoError(t, s.StopSession(txn, start.Add(2*time.Hour), float64(i*1000+10), 10, 5))
	}

	// Anchor at day 40, window is (day 10, day 40].
	// Energy: sessions at days 15, 25, 35 = 30 kWh (day 5 excluded).
	// Distance: odometer at days 20, 30, 40 → MAX-MIN = 12600 - 11200 = 1400 km
	// (day 10 excluded by `>` lower bound).
	anchor := now.Add(40 * 24 * time.Hour)
	energy, distance, ok, err := s.ConsumptionWindow(anchor)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.InDelta(t, 30.0, energy, 0.01)
	assert.InDelta(t, 1400.0, distance, 0.01)
}

func TestStopSessionPersistsConsumption(t *testing.T) {
	s := testStore(t)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// Odometer readings spanning the trailing window of the second session.
	// Need ≥2 readings between the first session's stop and the second's start
	// for OdometerDelta to give a non-zero per-session distance.
	odo := []struct {
		day float64
		km  float64
	}{
		{0, 10000},
		{8, 10100},
		{14, 10500},
		{20, 11000},
	}
	for _, r := range odo {
		require.NoError(t, s.InsertOdometerReading(OdometerReading{
			Timestamp: now.Add(time.Duration(r.day * 24 * float64(time.Hour))),
			Mileage:   r.km,
		}))
	}

	// First session at day 5.
	start1 := now.Add(5 * 24 * time.Hour)
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP001", ConnectorID: 1, TransactionID: 1,
		StartTime: start1, MeterStart: 0,
	}))
	require.NoError(t, s.StopSession(1, start1.Add(2*time.Hour), 10, 10, 5))

	// Second session at day 15.
	start2 := now.Add(15 * 24 * time.Hour)
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP001", ConnectorID: 1, TransactionID: 2,
		StartTime: start2, MeterStart: 10,
	}))
	require.NoError(t, s.StopSession(2, start2.Add(2*time.Hour), 30, 20, 10))

	history, err := s.SessionHistory(10, 0)
	require.NoError(t, err)
	require.Len(t, history, 2)

	// History is newest-first; second session is index 0.
	second := history[0]
	assert.Greater(t, second.Distance, 0.0, "second session should have distance since previous")
	assert.Greater(t, second.KWhPer100km, 0.0, "second session should have trailing-30d kWh/100km")
}

func TestSessionReportKWhPer100kmIsTrailingWindow(t *testing.T) {
	s := testStore(t)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// Odometer over the trailing window: insert at day 1 and day 30.
	// Window (anchor-30d, anchor] = (day 0, day 30] → both included →
	// MAX-MIN = 1000.
	require.NoError(t, s.InsertOdometerReading(OdometerReading{
		Timestamp: now.Add(1 * 24 * time.Hour), Mileage: 10000,
	}))
	require.NoError(t, s.InsertOdometerReading(OdometerReading{
		Timestamp: now.Add(30 * 24 * time.Hour), Mileage: 11000,
	}))

	// One session in window: 16 kWh.
	start := now.Add(15 * 24 * time.Hour)
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP001", ConnectorID: 1, TransactionID: 1,
		StartTime: start, MeterStart: 0,
	}))
	require.NoError(t, s.StopSession(1, start.Add(2*time.Hour), 16, 16, 8))

	// Report range covers only the last day, but KWhPer100km should reflect
	// the trailing 30 days ending at `to` = 30d after now.
	reportTo := now.Add(30 * 24 * time.Hour)
	reportFrom := reportTo.Add(-1 * 24 * time.Hour)
	report, err := s.SessionReportByRange(reportFrom, reportTo)
	require.NoError(t, err)

	// Expected: 16 kWh / 1000 km * 100 = 1.6 kWh/100km.
	assert.InDelta(t, 1.6, report.KWhPer100km, 0.05,
		"KWhPer100km should be trailing-30d window value, independent of report range")
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
