package meter

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/store"
)

func newTestPoller(t *testing.T) *Poller {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return &Poller{
		store:  st,
		events: events.For(events.SourceMeter, st),
		log:    slog.Default().With("component", "meter"),
	}
}

func TestAdjustEnergy_FirstReadingNoData(t *testing.T) {
	p := newTestPoller(t)
	p.initEnergyState()
	got := p.adjustEnergy(500)
	assert.Equal(t, 500.0, got)
	assert.Equal(t, 0.0, p.energyOffset)
}

func TestAdjustEnergy_NormalIncrease(t *testing.T) {
	p := newTestPoller(t)
	p.initEnergyState()
	assert.Equal(t, 100.0, p.adjustEnergy(100))
	assert.Equal(t, 200.0, p.adjustEnergy(200))
	assert.Equal(t, 5000.0, p.adjustEnergy(5000))
	assert.Equal(t, 0.0, p.energyOffset, "offset should remain 0 on monotonic data")
}

func TestAdjustEnergy_ResetTriggersOffsetBump(t *testing.T) {
	p := newTestPoller(t)
	p.initEnergyState()
	// Build up a high effective value
	p.adjustEnergy(100)
	p.adjustEnergy(116298)
	// Meter resets back to a small value
	got := p.adjustEnergy(50)
	assert.Equal(t, 116298.0, got, "after reset, effective should equal previous lastEffective")
	assert.Equal(t, 116248.0, p.energyOffset, "offset = previous(116298) - rawAfterReset(50)")

	// Subsequent readings continue from reset point with offset applied
	got = p.adjustEnergy(200)
	assert.Equal(t, 116448.0, got)

	// Offset should be persisted
	stored := p.store.GetFloat(meterEnergyOffsetSetting, -1)
	assert.Equal(t, 116248.0, stored)

	// A reset event was recorded
	events, err := p.store.SystemEventsBySource("meter", 10, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Equal(t, "meterResetDetected", events[0].Action)
}

func TestAdjustEnergy_SmallNoiseClampedNotReset(t *testing.T) {
	p := newTestPoller(t)
	p.initEnergyState()
	p.adjustEnergy(5000)
	// Drop by 200 Wh (below 1000 Wh threshold) — clamp, no offset change
	got := p.adjustEnergy(4800)
	assert.Equal(t, 5000.0, got, "small drop should clamp to previous effective")
	assert.Equal(t, 0.0, p.energyOffset, "offset must NOT change for sub-threshold drops")
}

func TestAdjustEnergy_LoadsPersistedOffsetOnInit(t *testing.T) {
	p := newTestPoller(t)
	require.NoError(t, p.store.Set(meterEnergyOffsetSetting, "5000"))
	require.NoError(t, p.store.InsertMeterReading(store.MeterReading{
		EnergyWh: 6000, // some prior effective value
	}))

	p.initEnergyState()
	assert.Equal(t, 5000.0, p.energyOffset)
	assert.Equal(t, 6000.0, p.lastEffective)

	// First poll after restart: raw=1100 (i.e., real meter at 1100 — was 1000 before),
	// effective should = 1100+5000 = 6100, monotonic continues.
	got := p.adjustEnergy(1100)
	assert.Equal(t, 6100.0, got)
}

func TestAdjustEnergy_ResetWhilePollerWasOffline(t *testing.T) {
	// Server reboot scenario: lastEffective in DB was high, settings have an offset,
	// but the meter ALSO reset. The first poll after restart returns a low raw value,
	// and effective < lastEffective by more than threshold → reset detected.
	p := newTestPoller(t)
	require.NoError(t, p.store.Set(meterEnergyOffsetSetting, "5000"))
	require.NoError(t, p.store.InsertMeterReading(store.MeterReading{EnergyWh: 116298}))

	p.initEnergyState()
	assert.Equal(t, 5000.0, p.energyOffset)
	assert.Equal(t, 116298.0, p.lastEffective)

	// Meter reset, current raw is 50 → effective = 50+5000 = 5050 < 116298-1000
	got := p.adjustEnergy(50)
	assert.Equal(t, 116298.0, got)
	// Offset was bumped by (116298 - 5050) = 111248 → new offset = 5000+111248 = 116248
	assert.Equal(t, 116248.0, p.energyOffset)
}
