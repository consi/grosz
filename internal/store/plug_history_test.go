package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlugPattern(t *testing.T) {
	s := testStore(t)
	loc := time.UTC
	// Plugged every night 20:00-06:00 for 14 days.
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	for d := 0; d < 14; d++ {
		day := base.AddDate(0, 0, d)
		require.NoError(t, s.RecordPlugTransition(day.Add(6*time.Hour), false))
		require.NoError(t, s.RecordPlugTransition(day.Add(20*time.Hour), true))
	}
	pat, days, err := s.PlugPattern(base, base.AddDate(0, 0, 14), loc)
	require.NoError(t, err)
	assert.InDelta(t, 13.75, days, 0.01)
	sunday := int(time.Sunday)
	assert.InDelta(t, 1.0, pat[[2]int{sunday, 23}], 0.01)
	assert.InDelta(t, 0.0, pat[[2]int{sunday, 12}], 0.01)
}
