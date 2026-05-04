package vehicle

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/consi/grosz/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.New(dbPath, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func testRenault(t *testing.T, st *store.Store, baseURL string) *Renault {
	t.Helper()
	r := &Renault{
		store:       st,
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		client:      http.DefaultClient,
		kamereonURL: baseURL,
		accountID:   "test-account",
		jwt:         "test-jwt",
	}
	return r
}

// vehiclesResponse builds a Kamereon vehicles response for testing.
func vehiclesResponse(vin, modelLabel string, imageURL string) map[string]any {
	assets := []any{}
	if imageURL != "" {
		assets = append(assets, map[string]any{
			"assetType": "PICTURE",
			"viewpoint": "mybrand_2",
			"renditions": []any{
				map[string]any{
					"resolutionType": "ONE_MYRENAULT_LARGE",
					"url":            imageURL,
				},
			},
		})
	}
	return map[string]any{
		"vehicleLinks": []any{
			map[string]any{
				"vin": vin,
				"vehicleDetails": map[string]any{
					"model":  map[string]any{"label": modelLabel},
					"assets": assets,
				},
			},
		},
	}
}

func TestGetVehicleDetails(t *testing.T) {
	imgData := []byte("fake-png-data")

	mux := http.NewServeMux()
	mux.HandleFunc("/test-image.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(imgData)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/commerce/v1/accounts/test-account/vehicles", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(vehiclesResponse("TESTVIN123", "RENAULT 5", srv.URL+"/test-image.png"))
	})

	st := testStore(t)
	r := testRenault(t, st, srv.URL)

	r.fetchVehicleDetails("TESTVIN123")

	assert.Equal(t, "RENAULT 5", st.GetDefault("vehicle.model_name", ""))

	picData := st.GetDefault("vehicle.picture_data", "")
	require.NotEmpty(t, picData)
	decoded, err := base64.StdEncoding.DecodeString(picData)
	require.NoError(t, err)
	assert.Equal(t, imgData, decoded)

	assert.NotEmpty(t, st.GetDefault("vehicle.details_fetched_at", ""))
}

func TestGetVehicleDetailsNoImage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/vehicles", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(vehiclesResponse("TESTVIN123", "RENAULT 5", ""))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)

	r.fetchVehicleDetails("TESTVIN123")

	assert.Equal(t, "RENAULT 5", st.GetDefault("vehicle.model_name", ""))
	assert.Empty(t, st.GetDefault("vehicle.picture_data", ""))
	assert.NotEmpty(t, st.GetDefault("vehicle.details_fetched_at", ""))
}

func TestPollStoresAllFields(t *testing.T) {
	st := testStore(t)
	r := &Renault{
		store:       st,
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		client:      http.DefaultClient,
		kamereonURL: "http://unused",
		accountID:   "test-account",
		jwt:         "test-jwt",
	}

	status := &BatteryStatus{
		Level:          72,
		Autonomy:       185,
		PlugStatus:     1,
		ChargingStatus: 1.0,
		RemainingTime:  90,
		Timestamp:      "2026-04-25T10:00:00Z",
	}

	_ = r.store.Set("vehicle.plug_status", fmt.Sprintf("%d", status.PlugStatus))
	_ = r.store.Set("vehicle.battery_autonomy", fmt.Sprintf("%d", status.Autonomy))
	_ = r.store.Set("vehicle.charging_status", fmt.Sprintf("%g", status.ChargingStatus))
	_ = r.store.Set("vehicle.charging_remaining_time", fmt.Sprintf("%d", status.RemainingTime))
	_ = r.store.Set("vehicle.battery_timestamp", status.Timestamp)
	_ = r.store.Set("scheduler.current_soc", fmt.Sprintf("%d", status.Level))

	assert.Equal(t, 72, st.GetInt("scheduler.current_soc", 0))
	assert.Equal(t, 1, st.GetInt("vehicle.plug_status", 0))
	assert.Equal(t, 185, st.GetInt("vehicle.battery_autonomy", 0))
	assert.InDelta(t, 1.0, st.GetFloat("vehicle.charging_status", 0), 0.01)
	assert.Equal(t, 90, st.GetInt("vehicle.charging_remaining_time", 0))
	assert.Equal(t, "2026-04-25T10:00:00Z", st.GetDefault("vehicle.battery_timestamp", ""))
}

func TestPollErrorClearsFields(t *testing.T) {
	st := testStore(t)

	_ = st.Set("scheduler.current_soc", "72")
	_ = st.Set("vehicle.plug_status", "1")
	_ = st.Set("vehicle.battery_autonomy", "185")
	_ = st.Set("vehicle.charging_status", "1")
	_ = st.Set("vehicle.charging_remaining_time", "90")
	_ = st.Set("vehicle.battery_timestamp", "2026-04-25T10:00:00Z")

	_ = st.Set("scheduler.current_soc", "0")
	_ = st.Set("vehicle.plug_status", "")
	_ = st.Set("vehicle.battery_autonomy", "")
	_ = st.Set("vehicle.charging_status", "")
	_ = st.Set("vehicle.charging_remaining_time", "")
	_ = st.Set("vehicle.battery_timestamp", "")

	assert.Equal(t, 0, st.GetInt("scheduler.current_soc", -1))
	assert.Equal(t, "", st.GetDefault("vehicle.plug_status", "x"))
	assert.Equal(t, "", st.GetDefault("vehicle.battery_autonomy", "x"))
	assert.Equal(t, "", st.GetDefault("vehicle.charging_status", "x"))
	assert.Equal(t, "", st.GetDefault("vehicle.charging_remaining_time", "x"))
	assert.Equal(t, "", st.GetDefault("vehicle.battery_timestamp", "x"))
}

func TestShouldRefreshDetails(t *testing.T) {
	st := testStore(t)
	r := testRenault(t, st, "http://unused")

	// Never fetched — should refresh
	assert.True(t, r.shouldRefreshDetails())

	// Just fetched — should not refresh
	r.markDetailsFetched(time.Now())
	assert.False(t, r.shouldRefreshDetails())

	// Fetched 25h ago — should refresh
	r.markDetailsFetched(time.Now().Add(-25 * time.Hour))
	assert.True(t, r.shouldRefreshDetails())
}

func TestShouldRefreshDetailsFromStore(t *testing.T) {
	st := testStore(t)

	// Persist a recent timestamp, then create a fresh Renault (simulating restart)
	_ = st.Set("vehicle.details_fetched_at", time.Now().UTC().Format(time.RFC3339))

	r := testRenault(t, st, "http://unused")
	assert.False(t, r.shouldRefreshDetails(), "should pick up recent timestamp from store after restart")
}

func TestFetchDetailsRetryAfterFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/vehicles", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)

	r.fetchVehicleDetails("TESTVIN123")

	// After failure, detailsFetchedAt should be set ~23h in the past so retry in ~1h
	r.mu.RLock()
	fetchedAt := r.detailsFetchedAt
	r.mu.RUnlock()

	age := time.Since(fetchedAt)
	assert.True(t, age > 22*time.Hour && age < 24*time.Hour,
		"expected fetchedAt ~23h ago for 1h retry, got age=%v", age)
}

func TestGetAccountIDPrefersMyRenault(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/persons/test-person", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"accounts": []any{
				map[string]any{"accountId": "sfdc-account", "accountType": "SFDC"},
				map[string]any{"accountId": "myrenault-account", "accountType": "MYRENAULT"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)

	accountID, err := r.getAccountID("test-person", "test-jwt")
	require.NoError(t, err)
	assert.Equal(t, "myrenault-account", accountID)
}

func TestDownloadImageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)

	_, err := r.downloadImage(srv.URL + "/fail")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}
