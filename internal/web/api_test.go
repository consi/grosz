package web

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
	"github.com/consi/grosz/testutil"
)

var apiPortMu sync.Mutex
var apiNextPort = 19500

func allocAPIPort() int {
	apiPortMu.Lock()
	defer apiPortMu.Unlock()
	p := apiNextPort
	apiNextPort++
	return p
}

// noopTariff is a stub Provider for tests that don't exercise tariff endpoints.
type noopTariff struct{}

func (noopTariff) Rates() ([]tariff.Rate, error) { return nil, nil }
func (noopTariff) Name() string                  { return "noop" }
func (noopTariff) Stop()                         {}

func newAPITestServer(t *testing.T) (*Server, *testutil.TestChargePoint, *ocpp.Server) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	srv := ocpp.NewServer(st, log)

	port := allocAPIPort()
	go srv.Start(port, "/{ws}")
	t.Cleanup(func() { srv.Stop() })
	time.Sleep(150 * time.Millisecond)

	url := fmt.Sprintf("ws://localhost:%d", port)
	tcp := testutil.NewTestChargePoint(t, url, "CP_API")
	time.Sleep(100 * time.Millisecond)
	_, err = tcp.SendBootNotification("Vendor", "Model")
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)

	web := New(srv, st, noopTariff{}, nil, nil, "test-boot", "test", "test", log)
	return web, tcp, srv
}

func TestAPI_ChargerReset_InvalidJSON(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/charger/CP_API/reset", bytes.NewReader([]byte("not json")))
	req.SetPathValue("id", "CP_API")
	w := httptest.NewRecorder()
	web.handleChargerReset(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAPI_ChargerReset_BadType(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/charger/CP_API/reset",
		bytes.NewReader([]byte(`{"type":"Catastrophic"}`)))
	req.SetPathValue("id", "CP_API")
	w := httptest.NewRecorder()
	web.handleChargerReset(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Hard or Soft")
}

func TestAPI_ChargerReset_HardOK(t *testing.T) {
	web, tcp, _ := newAPITestServer(t)

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/charger/CP_API/reset",
			bytes.NewReader([]byte(`{"type":"Hard"}`)))
		req.SetPathValue("id", "CP_API")
		w := httptest.NewRecorder()
		web.handleChargerReset(w, req)
		done <- w.Code
	}()

	rt, err := tcp.WaitReset(2 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, "Hard", string(rt))
	assert.Equal(t, http.StatusOK, <-done)
}

func TestAPI_ChargerClearCache_OK(t *testing.T) {
	web, tcp, _ := newAPITestServer(t)

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/charger/CP_API/clear-cache", nil)
		req.SetPathValue("id", "CP_API")
		w := httptest.NewRecorder()
		web.handleChargerClearCache(w, req)
		done <- w.Code
	}()

	require.NoError(t, tcp.WaitClearCache(2*time.Second))
	assert.Equal(t, http.StatusOK, <-done)
}

func TestAPI_ChargerUpdateFirmware_OK(t *testing.T) {
	web, tcp, _ := newAPITestServer(t)

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/charger/CP_API/update-firmware", nil)
		req.SetPathValue("id", "CP_API")
		w := httptest.NewRecorder()
		web.handleChargerUpdateFirmware(w, req)
		done <- w.Code
	}()

	got, err := tcp.WaitUpdateFirmware(2 * time.Second)
	require.NoError(t, err)
	assert.NotEmpty(t, got.Location, "location placeholder must be present")
	require.NotNil(t, got.RetrieveDate)
	assert.Equal(t, time.UTC, got.RetrieveDate.Location())
	assert.Equal(t, http.StatusOK, <-done)
}
