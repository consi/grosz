package vehicle

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/consi/grosz/internal/events"
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
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testRenault(t *testing.T, st *store.Store, baseURL string) *Renault {
	t.Helper()
	r := &Renault{
		store:       st,
		events:      events.For(events.SourceRenault, st),
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		client:      http.DefaultClient,
		kamereonURL: baseURL,
		gigyaURL:    baseURL,
		accountID:   "test-account",
		jwt:         "test-jwt",
		jwtExpiry:   time.Now().Add(time.Hour), // let ensureAuth short-circuit
		triggerCh:   make(chan struct{}, 1),
		plugCh:      make(chan struct{}, 1),
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
		_, _ = w.Write(imgData)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/commerce/v1/accounts/test-account/vehicles", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(vehiclesResponse("TESTVIN123", "RENAULT 5", srv.URL+"/test-image.png"))
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
		_ = json.NewEncoder(w).Encode(vehiclesResponse("TESTVIN123", "RENAULT 5", ""))
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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

// TestGigyaLoginReturnsPendingTFA verifies that a 403101 login response is
// surfaced as a *gigyaError carrying the code and regToken, which is what the
// StartTFA flow keys off to decide a verification is needed.
func TestGigyaLoginReturnsPendingTFA(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts.login", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorCode":    403101,
			"errorMessage": "Account Pending TFA Verification",
			"regToken":     "REG-123",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)

	_, err := r.gigyaLogin("user@example.com", "pw")
	require.Error(t, err)
	var ge *gigyaError
	require.ErrorAs(t, err, &ge)
	assert.Equal(t, 403101, ge.code)
	assert.Equal(t, "REG-123", ge.regToken)
}

// TestTFAFlowHappyPath drives StartTFA → CompleteTFA against a mocked Gigya
// tenant and asserts the durable session + trusted device are persisted and the
// required flag is cleared.
func TestTFAFlowHappyPath(t *testing.T) {
	const (
		regToken   = "REG-123"
		assertion  = "ASSERT-1"
		phvToken   = "PHV-1"
		provAssert = "PROV-1"
		loginToken = "LOGIN-TOKEN-XYZ"
		obfuscated = "j***@example.com"
		code       = "123456"
	)

	// The device starts untrusted (login → 403101 challenge) and becomes trusted
	// once finalizeTFA lands, after which the same login mints a real session —
	// exactly how Gigya treats the gmid before vs. after the TFA flow.
	var deviceTrusted atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/accounts.webSdkBootstrap", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"gmid": "GMID-1", "ucid": "UCID-1"})
	})
	mux.HandleFunc("/accounts.login", func(w http.ResponseWriter, req *http.Request) {
		if deviceTrusted.Load() {
			_ = req.ParseForm()
			assert.Equal(t, "GMID-1", req.FormValue("gmid"), "post-TFA login must present the trusted device")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sessionInfo": map[string]any{"cookieValue": loginToken},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errorCode":    403101,
			"errorMessage": "pending tfa",
			"regToken":     regToken,
		})
	})
	mux.HandleFunc("/accounts.tfa.initTFA", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"gigyaAssertion": assertion})
	})
	mux.HandleFunc("/accounts.tfa.email.getEmails", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"emails": []any{map[string]any{"id": "email-1", "obfuscated": obfuscated}},
		})
	})
	mux.HandleFunc("/accounts.tfa.email.sendVerificationCode", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"phvToken": phvToken})
	})
	mux.HandleFunc("/accounts.tfa.email.completeVerification", func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		assert.Equal(t, code, req.FormValue("code"))
		assert.Equal(t, phvToken, req.FormValue("phvToken"))
		_ = json.NewEncoder(w).Encode(map[string]any{"providerAssertion": provAssert})
	})
	mux.HandleFunc("/accounts.tfa.finalizeTFA", func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		assert.Equal(t, "false", req.FormValue("tempDevice"), "device must be marked trusted")
		deviceTrusted.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{"errorCode": 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)

	email, err := r.StartTFA("user@example.com", "pw")
	require.NoError(t, err)
	assert.Equal(t, obfuscated, email)
	assert.True(t, st.GetBool("vehicle.renault_tfa_required", false))

	require.NoError(t, r.CompleteTFA(code))
	assert.Equal(t, loginToken, st.GetDefault("vehicle.renault_session", ""))
	assert.False(t, st.GetBool("vehicle.renault_tfa_required", true))
	assert.NotEmpty(t, st.GetDefault("vehicle.renault_tfa_completed_at", ""))
	assert.Equal(t, "GMID-1", st.GetDefault("vehicle.renault_gmid", ""))
	assert.Equal(t, "UCID-1", st.GetDefault("vehicle.renault_ucid", ""))

	r.mu.RLock()
	sess, pending := r.session, r.tfa
	r.mu.RUnlock()
	assert.Equal(t, loginToken, sess)
	assert.Nil(t, pending, "pending verification should be cleared after completion")
}

// TestStartTFASucceedsWithoutChallenge covers the trusted-device path: when
// accounts.login succeeds outright, StartTFA persists the session and reports
// ErrTFANotRequired instead of emailing a code.
func TestStartTFASucceedsWithoutChallenge(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts.webSdkBootstrap", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"gmid": "GMID-1", "ucid": "UCID-1"})
	})
	mux.HandleFunc("/accounts.login", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionInfo": map[string]any{"cookieValue": "SESSION-OK"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	_ = st.Set("vehicle.renault_tfa_required", "true") // simulate a stale required flag
	r := testRenault(t, st, srv.URL)

	email, err := r.StartTFA("user@example.com", "pw")
	require.ErrorIs(t, err, ErrTFANotRequired)
	assert.Empty(t, email)
	assert.Equal(t, "SESSION-OK", st.GetDefault("vehicle.renault_session", ""))
	assert.False(t, st.GetBool("vehicle.renault_tfa_required", true), "required flag should clear on success")
}

// TestCompleteTFAWithoutPending rejects a code when no verification is in flight.
func TestCompleteTFAWithoutPending(t *testing.T) {
	st := testStore(t)
	r := testRenault(t, st, "http://unused")

	err := r.CompleteTFA("123456")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no TFA verification in progress")
}

// TestSetSocTargetUsesCachedSocMin verifies the write reuses the cached socMin
// (no extra getSocLevel call) and preserves it in the POST body — the core API
// saving of the event-based refactor.
func TestSetSocTargetUsesCachedSocMin(t *testing.T) {
	var getHits int32
	var gotSocMin, gotSocTarget int

	mux := http.NewServeMux()
	// GET soc-level (singular). Must NOT be hit when the cache is warm.
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&getHits, 1)
			w.WriteHeader(http.StatusNotFound)
		})
	// POST soc-levels (plural) — the write endpoint.
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-levels",
		func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				SocMin    int `json:"socMin"`
				SocTarget int `json:"socTarget"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotSocMin, gotSocTarget = body.SocMin, body.SocTarget
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"type":"SOC_SYNCH"}`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	_ = st.Set("vehicle.renault_user", "u")
	_ = st.Set("vehicle.renault_password", "p")
	_ = st.Set("vehicle.vin", "TESTVIN")
	r := testRenault(t, st, srv.URL)
	r.setSocLevelCache(18, 70) // warm the cache as the soc-level tier would

	require.NoError(t, r.SetSocTarget(80))

	assert.Equal(t, int32(0), atomic.LoadInt32(&getHits), "SetSocTarget must not read soc-level when the cache is warm")
	assert.Equal(t, 18, gotSocMin, "POST must preserve the cached socMin")
	assert.Equal(t, 80, gotSocTarget)
	assert.Equal(t, 80, st.GetInt("vehicle.soc_target", 0))

	min, known := r.cachedSocMin()
	assert.True(t, known)
	assert.Equal(t, 18, min)
	r.mu.RLock()
	cachedTarget := r.socTarget
	r.mu.RUnlock()
	assert.Equal(t, 80, cachedTarget, "successful write must refresh the target cache")
}

// TestSetSocTargetRefusesWhenSocMinUnknown verifies that when the car minimum is
// unknown and the soc-level GET is rate-limited (429), the write is refused
// rather than POSTing a fabricated socMin=0 that would reset the car's floor.
func TestSetSocTargetRefusesWhenSocMinUnknown(t *testing.T) {
	var postHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-levels",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				atomic.AddInt32(&postHits, 1)
				w.WriteHeader(http.StatusOK)
				return
			}
			// GET (getSocLevel): rate-limited, so socMin stays unknown.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"messages":[{"code":"err.func.wired.overloaded"}]}`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	_ = st.Set("vehicle.renault_user", "u")
	_ = st.Set("vehicle.renault_password", "p")
	_ = st.Set("vehicle.vin", "TESTVIN")
	r := testRenault(t, st, srv.URL) // cache cold: socMin unknown, store empty

	err := r.SetSocTarget(80)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate-limited")
	assert.Equal(t, int32(0), atomic.LoadInt32(&postHits), "must not POST a fabricated socMin=0")
	assert.Equal(t, 0, st.GetInt("vehicle.soc_target", 0), "target must not persist on refusal")
}

// TestGetSocLevelRateLimited verifies a 429 is surfaced as errRenaultRateLimited
// so callers can distinguish quota exhaustion from other failures.
func TestGetSocLevelRateLimited(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"messages":[{"code":"err.func.wired.overloaded"}]}`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRenault(t, testStore(t), srv.URL)
	_, err := r.getSocLevel("TESTVIN")
	require.Error(t, err)
	assert.True(t, errors.Is(err, errRenaultRateLimited), "429 must map to errRenaultRateLimited")
}

// TestGetSocLevelTopLevelShape verifies we parse the real KCM response, which is
// a bare top-level object (NOT wrapped in data.attributes like KCA endpoints).
func TestGetSocLevelTopLevelShape(t *testing.T) {
	var singularHits int32
	mux := http.NewServeMux()
	// Current gateways serve the plural resource; singular 404s.
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-levels",
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"lastEnergyUpdateTimestamp":"2025-04-18T06:51:09Z","socMin":25,"socTarget":80}`))
		})
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&singularHits, 1); w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRenault(t, testStore(t), srv.URL)
	lvl, err := r.getSocLevel("TESTVIN")
	require.NoError(t, err)
	assert.Equal(t, 25, lvl.SocMin, "socMin must parse from the bare top-level body")
	assert.Equal(t, 80, lvl.SocTarget)
	assert.Equal(t, int32(0), atomic.LoadInt32(&singularHits), "plural endpoint must be used without falling back")
}

// TestGetSocLevelWrappedFallback verifies the fallback still handles a gateway
// that returns the data.attributes-wrapped shape.
func TestGetSocLevelWrappedFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"socMin": 30, "socTarget": 90}},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRenault(t, testStore(t), srv.URL)
	lvl, err := r.getSocLevel("TESTVIN")
	require.NoError(t, err)
	assert.Equal(t, 30, lvl.SocMin)
	assert.Equal(t, 90, lvl.SocTarget)
}

// TestSocLevelSkippedWhenKnownAndFresh verifies the redundancy fix: once the
// limits are known and recently read, a slow-tier poll does NOT re-hit the
// strict-quota soc-level endpoint.
func TestSocLevelSkippedWhenKnownAndFresh(t *testing.T) {
	var socLevelHits, batteryHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/battery-status",
		batteryStatusHandler(&batteryHits, 50, 0))
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&socLevelHits, 1)
			_, _ = w.Write([]byte(`{"socMin":25,"socTarget":80}`))
		})
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/cockpit",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"totalMileage": 10.0}},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)
	r.markDetailsFetched(time.Now())
	r.setSocLevelCache(25, 80)                                                     // known
	_ = st.Set("vehicle.soc_level_read_at", time.Now().UTC().Format(time.RFC3339)) // fresh

	var cache abrpCache
	_, err := r.pollOnce("u", "p", "TESTVIN", true, "test", &cache)
	require.NoError(t, err)
	assert.Equal(t, int32(0), atomic.LoadInt32(&socLevelHits), "soc-level must be skipped when known and fresh")
}

// batteryStatusHandler encodes a minimal battery-status response.
func batteryStatusHandler(hits *int32, level, plug int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"attributes": map[string]any{
				"batteryLevel": level,
				"plugStatus":   plug,
				"timestamp":    "2026-04-25T10:00:00Z",
			}},
		})
	}
}

// TestPollOnceTierASkipsSlowTier confirms a Tier-A-only poll fetches battery
// status but not the slow group (soc-level, cockpit).
func TestPollOnceTierASkipsSlowTier(t *testing.T) {
	var batteryHits, socLevelHits, cockpitHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/battery-status",
		batteryStatusHandler(&batteryHits, 55, 1))
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&socLevelHits, 1) })
	cockpit := func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&cockpitHits, 1) }
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/cockpit", cockpit)
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v1/cars/TESTVIN/cockpit", cockpit)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)
	r.markDetailsFetched(time.Now()) // skip the daily details (Tier C) fetch

	var cache abrpCache
	plugged, err := r.pollOnce("u", "p", "TESTVIN", false, "test", &cache)
	require.NoError(t, err)
	assert.True(t, plugged)
	assert.Equal(t, int32(1), atomic.LoadInt32(&batteryHits))
	assert.Equal(t, int32(0), atomic.LoadInt32(&socLevelHits), "Tier A must not fetch soc-level")
	assert.Equal(t, int32(0), atomic.LoadInt32(&cockpitHits), "Tier A must not fetch cockpit")
	assert.Equal(t, 55, st.GetInt("scheduler.current_soc", 0))
	assert.Equal(t, 1, st.GetInt("vehicle.plug_status", 0))
}

// TestPollOnceTierBFetchesAndCaches confirms a Tier-B poll fetches the slow group
// and populates the store + in-memory caches.
func TestPollOnceTierBFetchesAndCaches(t *testing.T) {
	var batteryHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/battery-status",
		batteryStatusHandler(&batteryHits, 40, 0))
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"socMin": 20, "socTarget": 75}},
			})
		})
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/cockpit",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"totalMileage": 12345.0}},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)
	r.markDetailsFetched(time.Now())

	var cache abrpCache
	plugged, err := r.pollOnce("u", "p", "TESTVIN", true, "test", &cache)
	require.NoError(t, err)
	assert.False(t, plugged)
	assert.Equal(t, 75, st.GetInt("vehicle.soc_target", 0))
	assert.Equal(t, 20, st.GetInt("vehicle.soc_min", 0))
	assert.InDelta(t, 12345.0, cache.odometerKm, 0.1)

	min, known := r.cachedSocMin()
	assert.True(t, known)
	assert.Equal(t, 20, min)
}

// TestSocLevelBackoffAfter429 verifies that a soc-level 429 persists a backoff
// and suppresses further soc-level calls until it expires — so we stop pinning
// the exhausted quota (and don't re-hit it on every restart).
func TestSocLevelBackoffAfter429(t *testing.T) {
	var socLevelHits, batteryHits int32

	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/battery-status",
		batteryStatusHandler(&batteryHits, 50, 0))
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kcm/v1/vehicles/TESTVIN/ev/soc-level",
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&socLevelHits, 1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"messages":[{"code":"err.func.wired.overloaded"}]}`))
		})
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v2/cars/TESTVIN/cockpit",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"totalMileage": 100.0}},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st := testStore(t)
	r := testRenault(t, st, srv.URL)
	r.markDetailsFetched(time.Now())

	var cache abrpCache
	// First Tier-B poll: soc-level 429 → backoff persisted.
	_, err := r.pollOnce("u", "p", "TESTVIN", true, "test", &cache)
	require.NoError(t, err) // battery ok; soc-level is best-effort
	assert.Equal(t, int32(1), atomic.LoadInt32(&socLevelHits))
	assert.NotEmpty(t, st.GetDefault("vehicle.soc_level_backoff_until", ""), "429 must persist a backoff")
	assert.True(t, r.socLevelBackedOff())

	// Second Tier-B poll while backed off: soc-level must be skipped.
	_, err = r.pollOnce("u", "p", "TESTVIN", true, "test", &cache)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&socLevelHits), "soc-level must be skipped during backoff")
}

// TestSchedulePlugPollCoalesces verifies a burst of socket events collapses to a
// single queued poll (the buffered-channel debounce), so repeated status
// notifications can't fan out into multiple Kamereon polls.
func TestSchedulePlugPollCoalesces(t *testing.T) {
	r := testRenault(t, testStore(t), "http://unused")
	for i := 0; i < 5; i++ {
		r.SchedulePlugPoll()
	}
	assert.Equal(t, 1, len(r.plugCh), "bursts should coalesce to a single queued poll")
	<-r.plugCh
	assert.Equal(t, 0, len(r.plugCh))
}
