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
