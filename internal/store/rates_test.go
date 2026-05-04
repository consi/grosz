package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveAndLoadRates(t *testing.T) {
	s := testStore(t)

	now := time.Now().Truncate(time.Hour)
	rates := []Rate{
		{Start: now, End: now.Add(1 * time.Hour), Price: 0.45},
		{Start: now.Add(1 * time.Hour), End: now.Add(2 * time.Hour), Price: 0.30},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: 0.55},
	}

	require.NoError(t, s.SaveRates("pstryk", rates))

	loaded, err := s.LoadRates("pstryk")
	require.NoError(t, err)
	assert.Len(t, loaded, 3)
	assert.InDelta(t, 0.45, loaded[0].Price, 0.001)
	assert.InDelta(t, 0.30, loaded[1].Price, 0.001)
}

func TestSaveRatesUpserts(t *testing.T) {
	s := testStore(t)

	now := time.Now().Truncate(time.Hour)
	rates := []Rate{
		{Start: now, End: now.Add(1 * time.Hour), Price: 0.45},
	}

	require.NoError(t, s.SaveRates("pstryk", rates))

	// Update same time slot with new price
	rates[0].Price = 0.99
	require.NoError(t, s.SaveRates("pstryk", rates))

	loaded, err := s.LoadRates("pstryk")
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.InDelta(t, 0.99, loaded[0].Price, 0.001)
}

func TestLoadRatesFiltersPast(t *testing.T) {
	s := testStore(t)

	past := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	future := time.Now().Add(1 * time.Hour).Truncate(time.Hour)

	rates := []Rate{
		{Start: past, End: past.Add(1 * time.Hour), Price: 0.10},       // ended in the past
		{Start: future, End: future.Add(1 * time.Hour), Price: 0.50},   // future
	}

	require.NoError(t, s.SaveRates("pstryk", rates))

	loaded, err := s.LoadRates("pstryk")
	require.NoError(t, err)
	assert.Len(t, loaded, 1)
	assert.InDelta(t, 0.50, loaded[0].Price, 0.001)
}

func TestLoadRatesDifferentProviders(t *testing.T) {
	s := testStore(t)

	now := time.Now().Truncate(time.Hour)
	require.NoError(t, s.SaveRates("pstryk", []Rate{{Start: now, End: now.Add(time.Hour), Price: 0.45}}))
	require.NoError(t, s.SaveRates("fixed", []Rate{{Start: now, End: now.Add(time.Hour), Price: 0.50}}))

	pstryk, err := s.LoadRates("pstryk")
	require.NoError(t, err)
	assert.Len(t, pstryk, 1)
	assert.InDelta(t, 0.45, pstryk[0].Price, 0.001)
}

func TestPurgeOldRates(t *testing.T) {
	s := testStore(t)

	past := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	require.NoError(t, s.SaveRates("pstryk", []Rate{
		{Start: past, End: past.Add(time.Hour), Price: 0.10},
	}))

	require.NoError(t, s.PurgeOldRates(time.Now()))

	// Even loading all (including past) should return nothing
	var count int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM tariff_rates").Scan(&count))
	assert.Equal(t, 0, count)
}
