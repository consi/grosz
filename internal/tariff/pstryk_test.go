package tariff_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

func TestPstrykParsesRates(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	frames := makeFrames(now, 24, 0.50, 0.01) // 24 hours, varying prices

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "pricing", r.URL.Query().Get("metrics"))
		json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	// Wait for initial fetch
	time.Sleep(500 * time.Millisecond)

	rates, err := p.Rates()
	require.NoError(t, err)
	assert.Equal(t, 24, len(rates))
	assert.Equal(t, 0.50, rates[0].Price)
}

func TestPstrykPlaceholderDetection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	todayEnd := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

	// Today: 24 varying prices
	var frames []map[string]any
	for i := 0; i < 24; i++ {
		start := todayEnd.Add(time.Duration(i-24) * time.Hour)
		end := start.Add(time.Hour)
		frames = append(frames, map[string]any{
			"start":   start.Format(time.RFC3339),
			"end":     end.Format(time.RFC3339),
			"is_live": true,
			"metrics": map[string]any{"pricing": map[string]any{"price_gross": 0.50 + float64(i)*0.01}},
		})
	}
	// Tomorrow: 24 identical prices (placeholder/forecast)
	for i := 0; i < 24; i++ {
		start := todayEnd.Add(time.Duration(i) * time.Hour)
		end := start.Add(time.Hour)
		frames = append(frames, map[string]any{
			"start":   start.Format(time.RFC3339),
			"end":     end.Format(time.RFC3339),
			"is_live": false,
			"metrics": map[string]any{"pricing": map[string]any{"price_gross": 0.65}},
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	time.Sleep(500 * time.Millisecond)

	rates, err := p.Rates()
	require.NoError(t, err)
	// Tomorrow's placeholder rates should be filtered out
	for _, r := range rates {
		assert.True(t, r.Start.Before(todayEnd), "should not include tomorrow's placeholder rates")
	}
}

func TestPstryk429Handling(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(429)
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	time.Sleep(500 * time.Millisecond)

	_, err := p.Rates()
	assert.Error(t, err, "should have no rates after 429")
	assert.GreaterOrEqual(t, calls.Load(), int32(1))
}

func TestPstrykNoToken(t *testing.T) {
	st := testStore(t)
	// No token configured

	p := tariff.NewPstrykWithURL(st, testLogger(), "http://localhost:1")
	defer p.Stop()

	time.Sleep(200 * time.Millisecond)

	_, err := p.Rates()
	assert.Error(t, err)
}

func TestPstrykCachesRates(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	frames := makeFrames(now, 12, 0.40, 0.02)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	// First instance fetches and caches
	p1 := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	time.Sleep(500 * time.Millisecond)
	p1.Stop()

	// Second instance loads from cache
	srv.Close() // server is down
	p2 := tariff.NewPstrykWithURL(st, testLogger(), "http://localhost:1")
	defer p2.Stop()

	rates, err := p2.Rates()
	require.NoError(t, err)
	assert.Greater(t, len(rates), 0, "should load from cache")
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), log)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func makeFrames(start time.Time, count int, basePrice, increment float64) []map[string]any {
	var frames []map[string]any
	for i := 0; i < count; i++ {
		s := start.Add(time.Duration(i) * time.Hour)
		e := s.Add(time.Hour)
		frames = append(frames, map[string]any{
			"start":   s.Format(time.RFC3339),
			"end":     e.Format(time.RFC3339),
			"is_live": true,
			"metrics": map[string]any{"pricing": map[string]any{"price_gross": basePrice + float64(i)*increment}},
		})
	}
	return frames
}
