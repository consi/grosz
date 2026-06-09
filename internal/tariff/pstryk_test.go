package tariff_test

import (
	"context"
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
		metrics := r.URL.Query().Get("metrics")
		switch metrics {
		case "pricing":
			_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
		case "meter_values":
			_ = json.NewEncoder(w).Encode(map[string]any{"frames": []map[string]any{}})
		default:
			t.Errorf("unexpected metrics=%q", metrics)
		}
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
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
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
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
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

// TestPstrykFetchConsumption_FaeUsage covers the newer response shape where
// each frame carries a top-level fae_usage field (kWh).
func TestPstrykFetchConsumption_FaeUsage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Background refreshLoop also hits this server with metrics=pricing.
		// Route by metrics so the direct FetchConsumption call (below) gets
		// the consumption frames it expects.
		if r.URL.Query().Get("metrics") != "meter_values" {
			_ = json.NewEncoder(w).Encode(map[string]any{"frames": []map[string]any{}})
			return
		}
		assert.Equal(t, "test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "hour", r.URL.Query().Get("resolution"))
		assert.Empty(t, r.URL.Query().Get("for_tz"), "for_tz must not be set on hour-resolution requests")

		var frames []map[string]any
		for i := 0; i < 3; i++ {
			s := now.Add(time.Duration(i) * time.Hour)
			frames = append(frames, map[string]any{
				"start":     s.Format(time.RFC3339),
				"end":       s.Add(time.Hour).Format(time.RFC3339),
				"is_live":   false,
				"fae_usage": 1.25 + float64(i)*0.1,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	frames, err := p.FetchConsumption(context.Background(), now, now.Add(3*time.Hour))
	require.NoError(t, err)
	require.Len(t, frames, 3)
	assert.InDelta(t, 1250.0, frames[0].EnergyWh, 0.01) // 1.25 kWh → 1250 Wh
	assert.InDelta(t, 1350.0, frames[1].EnergyWh, 0.01)
	assert.True(t, frames[0].Hour.Equal(now), "hour key matches frame start (UTC)")
}

// TestPstrykFetchConsumption_MeterValuesFallback covers the older response
// shape where energy_active_import_register lives under metrics.meter_values.
func TestPstrykFetchConsumption_MeterValuesFallback(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var frames []map[string]any
		for i := 0; i < 2; i++ {
			s := now.Add(time.Duration(i) * time.Hour)
			frames = append(frames, map[string]any{
				"start": s.Format(time.RFC3339),
				"end":   s.Add(time.Hour).Format(time.RFC3339),
				"metrics": map[string]any{
					"meter_values": map[string]any{
						"energy_active_import_register": 0.80 + float64(i)*0.05,
					},
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	frames, err := p.FetchConsumption(context.Background(), now, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.InDelta(t, 800.0, frames[0].EnergyWh, 0.01)
	assert.InDelta(t, 850.0, frames[1].EnergyWh, 0.01)
}

// TestPstrykFetchConsumption_SkipsEmptyFrames ensures frames with neither
// fae_usage nor meter_values.energy_active_import_register are dropped (so a
// pricing-only response doesn't pollute the consumption table).
func TestPstrykFetchConsumption_SkipsEmptyFrames(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frames := []map[string]any{
			// Pricing-only frame — should be skipped.
			{
				"start":   now.Format(time.RFC3339),
				"end":     now.Add(time.Hour).Format(time.RFC3339),
				"metrics": map[string]any{"pricing": map[string]any{"price_gross": 0.5}},
			},
			// Valid consumption frame.
			{
				"start":     now.Add(time.Hour).Format(time.RFC3339),
				"end":       now.Add(2 * time.Hour).Format(time.RFC3339),
				"fae_usage": 0.5,
			},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	frames, err := p.FetchConsumption(context.Background(), now, now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.InDelta(t, 500.0, frames[0].EnergyWh, 0.01)
}

// TestPstrykFetchConsumption_NoToken returns an error so the caller knows to
// skip the upsert and event-log a configuration warning.
func TestPstrykFetchConsumption_NoToken(t *testing.T) {
	st := testStore(t)
	p := tariff.NewPstrykWithURL(st, testLogger(), "http://localhost:1")
	defer p.Stop()

	_, err := p.FetchConsumption(context.Background(), time.Now().Add(-time.Hour), time.Now())
	assert.Error(t, err)
}

// TestPstrykFetchConsumption_HourKeyIsUTC verifies that frames whose start is
// expressed in a non-UTC offset are normalized to UTC top-of-hour. Important
// for DST safety: hour keys must collide across the spring-forward day.
func TestPstrykFetchConsumption_HourKeyIsUTC(t *testing.T) {
	// 02:00 in CEST (+02:00) is 00:00 UTC.
	frameStart := "2026-06-15T02:00:00+02:00"
	expectedHour := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frames := []map[string]any{
			{
				"start":     frameStart,
				"end":       "2026-06-15T03:00:00+02:00",
				"fae_usage": 0.75,
			},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	frames, err := p.FetchConsumption(context.Background(), time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.True(t, frames[0].Hour.Equal(expectedHour),
		"expected %s got %s", expectedHour.Format(time.RFC3339), frames[0].Hour.Format(time.RFC3339))
	assert.Equal(t, time.UTC, frames[0].Hour.Location())
}

// TestPstrykRefreshToday_HourFrame: Pstryk returns an hour-resolution frame
// for the in-progress hour — it lands in the store with the finalized hours.
func TestPstrykRefreshToday_HourFrame(t *testing.T) {
	currentHour := time.Now().UTC().Truncate(time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("metrics") != "meter_values" || q.Get("resolution") != "hour" {
			_ = json.NewEncoder(w).Encode(map[string]any{"frames": []map[string]any{}})
			return
		}
		// Only the live refresh asks for a window extending past the current
		// hour; the backfill loop is clamped to finalized hours.
		we, _ := time.Parse(time.RFC3339, q.Get("window_end"))
		if !we.After(currentHour) {
			_ = json.NewEncoder(w).Encode(map[string]any{"frames": []map[string]any{}})
			return
		}
		frames := []map[string]any{
			{
				"start":     currentHour.Add(-time.Hour).Format(time.RFC3339),
				"end":       currentHour.Format(time.RFC3339),
				"is_live":   false,
				"fae_usage": 1.0,
			},
			{
				"start":     currentHour.Format(time.RFC3339),
				"end":       currentHour.Add(time.Hour).Format(time.RFC3339),
				"is_live":   true,
				"fae_usage": 0.42,
			},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	require.NoError(t, p.RefreshTodayConsumption(context.Background()))

	latest, ok := st.LatestPstrykHour()
	require.True(t, ok)
	assert.True(t, latest.Equal(currentHour), "in-progress hour must be stored")

	rows, err := st.PstrykHourlyConsumption(48)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	last := rows[len(rows)-1]
	assert.True(t, last.Hour.Equal(currentHour))
	assert.InDelta(t, 420.0, last.EnergyWh, 0.01) // 0.42 kWh → 420 Wh
}

// TestPstrykRefreshToday_FinalizedOnly: Pstryk (as observed in production)
// returns no frame for the in-progress hour — only finalized hours are
// stored, and nothing is fabricated for the current hour.
func TestPstrykRefreshToday_FinalizedOnly(t *testing.T) {
	currentHour := time.Now().UTC().Truncate(time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("metrics") != "meter_values" || q.Get("resolution") != "hour" {
			_ = json.NewEncoder(w).Encode(map[string]any{"frames": []map[string]any{}})
			return
		}
		frames := []map[string]any{
			{
				"start":     currentHour.Add(-2 * time.Hour).Format(time.RFC3339),
				"end":       currentHour.Add(-time.Hour).Format(time.RFC3339),
				"fae_usage": 1.0,
			},
			{
				"start":     currentHour.Add(-time.Hour).Format(time.RFC3339),
				"end":       currentHour.Format(time.RFC3339),
				"fae_usage": 1.0,
			},
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	}))
	defer srv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("tariff.pstryk_token", "test-token"))

	p := tariff.NewPstrykWithURL(st, testLogger(), srv.URL)
	defer p.Stop()

	require.NoError(t, p.RefreshTodayConsumption(context.Background()))

	latest, ok := st.LatestPstrykHour()
	require.True(t, ok)
	assert.True(t, latest.Equal(currentHour.Add(-time.Hour)),
		"only finalized hours stored, got %s", latest.Format(time.RFC3339))
}

// TestPstrykRefreshToday_NoToken: silently a no-op (the backfill loop owns
// the missing-token warning).
func TestPstrykRefreshToday_NoToken(t *testing.T) {
	st := testStore(t)
	p := tariff.NewPstrykWithURL(st, testLogger(), "http://localhost:1")
	defer p.Stop()

	require.NoError(t, p.RefreshTodayConsumption(context.Background()))
	_, ok := st.LatestPstrykHour()
	assert.False(t, ok)
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"), log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
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
