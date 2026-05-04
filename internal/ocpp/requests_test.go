package ocpp_test

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ocppserver "github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/testutil"
)

// portMu serializes port allocation to avoid collisions when go test -race
// runs subtests in parallel and we share package-level port counters.
var portMu sync.Mutex
var nextTestPort = 19000

func allocPort() int {
	portMu.Lock()
	defer portMu.Unlock()
	p := nextTestPort
	nextTestPort++
	return p
}

// integrationServer boots a real OCPP server + test charge point so that
// CS->CP request methods can be exercised over the wire.
func integrationServer(t *testing.T) (*ocppserver.Server, *testutil.TestChargePoint) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), log)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	srv := ocppserver.NewServer(st, log)

	port := allocPort()
	go srv.Start(port, "/{ws}")
	t.Cleanup(func() { srv.Stop() })
	time.Sleep(150 * time.Millisecond)

	url := fmt.Sprintf("ws://localhost:%d", port)
	tcp := testutil.NewTestChargePoint(t, url, "CP_REQ")
	time.Sleep(100 * time.Millisecond)

	_, err = tcp.SendBootNotification("Vendor", "Model")
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)

	return srv, tcp
}

func TestRequest_ResetSoft(t *testing.T) {
	srv, tcp := integrationServer(t)

	done := make(chan error, 1)
	go func() { done <- srv.Reset("CP_REQ", core.ResetTypeSoft) }()

	rt, err := tcp.WaitReset(2 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, core.ResetTypeSoft, rt)
	require.NoError(t, <-done)
}

func TestRequest_ResetHard(t *testing.T) {
	srv, tcp := integrationServer(t)

	done := make(chan error, 1)
	go func() { done <- srv.Reset("CP_REQ", core.ResetTypeHard) }()

	rt, err := tcp.WaitReset(2 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, core.ResetTypeHard, rt)
	require.NoError(t, <-done)
}

func TestRequest_ClearCache(t *testing.T) {
	srv, tcp := integrationServer(t)

	done := make(chan error, 1)
	go func() { done <- srv.ClearCache("CP_REQ") }()

	require.NoError(t, tcp.WaitClearCache(2*time.Second))
	require.NoError(t, <-done)
}

func TestRequest_UpdateFirmwareForcesUTC(t *testing.T) {
	srv, tcp := integrationServer(t)

	// Use a non-UTC timezone to confirm the server normalizes.
	loc, err := time.LoadLocation("Europe/Warsaw")
	require.NoError(t, err)
	retrieve := time.Date(2026, 4, 29, 10, 0, 0, 0, loc)

	done := make(chan error, 1)
	go func() { done <- srv.UpdateFirmware("CP_REQ", "https://example.com/fw", retrieve) }()

	req, err := tcp.WaitUpdateFirmware(2 * time.Second)
	require.NoError(t, err)
	require.NotNil(t, req.RetrieveDate)
	assert.Equal(t, time.UTC, req.RetrieveDate.Location(),
		"retrieveDate must arrive at the charger as UTC")
	// The instant should be preserved.
	assert.True(t, req.RetrieveDate.Equal(retrieve.UTC()))
	assert.Equal(t, "https://example.com/fw", req.Location)
	require.NoError(t, <-done)
}
