package testutil_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ocppserver "github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/testutil"
)

func TestIntegrationBootAndHeartbeat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), log)
	require.NoError(t, err)
	defer st.Close()

	// Start OCPP server
	srv := ocppserver.NewServer(st, log)
	port := 18887
	go srv.Start(port, "/{ws}")
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond) // let server start

	// Connect test charge point
	url := fmt.Sprintf("ws://localhost:%d", port)
	tcp := testutil.NewTestChargePoint(t, url, "TEST001")
	time.Sleep(200 * time.Millisecond) // let connection establish

	// Send BootNotification
	bootConf, err := tcp.SendBootNotification("Myenergi", "Zappi")
	require.NoError(t, err)
	assert.Equal(t, core.RegistrationStatusAccepted, bootConf.Status)
	assert.Equal(t, 120, bootConf.Interval)

	// Verify server state
	time.Sleep(100 * time.Millisecond)
	cp := srv.ChargePoint("TEST001")
	require.NotNil(t, cp)
	assert.True(t, cp.IsConnected())
	cp.SetBootInfo(nil) // just verify it doesn't panic

	// Send Heartbeat
	hbConf, err := tcp.SendHeartbeat()
	require.NoError(t, err)
	assert.False(t, hbConf.CurrentTime.Time.IsZero())

	// Verify events recorded
	events, err := st.RecentEvents(10, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(events), 2) // boot recv + heartbeat recv at minimum
}

func TestIntegrationChargingSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), log)
	require.NoError(t, err)
	defer st.Close()

	srv := ocppserver.NewServer(st, log)
	port := 18888
	go srv.Start(port, "/{ws}")
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	url := fmt.Sprintf("ws://localhost:%d", port)
	tcp := testutil.NewTestChargePoint(t, url, "TEST002")
	time.Sleep(200 * time.Millisecond)

	// Boot
	_, err = tcp.SendBootNotification("Myenergi", "Zappi")
	require.NoError(t, err)

	// Status: Available
	require.NoError(t, tcp.SendStatusNotification(1, core.ChargePointStatusAvailable))
	time.Sleep(50 * time.Millisecond)

	// Status: Preparing (car plugged in)
	require.NoError(t, tcp.SendStatusNotification(1, core.ChargePointStatusPreparing))
	time.Sleep(50 * time.Millisecond)

	// Start transaction
	startConf, err := tcp.SendStartTransaction(1, "virtual-tag", 1000000)
	require.NoError(t, err)
	txnID := startConf.TransactionId
	assert.Greater(t, txnID, 0)

	// Status: Charging
	require.NoError(t, tcp.SendStatusNotification(1, core.ChargePointStatusCharging))

	// Send meter values
	require.NoError(t, tcp.SendMeterValues(1, []types.MeterValue{
		{
			Timestamp: types.NewDateTime(time.Now()),
			SampledValue: []types.SampledValue{
				{Value: "7360", Measurand: types.MeasurandPowerActiveImport, Unit: types.UnitOfMeasureW},
				{Value: "230", Measurand: types.MeasurandVoltage, Unit: types.UnitOfMeasureV},
				{Value: "1005000", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
			},
		},
	}))
	time.Sleep(50 * time.Millisecond)

	// Verify connector state
	cp := srv.ChargePoint("TEST002")
	require.NotNil(t, cp)
	conn := cp.GetConnector(1)
	snap := conn.Snapshot()
	assert.Equal(t, "Charging", snap.Status)
	assert.InDelta(t, 7360, snap.Measurements["Power.Active.Import"].Value, 1)

	// Active session exists
	sess, err := st.ActiveSession()
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, txnID, sess.TransactionID)

	// Stop transaction
	_, err = tcp.SendStopTransaction(txnID, 1015000)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)

	// Session completed
	active, err := st.ActiveSession()
	require.NoError(t, err)
	assert.Nil(t, active)

	history, err := st.SessionHistory(10, 0)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "completed", history[0].Status)
	assert.InDelta(t, 15.0, history[0].Energy, 0.5) // ~15 kWh
}

func TestIntegrationRemoteStartStop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), log)
	require.NoError(t, err)
	defer st.Close()

	srv := ocppserver.NewServer(st, log)
	port := 18889
	go srv.Start(port, "/{ws}")
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	url := fmt.Sprintf("ws://localhost:%d", port)
	tcp := testutil.NewTestChargePoint(t, url, "TEST003")
	_ = tcp
	time.Sleep(200 * time.Millisecond)

	// Boot
	_, err = tcp.SendBootNotification("Myenergi", "Zappi")
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)

	// Remote start
	err = srv.RemoteStartTransaction("TEST003", "grosz", 1)
	require.NoError(t, err)

	// Remote stop (need a transaction first)
	startConf, err := tcp.SendStartTransaction(1, "grosz", 0)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)

	err = srv.RemoteStopTransaction("TEST003", startConf.TransactionId)
	require.NoError(t, err)
}
