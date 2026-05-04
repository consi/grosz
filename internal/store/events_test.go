package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordAndRecentEvents(t *testing.T) {
	s := testStore(t)

	now := time.Now()
	events := []Event{
		{Timestamp: now.Add(-2 * time.Second), Direction: "recv", ChargeBox: "CP001", Action: "BootNotification", Payload: map[string]string{"vendor": "Myenergi"}},
		{Timestamp: now.Add(-1 * time.Second), Direction: "send", ChargeBox: "CP001", Action: "BootNotification", Payload: map[string]string{"status": "Accepted"}},
		{Timestamp: now, Direction: "recv", ChargeBox: "CP001", Action: "Heartbeat", Payload: map[string]string{}},
	}

	for _, e := range events {
		require.NoError(t, s.RecordEvent(e))
	}

	recent, err := s.RecentEvents(10, 0)
	require.NoError(t, err)
	assert.Len(t, recent, 3)

	// Newest first
	assert.Equal(t, "Heartbeat", recent[0].Action)
	assert.Equal(t, "BootNotification", recent[1].Action)
	assert.Equal(t, "send", recent[1].Direction)
}

func TestRecentEventsLimit(t *testing.T) {
	s := testStore(t)

	for i := 0; i < 5; i++ {
		require.NoError(t, s.RecordEvent(Event{
			Timestamp: time.Now(),
			Direction: "recv",
			ChargeBox: "CP001",
			Action:    "Heartbeat",
			Payload:   nil,
		}))
	}

	recent, err := s.RecentEvents(3, 0)
	require.NoError(t, err)
	assert.Len(t, recent, 3)
}

func TestEventsByAction(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.RecordEvent(Event{Timestamp: time.Now(), Direction: "recv", ChargeBox: "CP001", Action: "BootNotification", Payload: nil}))
	require.NoError(t, s.RecordEvent(Event{Timestamp: time.Now(), Direction: "recv", ChargeBox: "CP001", Action: "Heartbeat", Payload: nil}))
	require.NoError(t, s.RecordEvent(Event{Timestamp: time.Now(), Direction: "recv", ChargeBox: "CP001", Action: "Heartbeat", Payload: nil}))

	heartbeats, err := s.EventsByAction("Heartbeat", 10, 0)
	require.NoError(t, err)
	assert.Len(t, heartbeats, 2)

	boots, err := s.EventsByAction("BootNotification", 10, 0)
	require.NoError(t, err)
	assert.Len(t, boots, 1)
}

func TestEventsSince(t *testing.T) {
	s := testStore(t)

	for i := 0; i < 5; i++ {
		require.NoError(t, s.RecordEvent(Event{
			Timestamp: time.Now(),
			Direction: "recv",
			ChargeBox: "CP001",
			Action:    "Heartbeat",
			Payload:   nil,
		}))
	}

	// Get events after ID 2 (should return IDs 3, 4, 5)
	events, err := s.EventsSince(2, 10)
	require.NoError(t, err)
	assert.Len(t, events, 3)
	assert.Equal(t, int64(3), events[0].ID) // oldest first
}

// meterValuesPayload builds an OCPP MeterValues JSON payload with a single
// Energy.Active.Import.Register sample at the given Wh.
func meterValuesPayload(t *testing.T, txnID int, ts time.Time, samples []map[string]string) string {
	t.Helper()
	payload := map[string]any{
		"connectorId":   1,
		"transactionId": txnID,
		"meterValue": []map[string]any{
			{
				"timestamp":    ts.UTC().Format(time.RFC3339),
				"sampledValue": samples,
			},
		},
	}
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	return string(b)
}

func recordMeterValues(t *testing.T, s *Store, txnID int, at time.Time, samples []map[string]string) {
	t.Helper()
	require.NoError(t, s.RecordEvent(Event{
		Timestamp: at,
		Direction: "recv",
		ChargeBox: "CP001",
		Action:    "MeterValues",
		Payload:   json.RawMessage(meterValuesPayload(t, txnID, at, samples)),
	}))
}

func TestLatestEnergyForTransaction_ReturnsLatest(t *testing.T) {
	s := testStore(t)

	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(5 * time.Minute)
	recordMeterValues(t, s, 11, t1, []map[string]string{
		{"value": "1000", "measurand": "Energy.Active.Import.Register", "unit": "Wh"},
	})
	recordMeterValues(t, s, 11, t2, []map[string]string{
		{"value": "5500", "measurand": "Energy.Active.Import.Register", "unit": "Wh"},
	})

	kwh, _, ok := s.LatestEnergyForTransaction(11)
	require.True(t, ok)
	assert.InDelta(t, 5.5, kwh, 0.001, "latest reading should win")
}

// Zappi reports the energy register only on phase=N. The fallback must catch
// it when the unphased base key is absent.
func TestLatestEnergyForTransaction_HandlesPhaseN(t *testing.T) {
	s := testStore(t)

	at := time.Date(2026, 5, 1, 12, 58, 4, 0, time.UTC)
	recordMeterValues(t, s, 11, at, []map[string]string{
		{"value": "32351", "measurand": "Energy.Active.Import.Register", "phase": "N", "unit": "Wh"},
	})

	kwh, _, ok := s.LatestEnergyForTransaction(11)
	require.True(t, ok)
	assert.InDelta(t, 32.351, kwh, 0.001)
}

func TestLatestEnergyForTransaction_NoData(t *testing.T) {
	s := testStore(t)

	_, _, ok := s.LatestEnergyForTransaction(99)
	assert.False(t, ok)
}

// Two different transactions interleaved — make sure we filter to the right one.
func TestLatestEnergyForTransaction_FiltersByTransaction(t *testing.T) {
	s := testStore(t)

	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	recordMeterValues(t, s, 10, t1, []map[string]string{
		{"value": "999", "measurand": "Energy.Active.Import.Register", "phase": "N", "unit": "Wh"},
	})
	recordMeterValues(t, s, 11, t1.Add(time.Minute), []map[string]string{
		{"value": "32351", "measurand": "Energy.Active.Import.Register", "phase": "N", "unit": "Wh"},
	})

	kwh, _, ok := s.LatestEnergyForTransaction(11)
	require.True(t, ok)
	assert.InDelta(t, 32.351, kwh, 0.001)

	kwh10, _, ok := s.LatestEnergyForTransaction(10)
	require.True(t, ok)
	assert.InDelta(t, 0.999, kwh10, 0.001)
}
