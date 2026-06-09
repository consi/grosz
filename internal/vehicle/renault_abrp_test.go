package vehicle

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/consi/grosz/internal/abrp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fptr(v float64) *float64 { return &v }

func TestBuildABRPTelemetryPowerWatts(t *testing.T) {
	status := &BatteryStatus{
		Level: 60, Autonomy: 200, PlugStatus: 1, ChargingStatus: 1.0,
		ChargingInstantaneousPower: fptr(7400), // Watts
	}
	tlm := buildABRPTelemetry(status, nil, nil, 52, 12345)

	assert.EqualValues(t, 60, tlm.SoC)
	assert.Equal(t, 1, tlm.IsCharging)
	require.NotNil(t, tlm.IsParked)
	assert.Equal(t, 1, *tlm.IsParked)
	require.NotNil(t, tlm.Power)
	assert.InDelta(t, -7.4, *tlm.Power, 0.001) // 7400 W -> 7.4 kW, negated for charging
	require.NotNil(t, tlm.EstRange)
	assert.InDelta(t, 200, *tlm.EstRange, 0.001)
	require.NotNil(t, tlm.Capacity)
	assert.InDelta(t, 52, *tlm.Capacity, 0.001)
	require.NotNil(t, tlm.Odometer)
	assert.InDelta(t, 12345, *tlm.Odometer, 0.001)
	assert.Nil(t, tlm.Lat)
	assert.Nil(t, tlm.Lon)
}

func TestBuildABRPTelemetryPowerKw(t *testing.T) {
	status := &BatteryStatus{
		Level: 60, ChargingStatus: 1.0,
		ChargingInstantaneousPower: fptr(7.4), // already kW
	}
	tlm := buildABRPTelemetry(status, nil, nil, 0, 0)

	require.NotNil(t, tlm.Power)
	assert.InDelta(t, -7.4, *tlm.Power, 0.001)
	assert.Nil(t, tlm.Capacity, "capacity 0 -> omitted")
	assert.Nil(t, tlm.Odometer, "odometer 0 -> omitted")
}

func TestBuildABRPTelemetryNotCharging(t *testing.T) {
	status := &BatteryStatus{
		Level: 90, PlugStatus: 0, ChargingStatus: 0.0,
		ChargingInstantaneousPower: fptr(7400),
	}
	tlm := buildABRPTelemetry(status, nil, nil, 0, 0)

	assert.Equal(t, 0, tlm.IsCharging)
	assert.Nil(t, tlm.Power, "power omitted when not charging")
	assert.Nil(t, tlm.IsParked, "unplugged + not charging -> parked omitted")
}

func TestBuildABRPTelemetryParkedWhenPlugged(t *testing.T) {
	status := &BatteryStatus{Level: 90, PlugStatus: 1, ChargingStatus: 0.2} // plugged, charge complete
	tlm := buildABRPTelemetry(status, nil, nil, 0, 0)

	require.NotNil(t, tlm.IsParked)
	assert.Equal(t, 1, *tlm.IsParked)
	assert.Equal(t, 0, tlm.IsCharging)
}

func TestBuildABRPTelemetryGPSAndTemps(t *testing.T) {
	status := &BatteryStatus{Level: 50, BatteryTemperature: fptr(18.5)}
	hvac := &HvacStatus{ExternalTemperature: fptr(9.0)}
	loc := &LocationStatus{GPSLatitude: 52.1, GPSLongitude: 21.0}
	tlm := buildABRPTelemetry(status, hvac, loc, 0, 0)

	require.NotNil(t, tlm.BattTemp)
	assert.InDelta(t, 18.5, *tlm.BattTemp, 0.001)
	require.NotNil(t, tlm.ExtTemp)
	assert.InDelta(t, 9.0, *tlm.ExtTemp, 0.001)
	require.NotNil(t, tlm.Lat)
	assert.InDelta(t, 52.1, *tlm.Lat, 0.001)
	require.NotNil(t, tlm.Lon)
	assert.InDelta(t, 21.0, *tlm.Lon, 0.001)
}

func TestBuildABRPTelemetryZeroGPSOmitted(t *testing.T) {
	status := &BatteryStatus{Level: 50}
	loc := &LocationStatus{GPSLatitude: 0, GPSLongitude: 0}
	tlm := buildABRPTelemetry(status, nil, loc, 0, 0)

	assert.Nil(t, tlm.Lat)
	assert.Nil(t, tlm.Lon)
}

func TestGetHvacStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v1/cars/TESTVIN/hvac-status",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"externalTemperature": 11.5}},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRenault(t, testStore(t), srv.URL)
	hvac, err := r.getHvacStatus("testvin")
	require.NoError(t, err)
	require.NotNil(t, hvac.ExternalTemperature)
	assert.InDelta(t, 11.5, *hvac.ExternalTemperature, 0.001)
}

func TestGetLocation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v1/cars/TESTVIN/location",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"gpsLatitude": 52.23, "gpsLongitude": 21.01}},
			})
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRenault(t, testStore(t), srv.URL)
	loc, err := r.getLocation("testvin")
	require.NoError(t, err)
	assert.InDelta(t, 52.23, loc.GPSLatitude, 0.001)
	assert.InDelta(t, 21.01, loc.GPSLongitude, 0.001)
}

func TestGetLocationBestEffort403(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v1/cars/TESTVIN/location",
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := testRenault(t, testStore(t), srv.URL)
	_, err := r.getLocation("testvin")
	require.Error(t, err) // caller (pushABRP) treats this as best-effort: logs and continues
}

// TestPushABRPEndToEnd exercises the full chain: best-effort hvac/location
// fetches, capacity from settings, telemetry assembly, and the ABRP send.
func TestPushABRPEndToEnd(t *testing.T) {
	var gotTLM string
	abrpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotTLM = r.PostFormValue("tlm")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer abrpSrv.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v1/cars/TESTVIN/hvac-status",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"externalTemperature": 8.0}},
			})
		})
	mux.HandleFunc("/commerce/v1/accounts/test-account/kamereon/kca/car-adapter/v1/cars/TESTVIN/location",
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"attributes": map[string]any{"gpsLatitude": 52.0, "gpsLongitude": 21.0}},
			})
		})
	renSrv := httptest.NewServer(mux)
	defer renSrv.Close()

	st := testStore(t)
	require.NoError(t, st.Set("abrp.token", "tok"))
	require.NoError(t, st.Set("scheduler.battery_capacity", "52"))
	r := testRenault(t, st, renSrv.URL)
	ac := abrp.NewWithURL(st, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})), abrpSrv.URL)

	status := &BatteryStatus{
		Level: 65, Autonomy: 210, PlugStatus: 1, ChargingStatus: 1.0,
		ChargingInstantaneousPower: fptr(11000), BatteryTemperature: fptr(20),
	}
	r.pushABRP(ac, "testvin", status, 30000)

	require.NotEmpty(t, gotTLM, "ABRP server should have received a tlm payload")
	var tlm map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotTLM), &tlm))
	assert.EqualValues(t, 65, tlm["soc"])
	assert.EqualValues(t, 1, tlm["is_charging"])
	assert.InDelta(t, -11.0, tlm["power"], 0.001) // 11000 W -> 11 kW negated
	assert.InDelta(t, 52, tlm["capacity"], 0.001)
	assert.InDelta(t, 30000, tlm["odometer"], 0.001)
	assert.InDelta(t, 8.0, tlm["ext_temp"], 0.001)
	assert.InDelta(t, 20, tlm["batt_temp"], 0.001)
	assert.InDelta(t, 52.0, tlm["lat"], 0.001)
	assert.InDelta(t, 21.0, tlm["lon"], 0.001)
}
