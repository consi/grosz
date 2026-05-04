package ocpp

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dbPath, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	s := NewServer(st, log)
	// Pre-create the charge point so handlers find it
	s.mu.Lock()
	s.points["CP001"] = NewChargePoint("CP001")
	s.mu.Unlock()
	return s
}

func TestOnBootNotification(t *testing.T) {
	s := testServer(t)
	fixedNow := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return fixedNow }
	defer func() { timeNow = time.Now }()

	req := core.NewBootNotificationRequest("Zappi", "Myenergi")
	req.ChargeBoxSerialNumber = "S12345"
	req.FirmwareVersion = "5540"

	conf, err := s.OnBootNotification("CP001", req)
	require.NoError(t, err)
	require.NotNil(t, conf)
	assert.Equal(t, core.RegistrationStatusAccepted, conf.Status)
	assert.Equal(t, 120, conf.Interval)
	assert.Equal(t, fixedNow, conf.CurrentTime.Time)

	// Check boot info stored
	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp)
	assert.True(t, cp.IsConnected())
	cp.mu.RLock()
	assert.Equal(t, "Myenergi", cp.BootInfo.Vendor)
	assert.Equal(t, "Zappi", cp.BootInfo.Model)
	assert.Equal(t, "S12345", cp.BootInfo.SerialNumber)
	cp.mu.RUnlock()

	// Check event persisted
	time.Sleep(10 * time.Millisecond) // allow async event recording
	events, err := s.store.RecentEvents(10, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(events), 1)
}

func TestOnHeartbeat(t *testing.T) {
	s := testServer(t)
	fixedNow := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	timeNow = func() time.Time { return fixedNow }
	defer func() { timeNow = time.Now }()

	conf, err := s.OnHeartbeat("CP001", core.NewHeartbeatRequest())
	require.NoError(t, err)
	assert.Equal(t, fixedNow, conf.CurrentTime.Time)
}

func TestOnStatusNotification(t *testing.T) {
	s := testServer(t)

	req := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusAvailable)
	conf, err := s.OnStatusNotification("CP001", req)
	require.NoError(t, err)
	require.NotNil(t, conf)

	cp := s.ChargePoint("CP001")
	conn := cp.GetConnector(1)
	snap := conn.Snapshot()
	assert.Equal(t, "Available", snap.Status)

	// Update to Charging
	req2 := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusCharging)
	_, err = s.OnStatusNotification("CP001", req2)
	require.NoError(t, err)

	snap = conn.Snapshot()
	assert.Equal(t, "Charging", snap.Status)
}

func TestOnMeterValues(t *testing.T) {
	s := testServer(t)

	now := types.NewDateTime(time.Now())
	req := &core.MeterValuesRequest{
		ConnectorId: 1,
		MeterValue: []types.MeterValue{
			{
				Timestamp: now,
				SampledValue: []types.SampledValue{
					{Value: "7360", Measurand: types.MeasurandPowerActiveImport, Unit: types.UnitOfMeasureW},
					{Value: "230.5", Measurand: types.MeasurandVoltage, Unit: types.UnitOfMeasureV},
					{Value: "1500.5", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
				},
			},
		},
	}

	conf, err := s.OnMeterValues("CP001", req)
	require.NoError(t, err)
	require.NotNil(t, conf)

	cp := s.ChargePoint("CP001")
	conn := cp.GetConnector(1)
	snap := conn.Snapshot()

	assert.InDelta(t, 7360, snap.Measurements["Power.Active.Import"].Value, 0.1)
	assert.InDelta(t, 230.5, snap.Measurements["Voltage"].Value, 0.1)
	assert.InDelta(t, 1500.5, snap.Measurements["Energy.Active.Import.Register"].Value, 0.1)
}

func TestOnAuthorize(t *testing.T) {
	s := testServer(t)

	conf, err := s.OnAuthorize("CP001", core.NewAuthorizationRequest("virtual-tag-123"))
	require.NoError(t, err)
	assert.Equal(t, types.AuthorizationStatusAccepted, conf.IdTagInfo.Status)
}

// SuspendedEV must finalize the active session in the DB so it appears in
// reports — Zappi keeps the OCPP transaction open until unplug.
func TestOnStatusNotification_SuspendedEVFinalizesSession(t *testing.T) {
	s := testServer(t)

	startReq := core.NewStartTransactionRequest(1, "garaz", 0, types.NewDateTime(time.Now().Add(-1*time.Hour)))
	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	txnID := startConf.TransactionId

	mvReq := &core.MeterValuesRequest{
		ConnectorId: 1,
		TransactionId: &txnID,
		MeterValue: []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "7952", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
			},
		}},
	}
	_, err = s.OnMeterValues("CP001", mvReq)
	require.NoError(t, err)

	// Use a recent timestamp (truncated to second precision so RFC3339
	// round-trip is exact) so the inserted stop marker stays within the
	// RecentChartMarkers 48h window.
	statusTS := time.Now().Add(-30 * time.Minute).Truncate(time.Second).UTC()
	statusReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusSuspendedEV)
	statusReq.Timestamp = types.NewDateTime(statusTS)
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)

	// Active session should now be nil — it was completed.
	active, err := s.store.ActiveSession()
	require.NoError(t, err)
	assert.Nil(t, active, "session should be finalized after SuspendedEV")

	hist, err := s.store.SessionHistory(1, 0)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, "completed", hist[0].Status)
	assert.InDelta(t, 7.952, hist[0].Energy, 0.001)
	assert.False(t, hist[0].StopTime.IsZero())

	// And a "stop" chart marker should have been emitted at the StatusNotification timestamp.
	markers, err := s.store.RecentChartMarkers(48)
	require.NoError(t, err)
	stopCount := 0
	for _, m := range markers {
		if m.Type == "stop" {
			stopCount++
			assert.True(t, m.Timestamp.Equal(statusTS), "stop marker must use the status timestamp")
		}
	}
	assert.Equal(t, 1, stopCount, "expected exactly one stop marker")
}

// SuspendedEVSE means the charger paused (e.g. grid request). Same finalize
// path, same marker emission.
func TestOnStatusNotification_SuspendedEVSE_EmitsStopMarkerAndFinalizes(t *testing.T) {
	s := testServer(t)
	assertStopStatusFinalizes(t, s, core.ChargePointStatusSuspendedEVSE)
}

// Finishing happens during normal session wind-down.
func TestOnStatusNotification_Finishing_EmitsStopMarkerAndFinalizes(t *testing.T) {
	s := testServer(t)
	assertStopStatusFinalizes(t, s, core.ChargePointStatusFinishing)
}

// Faulted during an active charge must still finalize (and be visible on chart)
// rather than leaving the session stranded.
func TestOnStatusNotification_Faulted_EmitsStopMarkerAndFinalizes(t *testing.T) {
	s := testServer(t)
	assertStopStatusFinalizes(t, s, core.ChargePointStatusFaulted)
}

// assertStopStatusFinalizes drives one start + meter + status sequence and
// checks both the DB session is closed and a "stop" chart marker is recorded
// at the status timestamp.
func assertStopStatusFinalizes(t *testing.T, s *Server, status core.ChargePointStatus) {
	t.Helper()
	startReq := core.NewStartTransactionRequest(1, "garaz", 0,
		types.NewDateTime(time.Now().Add(-1*time.Hour)))
	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	txnID := startConf.TransactionId

	mvReq := &core.MeterValuesRequest{
		ConnectorId:   1,
		TransactionId: &txnID,
		MeterValue: []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "5000", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
			},
		}},
	}
	_, err = s.OnMeterValues("CP001", mvReq)
	require.NoError(t, err)

	at := time.Now().Add(-30 * time.Minute).Truncate(time.Second).UTC()
	statusReq := core.NewStatusNotificationRequest(1, core.NoError, status)
	statusReq.Timestamp = types.NewDateTime(at)
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)

	active, err := s.store.ActiveSession()
	require.NoError(t, err)
	assert.Nil(t, active, "%s should finalize the active session", status)

	markers, err := s.store.RecentChartMarkers(48)
	require.NoError(t, err)
	var stops []store.ChartMarker
	for _, m := range markers {
		if m.Type == "stop" {
			stops = append(stops, m)
		}
	}
	require.Len(t, stops, 1, "%s must emit exactly one stop marker", status)
	assert.True(t, stops[0].Timestamp.Equal(at))
}

// After a websocket reconnect the in-memory ChargePoint is replaced. Even
// without measurements pre-loaded, the finalize path must still close the
// session — by reading the txn from connector_state and the energy from the
// fallback in ocpp_events. This is the bug that left session #11 stranded.
func TestFinalize_AfterWSReconnect_StillFinalizes(t *testing.T) {
	s := testServer(t)

	startReq := core.NewStartTransactionRequest(1, "garaz", 0,
		types.NewDateTime(time.Now().Add(-1*time.Hour)))
	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	txnID := startConf.TransactionId

	// MeterValues recorded into ocpp_events (also populates in-memory map).
	mvReq := &core.MeterValuesRequest{
		ConnectorId:   1,
		TransactionId: &txnID,
		MeterValue: []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "32351", Measurand: types.MeasurandEnergyActiveImportRegister, Phase: "N", Unit: types.UnitOfMeasureWh},
			},
		}},
	}
	_, err = s.OnMeterValues("CP001", mvReq)
	require.NoError(t, err)
	// Allow the recordEvent goroutine to flush before we reset state.
	time.Sleep(20 * time.Millisecond)

	// Simulate a websocket reconnect: replace the in-memory ChargePoint with
	// a fresh one. (After the fix this is what happens for first-time CPs;
	// for reconnects we now preserve, so this test is a worst-case.)
	s.mu.Lock()
	s.points["CP001"] = NewChargePoint("CP001")
	s.mu.Unlock()

	at := time.Now().Add(-30 * time.Minute).Truncate(time.Second).UTC()
	statusReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusSuspendedEV)
	statusReq.Timestamp = types.NewDateTime(at)
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)

	active, err := s.store.ActiveSession()
	require.NoError(t, err)
	assert.Nil(t, active, "finalize must work even after in-memory reset")

	hist, err := s.store.SessionHistory(1, 0)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, "completed", hist[0].Status)
	assert.InDelta(t, 32.351, hist[0].Energy, 0.001)

	markers, err := s.store.RecentChartMarkers(48)
	require.NoError(t, err)
	stopCount := 0
	for _, m := range markers {
		if m.Type == "stop" {
			stopCount++
			assert.True(t, m.Timestamp.Equal(at))
		}
	}
	assert.Equal(t, 1, stopCount)
}

// Available -> SuspendedEV with no charging in between (no active txn) must
// not emit a "stop" marker — there's nothing to stop.
func TestOnStatusNotification_StopFromAvailable_NoStopMarker(t *testing.T) {
	s := testServer(t)

	// Establish prior Available status with no active session.
	avReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusAvailable)
	_, err := s.OnStatusNotification("CP001", avReq)
	require.NoError(t, err)

	susReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusSuspendedEV)
	_, err = s.OnStatusNotification("CP001", susReq)
	require.NoError(t, err)

	markers, err := s.store.RecentChartMarkers(48)
	require.NoError(t, err)
	for _, m := range markers {
		assert.NotEqual(t, "stop", m.Type, "no stop marker without an active session")
	}
}

// Repeated SuspendedEV notifications must emit only one stop marker, since
// only the first one actually finalizes the session.
func TestOnStatusNotification_RepeatedSuspendedEV_OneStopMarker(t *testing.T) {
	s := testServer(t)

	startReq := core.NewStartTransactionRequest(1, "garaz", 0,
		types.NewDateTime(time.Now().Add(-1*time.Hour)))
	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	txnID := startConf.TransactionId

	mvReq := &core.MeterValuesRequest{
		ConnectorId:   1,
		TransactionId: &txnID,
		MeterValue: []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "5000", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
			},
		}},
	}
	_, err = s.OnMeterValues("CP001", mvReq)
	require.NoError(t, err)

	statusReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusSuspendedEV)
	statusReq.Timestamp = types.NewDateTime(time.Now())
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)
	// Same status arriving again — Zappi re-emits SuspendedEV every 25min.
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)

	markers, err := s.store.RecentChartMarkers(48)
	require.NoError(t, err)
	stopCount := 0
	for _, m := range markers {
		if m.Type == "stop" {
			stopCount++
		}
	}
	assert.Equal(t, 1, stopCount, "duplicate suspends must not double-mark")
}

// Zappi reports Energy.Active.Import.Register with phase=N. The connector
// stores it under the phase-suffixed key, so finalize must fall back to it.
func TestOnStatusNotification_SuspendedEVHandlesPhaseN(t *testing.T) {
	s := testServer(t)

	startReq := core.NewStartTransactionRequest(1, "garaz", 0, types.NewDateTime(time.Now().Add(-30*time.Minute)))
	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	txnID := startConf.TransactionId

	mvReq := &core.MeterValuesRequest{
		ConnectorId:   1,
		TransactionId: &txnID,
		MeterValue: []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "5000", Measurand: types.MeasurandEnergyActiveImportRegister, Phase: "N", Unit: types.UnitOfMeasureWh},
			},
		}},
	}
	_, err = s.OnMeterValues("CP001", mvReq)
	require.NoError(t, err)

	statusReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusSuspendedEV)
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)

	hist, err := s.store.SessionHistory(1, 0)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, "completed", hist[0].Status)
	assert.InDelta(t, 5.0, hist[0].Energy, 0.001)
}

// Subsequent SuspendedEV notifications are no-ops because StopSession is
// gated on status='active'.
func TestOnStatusNotification_SuspendedEVIdempotent(t *testing.T) {
	s := testServer(t)

	startReq := core.NewStartTransactionRequest(1, "garaz", 0, types.NewDateTime(time.Now().Add(-1*time.Hour)))
	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	txnID := startConf.TransactionId

	mvReq := &core.MeterValuesRequest{
		ConnectorId:   1,
		TransactionId: &txnID,
		MeterValue: []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "3000", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
			},
		}},
	}
	_, err = s.OnMeterValues("CP001", mvReq)
	require.NoError(t, err)

	statusReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusSuspendedEV)
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)
	_, err = s.OnStatusNotification("CP001", statusReq)
	require.NoError(t, err)

	hist, err := s.store.SessionHistory(2, 0)
	require.NoError(t, err)
	assert.Len(t, hist, 1, "should not create a duplicate row")
}

func TestOnStartStopTransaction(t *testing.T) {
	s := testServer(t)

	// Start
	now := types.NewDateTime(time.Now())
	startReq := &core.StartTransactionRequest{
		ConnectorId: 1,
		IdTag:       "grosz",
		MeterStart:  1000000, // 1000 kWh in Wh
		Timestamp:   now,
	}

	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	assert.Equal(t, types.AuthorizationStatusAccepted, startConf.IdTagInfo.Status)
	txnID := startConf.TransactionId
	assert.Greater(t, txnID, 0)

	// Verify connector state
	cp := s.ChargePoint("CP001")
	conn := cp.GetConnector(1)
	snap := conn.Snapshot()
	assert.Equal(t, txnID, snap.TransactionID)
	assert.Equal(t, "grosz", snap.IdTag)

	// Verify session in store
	active, err := s.store.ActiveSession()
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, txnID, active.TransactionID)

	// Stop
	stopTime := time.Now().Add(2 * time.Hour)
	stopReq := &core.StopTransactionRequest{
		TransactionId: txnID,
		MeterStop:     1015000, // 1015 kWh in Wh
		Timestamp:     types.NewDateTime(stopTime),
	}

	stopConf, err := s.OnStopTransaction("CP001", stopReq)
	require.NoError(t, err)
	require.NotNil(t, stopConf)

	// Verify connector cleared
	snap = conn.Snapshot()
	assert.Equal(t, 0, snap.TransactionID)

	// Verify session completed
	active, err = s.store.ActiveSession()
	require.NoError(t, err)
	assert.Nil(t, active)

	history, err := s.store.SessionHistory(10, 0)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "completed", history[0].Status)
	assert.InDelta(t, 15.0, history[0].Energy, 0.1) // 1015-1000 kWh

	// And a "stop" chart marker at the OCPP stop timestamp.
	markers, err := s.store.RecentChartMarkers(48)
	require.NoError(t, err)
	stopCount := 0
	for _, m := range markers {
		if m.Type == "stop" {
			stopCount++
			assert.WithinDuration(t, stopTime, m.Timestamp, time.Second,
				"stop marker should match the OCPP stop timestamp")
		}
	}
	assert.Equal(t, 1, stopCount, "expected exactly one stop marker")
}

// When finalizeSessionOnStop already closed the session (because Zappi sent
// SuspendedEV first), the later StopTransaction must NOT add a duplicate stop
// marker. Only the first finalize gets to mark.
func TestOnStopTransaction_AfterSuspend_DoesNotDoubleMark(t *testing.T) {
	s := testServer(t)

	startReq := core.NewStartTransactionRequest(1, "grosz", 0,
		types.NewDateTime(time.Now().Add(-1*time.Hour)))
	startConf, err := s.OnStartTransaction("CP001", startReq)
	require.NoError(t, err)
	txnID := startConf.TransactionId

	mvReq := &core.MeterValuesRequest{
		ConnectorId:   1,
		TransactionId: &txnID,
		MeterValue: []types.MeterValue{{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "5000", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
			},
		}},
	}
	_, err = s.OnMeterValues("CP001", mvReq)
	require.NoError(t, err)

	// SuspendedEV finalize fires first
	susReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusSuspendedEV)
	susReq.Timestamp = types.NewDateTime(time.Now())
	_, err = s.OnStatusNotification("CP001", susReq)
	require.NoError(t, err)

	// Real StopTransaction arrives later (after unplug)
	_, err = s.OnStopTransaction("CP001", &core.StopTransactionRequest{
		TransactionId: txnID, MeterStop: 5000,
		Timestamp: types.NewDateTime(time.Now().Add(time.Minute)),
	})
	require.NoError(t, err)

	markers, err := s.store.RecentChartMarkers(48)
	require.NoError(t, err)
	stopCount := 0
	for _, m := range markers {
		if m.Type == "stop" {
			stopCount++
		}
	}
	assert.Equal(t, 1, stopCount, "must not double-mark stop")
}

func TestOnDataTransfer(t *testing.T) {
	s := testServer(t)

	conf, err := s.OnDataTransfer("CP001", core.NewDataTransferRequest("myenergi"))
	require.NoError(t, err)
	assert.Equal(t, core.DataTransferStatusAccepted, conf.Status)
}

// State-aware chart markers: Available followed by Available (e.g. after an
// app restart and Zappi re-emitting current status) must NOT insert a phantom
// "unplug" marker.
func TestOnStatusNotification_DuplicateStatusNoChartMarker(t *testing.T) {
	s := testServer(t)

	// First Available — no prior state, so no marker either way.
	req := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusAvailable)
	_, err := s.OnStatusNotification("CP001", req)
	require.NoError(t, err)

	// Second Available — duplicate. Must not emit "unplug".
	_, err = s.OnStatusNotification("CP001", req)
	require.NoError(t, err)

	markers, err := s.store.RecentChartMarkers(24)
	require.NoError(t, err)
	assert.Empty(t, markers, "duplicate Available must not emit any chart marker")
}

// Real plug-in transition (Available -> Preparing) must insert a "plug" marker.
func TestOnStatusNotification_PlugTransitionEmitsMarker(t *testing.T) {
	s := testServer(t)

	availableReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusAvailable)
	_, err := s.OnStatusNotification("CP001", availableReq)
	require.NoError(t, err)

	preparingReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusPreparing)
	_, err = s.OnStatusNotification("CP001", preparingReq)
	require.NoError(t, err)

	markers, err := s.store.RecentChartMarkers(24)
	require.NoError(t, err)
	require.Len(t, markers, 1)
	assert.Equal(t, "plug", markers[0].Type)
}

// Real unplug transition (Charging -> Available) must insert an "unplug" marker.
func TestOnStatusNotification_UnplugTransitionEmitsMarker(t *testing.T) {
	s := testServer(t)

	// Establish prior state with non-Available status
	chargingReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusCharging)
	_, err := s.OnStatusNotification("CP001", chargingReq)
	require.NoError(t, err)

	availableReq := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusAvailable)
	_, err = s.OnStatusNotification("CP001", availableReq)
	require.NoError(t, err)

	markers, err := s.store.RecentChartMarkers(24)
	require.NoError(t, err)
	require.Len(t, markers, 1)
	assert.Equal(t, "unplug", markers[0].Type)
}

// First-ever Available with no prior state must not emit "unplug" — we have
// no claim that anything was previously plugged.
func TestOnStatusNotification_FirstAvailableNoUnplug(t *testing.T) {
	s := testServer(t)

	req := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusAvailable)
	_, err := s.OnStatusNotification("CP001", req)
	require.NoError(t, err)

	markers, err := s.store.RecentChartMarkers(24)
	require.NoError(t, err)
	assert.Empty(t, markers)
}

// Repeat StatusNotification updates last_status_notification timestamp so the
// stale-state checker doesn't keep firing TriggerMessage.
func TestOnStatusNotification_TouchesLastNotification(t *testing.T) {
	s := testServer(t)

	t1 := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return t1 }
	defer func() { timeNow = time.Now }()

	req := core.NewStatusNotificationRequest(1, core.NoError, core.ChargePointStatusCharging)
	_, err := s.OnStatusNotification("CP001", req)
	require.NoError(t, err)

	cs1, err := s.store.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs1)
	assert.True(t, cs1.LastStatusNotification.Equal(t1))

	t2 := t1.Add(20 * time.Minute)
	timeNow = func() time.Time { return t2 }

	_, err = s.OnStatusNotification("CP001", req)
	require.NoError(t, err)

	cs2, err := s.store.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs2)
	assert.True(t, cs2.LastStatusNotification.Equal(t2),
		"duplicate StatusNotification still updates last_status_notification")
	assert.Equal(t, "Charging", cs2.Status)
}

// StartTransaction persists transaction_id in connector_state so it survives a
// restart.
func TestOnStartTransaction_PersistsConnectorState(t *testing.T) {
	s := testServer(t)

	req := &core.StartTransactionRequest{
		ConnectorId: 1, IdTag: "grosz", MeterStart: 0,
		Timestamp: types.NewDateTime(time.Now()),
	}
	conf, err := s.OnStartTransaction("CP001", req)
	require.NoError(t, err)

	cs, err := s.store.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs)
	assert.Equal(t, conf.TransactionId, cs.TransactionID)
	assert.Equal(t, "grosz", cs.IdTag)
}

// StopTransaction clears transaction_id from connector_state.
func TestOnStopTransaction_ClearsConnectorTransaction(t *testing.T) {
	s := testServer(t)

	now := types.NewDateTime(time.Now())
	startConf, err := s.OnStartTransaction("CP001", &core.StartTransactionRequest{
		ConnectorId: 1, IdTag: "grosz", MeterStart: 0, Timestamp: now,
	})
	require.NoError(t, err)
	txnID := startConf.TransactionId

	_, err = s.OnStopTransaction("CP001", &core.StopTransactionRequest{
		TransactionId: txnID, MeterStop: 0, Timestamp: now,
	})
	require.NoError(t, err)

	cs, err := s.store.GetConnectorState("CP001", 1)
	require.NoError(t, err)
	require.NotNil(t, cs)
	assert.Equal(t, 0, cs.TransactionID, "transaction must be cleared on stop")
	assert.Empty(t, cs.IdTag)
}

// Hydration on server start: existing connector_state rows should populate
// the in-memory ChargePoint + Connector before any handlers run, with
// Connected=false until a real websocket connect.
func TestNewServer_HydratesFromConnectorState(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dbPath, log)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()

	at := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertConnectorState(store.ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1,
		Status: "Charging", StatusAt: at,
		TransactionID: 99, IdTag: "grosz",
		LastStatusNotification: at,
	}))

	s := NewServer(st, log)
	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp, "hydration must create the ChargePoint")
	assert.False(t, cp.IsConnected(), "hydrated CP starts disconnected")
	conn := cp.GetConnector(1)
	snap := conn.Snapshot()
	assert.Equal(t, "Charging", snap.Status)
	assert.Equal(t, 99, snap.TransactionID)
	assert.Equal(t, "grosz", snap.IdTag)
}

func TestTransactionIDIncrement(t *testing.T) {
	s := testServer(t)

	now := types.NewDateTime(time.Now())
	ids := make([]int, 3)
	for i := range ids {
		req := &core.StartTransactionRequest{ConnectorId: 1, IdTag: "tag", MeterStart: 0, Timestamp: now}
		conf, err := s.OnStartTransaction("CP001", req)
		require.NoError(t, err)
		ids[i] = conf.TransactionId
		// Stop so next start can work
		_, _ = s.OnStopTransaction("CP001", &core.StopTransactionRequest{TransactionId: conf.TransactionId, MeterStop: 0, Timestamp: now})
	}

	assert.Equal(t, ids[0]+1, ids[1])
	assert.Equal(t, ids[1]+1, ids[2])
}
