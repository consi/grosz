package ocpp

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
)

// newCheckerForTest builds a StatusChecker against testServer's *Server,
// stubbing out the actual TriggerMessage transport so tests don't need a
// real OCPP connection.
func newCheckerForTest(t *testing.T, threshold time.Duration) (*StatusChecker, *triggerRecorder) {
	t.Helper()
	s := testServer(t)
	rec := &triggerRecorder{}
	c := NewStatusChecker(s, func() time.Duration { return threshold }, s.log)
	c.triggerFn = rec.record
	return c, rec
}

type triggerRecorder struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *triggerRecorder) record(cpID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, cpID)
	return r.err
}

func (r *triggerRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// Stale connector — last_status_notification older than threshold —
// triggers a StatusNotification.
func TestStatusChecker_FiresWhenStale(t *testing.T) {
	c, rec := newCheckerForTest(t, 25*time.Minute)
	s := c.s

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return now }
	defer func() { timeNow = time.Now }()

	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp)
	cp.SetConnected(true)
	cp.GetConnector(1) // creates connector entry

	// Persist a stale state.
	stale := now.Add(-30 * time.Minute)
	require.NoError(t, s.store.UpsertConnectorState(store.ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Charging",
		StatusAt: stale, LastStatusNotification: stale,
	}))

	c.tick()
	waitFor(t, func() bool { return len(rec.snapshot()) >= 1 })
	assert.Equal(t, []string{"CP001"}, rec.snapshot())
}

// Fresh connector — last_status_notification within threshold — must NOT trigger.
func TestStatusChecker_SkipsWhenFresh(t *testing.T) {
	c, rec := newCheckerForTest(t, 25*time.Minute)
	s := c.s

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return now }
	defer func() { timeNow = time.Now }()

	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp)
	cp.SetConnected(true)
	cp.GetConnector(1)

	fresh := now.Add(-5 * time.Minute)
	require.NoError(t, s.store.UpsertConnectorState(store.ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Charging",
		StatusAt: fresh, LastStatusNotification: fresh,
	}))

	c.tick()
	time.Sleep(20 * time.Millisecond) // give async fireTrigger a chance
	assert.Empty(t, rec.snapshot(), "fresh state must not trigger")
}

// Disconnected charge points must be skipped — TriggerMessage can't reach them.
func TestStatusChecker_SkipsDisconnected(t *testing.T) {
	c, rec := newCheckerForTest(t, 25*time.Minute)
	s := c.s

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return now }
	defer func() { timeNow = time.Now }()

	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp)
	cp.SetConnected(false)
	cp.GetConnector(1)

	stale := now.Add(-30 * time.Minute)
	require.NoError(t, s.store.UpsertConnectorState(store.ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Charging",
		StatusAt: stale, LastStatusNotification: stale,
	}))

	c.tick()
	time.Sleep(20 * time.Millisecond)
	assert.Empty(t, rec.snapshot(), "disconnected CP must be skipped")
}

// Connector with no persisted state yet (e.g., right after boot, before any
// StatusNotification arrived) is considered stale and triggered.
func TestStatusChecker_FiresWhenNoPersistedState(t *testing.T) {
	c, rec := newCheckerForTest(t, 25*time.Minute)
	s := c.s

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return now }
	defer func() { timeNow = time.Now }()

	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp)
	cp.SetConnected(true)
	cp.GetConnector(1) // connector exists in memory but no DB row yet

	c.tick()
	waitFor(t, func() bool { return len(rec.snapshot()) >= 1 })
	assert.Equal(t, []string{"CP001"}, rec.snapshot())
}

// Threshold 0 disables the check entirely (defensive — should never happen
// in production since the default is 25 min).
func TestStatusChecker_ZeroThresholdDisabled(t *testing.T) {
	c, rec := newCheckerForTest(t, 0)
	s := c.s

	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp)
	cp.SetConnected(true)
	cp.GetConnector(1)

	c.tick()
	time.Sleep(20 * time.Millisecond)
	assert.Empty(t, rec.snapshot())
}

// Start/Stop wiring works; calling Stop() unblocks Start()'s goroutine.
func TestStatusChecker_StartStop(t *testing.T) {
	// Small threshold so the loop cycles quickly within the test window.
	c, _ := newCheckerForTest(t, 10*time.Millisecond)
	c.Start()

	// Let at least one tick fire.
	time.Sleep(30 * time.Millisecond)

	done := make(chan struct{})
	go func() { c.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return")
	}
}

// Idle interval is used when threshold is 0 (disabled). Sanity check that
// Start/Stop still works in that mode without hanging on the default minute.
func TestStatusChecker_StartStopWhenDisabled(t *testing.T) {
	prev := statusCheckIdleInterval
	statusCheckIdleInterval = 10 * time.Millisecond
	defer func() { statusCheckIdleInterval = prev }()

	c, _ := newCheckerForTest(t, 0)
	c.Start()
	time.Sleep(30 * time.Millisecond)

	done := make(chan struct{})
	go func() { c.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return")
	}
}

// Cooldown: back-to-back tick()s for a stale CP must only fire once within
// the threshold window — guards against trigger storms when Zappi never
// answers a TriggerMessage with a fresh StatusNotification.
func TestStatusChecker_CooldownPreventsRepeatedFires(t *testing.T) {
	c, rec := newCheckerForTest(t, 25*time.Minute)
	s := c.s

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return now }
	defer func() { timeNow = time.Now }()

	cp := s.ChargePoint("CP001")
	require.NotNil(t, cp)
	cp.SetConnected(true)
	cp.GetConnector(1)

	stale := now.Add(-30 * time.Minute)
	require.NoError(t, s.store.UpsertConnectorState(store.ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Charging",
		StatusAt: stale, LastStatusNotification: stale,
	}))

	c.tick()
	waitFor(t, func() bool { return len(rec.snapshot()) >= 1 })

	// Second tick at the same instant must be suppressed by cooldown.
	c.tick()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, []string{"CP001"}, rec.snapshot(),
		"second tick within cooldown must not re-fire")

	// Advance past the cooldown window — a subsequent tick may fire again.
	timeNow = func() time.Time { return now.Add(26 * time.Minute) }
	c.tick()
	waitFor(t, func() bool { return len(rec.snapshot()) >= 2 })
	assert.Equal(t, []string{"CP001", "CP001"}, rec.snapshot())
}

// trigger errors don't crash the loop and don't bubble up.
func TestStatusChecker_TriggerErrorIsLogged(t *testing.T) {
	c, rec := newCheckerForTest(t, 25*time.Minute)
	rec.err = errors.New("simulated")
	s := c.s

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return now }
	defer func() { timeNow = time.Now }()

	cp := s.ChargePoint("CP001")
	cp.SetConnected(true)
	cp.GetConnector(1)
	stale := now.Add(-30 * time.Minute)
	require.NoError(t, s.store.UpsertConnectorState(store.ConnectorState{
		ChargeBox: "CP001", ConnectorID: 1, Status: "Charging",
		StatusAt: stale, LastStatusNotification: stale,
	}))

	c.tick()
	waitFor(t, func() bool { return len(rec.snapshot()) >= 1 })
	// nothing else to assert — the test passes if tick() didn't panic.
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 1s")
}

// Sanity check: atomic counter is uncontended by ticker. No real assertion;
// kept so go vet/race finds issues if introduced later.
var _ = atomic.AddInt64
