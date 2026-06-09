package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// InsertChartMarker takes an explicit timestamp so a stop marker can match the
// actual stop time (OCPP timestamp / status_at), not the moment we got around
// to inserting. The timestamp must be recent enough to fall within the
// RecentChartMarkers window (48h here) — truncated to second precision so the
// RFC3339 round-trip is exact.
func TestInsertChartMarker_UsesGivenTimestamp(t *testing.T) {
	s := testStore(t)

	at := time.Now().Add(-time.Hour).Truncate(time.Second).UTC()
	require.NoError(t, s.InsertChartMarker("stop", at))

	markers, err := s.RecentChartMarkers(48)
	require.NoError(t, err)
	require.Len(t, markers, 1)
	assert.Equal(t, "stop", markers[0].Type)
	assert.True(t, markers[0].Timestamp.Equal(at), "stored ts must equal input")
}

// Pstryk never reports the in-progress hour, so HourlyConsumption must fill
// the current hour from local meter readings (cumulative register delta).
func TestHourlyConsumption_CurrentHourFromMeter(t *testing.T) {
	s := testStore(t)
	currentHour := time.Now().UTC().Truncate(time.Hour)

	require.NoError(t, s.UpsertPstrykConsumption([]PstrykConsumption{
		{Hour: currentHour.Add(-2 * time.Hour), EnergyWh: 1000},
		{Hour: currentHour.Add(-time.Hour), EnergyWh: 1200},
	}))
	require.NoError(t, s.InsertMeterReading(MeterReading{
		Timestamp: currentHour.Add(5 * time.Minute), PowerW: 400, EnergyWh: 50000,
	}))
	require.NoError(t, s.InsertMeterReading(MeterReading{
		Timestamp: currentHour.Add(25 * time.Minute), PowerW: 600, EnergyWh: 50250,
	}))

	rows, err := s.HourlyConsumption(48)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	last := rows[len(rows)-1]
	assert.True(t, last.Hour.Equal(currentHour))
	assert.InDelta(t, 250.0, last.EnergyWh, 0.01) // 50250 − 50000
	assert.InDelta(t, 500.0, last.PowerW, 0.01)   // avg(400, 600)
}

// When Pstryk DOES have a row for the current hour (e.g., the API starts
// reporting live frames), the Pstryk value wins over the meter derivation.
func TestHourlyConsumption_PstrykCurrentHourWins(t *testing.T) {
	s := testStore(t)
	currentHour := time.Now().UTC().Truncate(time.Hour)

	require.NoError(t, s.UpsertPstrykConsumption([]PstrykConsumption{
		{Hour: currentHour, EnergyWh: 777},
	}))
	require.NoError(t, s.InsertMeterReading(MeterReading{
		Timestamp: currentHour.Add(5 * time.Minute), PowerW: 400, EnergyWh: 50000,
	}))
	require.NoError(t, s.InsertMeterReading(MeterReading{
		Timestamp: currentHour.Add(25 * time.Minute), PowerW: 600, EnergyWh: 50250,
	}))

	rows, err := s.HourlyConsumption(48)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 777.0, rows[0].EnergyWh, 0.01)
}

func TestInsertChartMarker_PreservesType(t *testing.T) {
	s := testStore(t)

	now := time.Now()
	for _, typ := range []string{"start", "stop", "plug", "unplug"} {
		require.NoError(t, s.InsertChartMarker(typ, now))
	}
	markers, err := s.RecentChartMarkers(1)
	require.NoError(t, err)
	require.Len(t, markers, 4)
	got := map[string]bool{}
	for _, m := range markers {
		got[m.Type] = true
	}
	for _, typ := range []string{"start", "stop", "plug", "unplug"} {
		assert.True(t, got[typ], "marker type %s missing", typ)
	}
}

