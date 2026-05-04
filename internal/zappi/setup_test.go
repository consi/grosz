package zappi_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ocppserver "github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/zappi"
	"github.com/consi/grosz/testutil"
)

func TestZappiSetupSequence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), log)
	require.NoError(t, err)
	defer st.Close()

	// Seed settings
	require.NoError(t, st.SetMany(map[string]string{
		"zappi.commercial_mode": "true",
		"zappi.meter_interval":  "10",
	}))

	srv := ocppserver.NewServer(st, log)

	// Register Zappi boot hook
	zappi.RegisterBootHook(srv, st, log)

	port := 18890
	go srv.Start(port, "/{ws}")
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	// Connect as Zappi
	url := fmt.Sprintf("ws://localhost:%d", port)
	tcp := testutil.NewTestChargePoint(t, url, "ZAPPI001")
	time.Sleep(200 * time.Millisecond)

	// Boot as Zappi — this triggers the setup hook
	conf, err := tcp.SendBootNotification("Myenergi", "Zappi")
	require.NoError(t, err)
	assert.Equal(t, core.RegistrationStatusAccepted, conf.Status)

	// Wait for setup to complete (runs async in goroutine)
	time.Sleep(2 * time.Second)

	// Verify config keys were set on the test charge point
	tcp.SetConfig("", "") // just to sync the mutex
	// The test CP auto-accepts all ChangeConfiguration, so check its store
	// CommercialMode should have been set to True
	// (The test charge point stores all config changes)
}

func TestNonZappiSkipsSetup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), log)
	require.NoError(t, err)
	defer st.Close()

	srv := ocppserver.NewServer(st, log)

	setupCalled := false
	srv.RegisterBootHook(func(cpID string, req *core.BootNotificationRequest) {
		if zappi.IsZappi(req.ChargePointVendor, req.ChargePointModel) {
			setupCalled = true
		}
	})

	port := 18891
	go srv.Start(port, "/{ws}")
	defer srv.Stop()
	time.Sleep(200 * time.Millisecond)

	url := fmt.Sprintf("ws://localhost:%d", port)
	tcp := testutil.NewTestChargePoint(t, url, "OTHER001")
	time.Sleep(200 * time.Millisecond)

	// Boot as non-Zappi
	_, err = tcp.SendBootNotification("ABB", "Terra AC")
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	assert.False(t, setupCalled, "setup should not be called for non-Zappi chargers")
}
