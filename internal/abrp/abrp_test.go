package abrp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureRecorder records emitted system events in memory for assertions.
type captureRecorder struct{ evs []events.SystemEvent }

func (c *captureRecorder) RecordSystemEvent(e events.SystemEvent) error {
	c.evs = append(c.evs, e)
	return nil
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.New(dbPath, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestClient builds a Client pointed at srv with a capturing event recorder.
func newTestClient(t *testing.T, st *store.Store, srvURL string) (*Client, *captureRecorder) {
	t.Helper()
	c := NewWithURL(st, testLogger(), srvURL)
	rec := &captureRecorder{}
	c.events = events.For(events.SourceABRP, rec)
	return c, rec
}

func TestSendNoOpWhenTokenEmpty(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	st := testStore(t)
	c, rec := newTestClient(t, st, srv.URL)

	require.False(t, c.Enabled())
	err := c.Send(context.Background(), Telemetry{UTC: 1, SoC: 50})
	require.NoError(t, err)
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits), "no HTTP call when token empty")
	assert.Empty(t, rec.evs, "no event emitted when token empty")
}

func TestSendSuccess(t *testing.T) {
	var gotURL, gotCT, gotTLM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotCT = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		gotTLM = r.PostFormValue("tlm")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set(tokenSettingKey, "user-token"))
	c, rec := newTestClient(t, st, srv.URL)
	require.True(t, c.Enabled())

	power := -7.4
	rng := 210.0
	err := c.Send(context.Background(), Telemetry{
		UTC: 1700000000, SoC: 80, IsCharging: 1, Power: &power, EstRange: &rng,
	})
	require.NoError(t, err)

	assert.Contains(t, gotURL, "/1/tlm/send")
	assert.Contains(t, gotURL, "api_key="+genericAPIKey)
	assert.Contains(t, gotURL, "token=user-token")
	assert.Equal(t, "application/x-www-form-urlencoded", gotCT)

	var tlm map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotTLM), &tlm))
	assert.EqualValues(t, 1700000000, tlm["utc"])
	assert.EqualValues(t, 80, tlm["soc"])
	assert.EqualValues(t, 1, tlm["is_charging"])
	assert.InDelta(t, -7.4, tlm["power"], 0.001)
	assert.InDelta(t, 210.0, tlm["est_battery_range"], 0.001)

	require.Len(t, rec.evs, 1)
	assert.Equal(t, string(events.SourceABRP), rec.evs[0].Source)
	assert.Equal(t, string(events.ActionABRPSend), rec.evs[0].Action)
	assert.Equal(t, string(events.LevelInfo), rec.evs[0].Level)
}

func TestSendRejectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"error","error":"invalid token"}`)
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set(tokenSettingKey, "bad"))
	c, rec := newTestClient(t, st, srv.URL)

	err := c.Send(context.Background(), Telemetry{UTC: 1, SoC: 10})
	require.Error(t, err)
	require.Len(t, rec.evs, 1)
	assert.Equal(t, string(events.LevelError), rec.evs[0].Level)
}

func TestSendNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set(tokenSettingKey, "x"))
	c, rec := newTestClient(t, st, srv.URL)

	err := c.Send(context.Background(), Telemetry{UTC: 1, SoC: 10})
	require.Error(t, err)
	require.Len(t, rec.evs, 1)
	assert.Equal(t, string(events.LevelError), rec.evs[0].Level)
}
