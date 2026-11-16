package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

// --- mocks ---

type mockCharger struct {
	mu        sync.Mutex
	status    string
	txnID     int
	connected bool
	starts    int
	stops     int
	clears    int
	profiles  int
}

func (m *mockCharger) ConnectorStatus(cpID string, connectorID int) (string, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, m.txnID, m.connected
}

func (m *mockCharger) RemoteStartTransaction(cpID, idTag string, connectorID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.starts++
	return nil
}

func (m *mockCharger) RemoteStopTransaction(cpID string, txnID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
	return nil
}

func (m *mockCharger) ClearChargingProfile(cpID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clears++
	return nil
}

func (m *mockCharger) SetChargingProfile(cpID string, connectorID int, profile *types.ChargingProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.profiles++
	return nil
}

func (m *mockCharger) setStatus(status string, txnID int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = status
	m.txnID = txnID
}

type mockTariff struct {
	rates []tariff.Rate
}

func (m *mockTariff) Rates() ([]tariff.Rate, error) { return m.rates, nil }
func (m *mockTariff) Name() string                  { return "mock" }
func (m *mockTariff) Stop()                         {}

// --- helpers ---

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.New(dbPath, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestScheduler(t *testing.T, mock *mockCharger) (*Scheduler, *store.Store) {
	t.Helper()
	st := testStore(t)
	_ = st.Set("zappi.charge_box_id", "CP001")
	_ = st.Set("zappi.id_tag", "grosz")
	// Planner defaults for tests: mid SoC, cheap fill off unless a test opts in.
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.current_soc", "60")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.safe_soc", "50")
	_ = st.Set("scheduler.min_soc", "15")
	_ = st.Set("scheduler.cheap_fill_price", "0")
	_ = st.Set("scheduler.departure_time", "07:00")

	s := &Scheduler{
		charger:  mock,
		store:    st,
		events:   events.For(events.SourceScheduler, st),
		log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		notifyCh: make(chan struct{}, 1),
	}
	return s, st
}

// --- Off Mode ---

func TestControlCharging_Off_StopsActiveTransaction(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	s.controlCharging()

	assert.Equal(t, 1, mock.stops)
	assert.Equal(t, 0, mock.starts)
}

func TestControlCharging_Off_DoesNotStartWhenPlugged(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	s.controlCharging()

	assert.Equal(t, 0, mock.starts)
	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Off_NoOpWhenUnplugged(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Available", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	s.controlCharging()

	assert.Equal(t, 0, mock.starts)
	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Off_NoOpWhenAlreadyStopped(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	s.controlCharging()

	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Off_StopsDespiteActiveSchedule(t *testing.T) {
	now := time.Now()
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	// Set up a schedule that says "charge now"
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 1, mock.stops, "off mode must stop even with active schedule")
	assert.Equal(t, 0, mock.starts)
}

func TestControlCharging_Off_NoOpWhenDisconnected(t *testing.T) {
	mock := &mockCharger{connected: false, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	s.controlCharging()

	assert.Equal(t, 0, mock.stops)
	assert.Equal(t, 0, mock.starts)
}

// --- Force Mode ---

func TestControlCharging_Force_StartsWhenPreparing(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 1, mock.starts)
	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Force_NoStartWhenSuspendedEV(t *testing.T) {
	mock := &mockCharger{connected: true, status: "SuspendedEV", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 0, mock.starts, "car won't accept power in SuspendedEV")
}

func TestControlCharging_Force_StartsWhenSuspendedEVSE(t *testing.T) {
	mock := &mockCharger{connected: true, status: "SuspendedEVSE", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 1, mock.starts)
}

func TestControlCharging_Force_NoStartWhenAlreadyCharging(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 1}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 0, mock.starts)
}

func TestControlCharging_Force_NoStartWhenUnplugged(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Available", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 0, mock.starts)
}

func TestControlCharging_Force_NoOpWhenDisconnected(t *testing.T) {
	mock := &mockCharger{connected: false, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 0, mock.starts)
	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Force_PlugUnplugReplug(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	// Car plugged in → starts
	s.controlCharging()
	assert.Equal(t, 1, mock.starts)

	// Simulate charger accepted start, now charging
	mock.setStatus("Charging", 1)
	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "should not duplicate start")

	// Car unplugged
	mock.setStatus("Available", 0)
	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "should not start when unplugged")

	// Car plugged back in
	mock.setStatus("Preparing", 0)
	s.controlCharging()
	assert.Equal(t, 2, mock.starts, "should start again after replug")
}

// --- Schedule Mode ---

func TestControlCharging_Schedule_StartsInWindow(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
				Price: 0.30,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 1, mock.starts)
	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Schedule_StopsOutsideWindow(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	// Schedule window is in the future
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(1 * time.Hour),
				End:   now.Add(2 * time.Hour),
				Power: 11000,
				Price: 0.30,
			}},
		}},
	}

	s.controlCharging()

	// A planned block still lies ahead: pause via the 0 W profile period
	// instead of ending the transaction (Zappi would park in Finishing).
	assert.Equal(t, 0, mock.stops)
	assert.Equal(t, 1, mock.profiles)
	assert.Equal(t, 0, mock.starts)
}

func TestControlCharging_Schedule_NoStartOutsideWindow(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(1 * time.Hour),
				End:   now.Add(2 * time.Hour),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 0, mock.starts)
	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Schedule_NoActionWhenUnplugged(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Available", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 0, mock.starts, "should not start when car unplugged")
	assert.Equal(t, 0, mock.stops)
}

func TestControlCharging_Schedule_NoScheduleNoStart(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = nil

	s.controlCharging()

	assert.Equal(t, 0, mock.starts)
}

// Nothing planned but the car is not full: keep the transaction open and
// hold it at 0 W (stopping would park Zappi in Finishing until re-plug).
func TestControlCharging_Schedule_HoldsWhenNothingPlannedAndNotFull(t *testing.T) {
	mock := &mockCharger{connected: true, status: "SuspendedEVSE", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = nil

	s.controlCharging()

	assert.Equal(t, 0, mock.stops, "must not stop a plugged-in car below target")
	assert.Equal(t, 1, mock.profiles, "hold profile written")
	assert.True(t, s.profileInSync(nil))
}

func TestControlCharging_Schedule_StopsWhenTargetReached(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.current_soc", "80")
	s.current = nil

	s.controlCharging()

	assert.Equal(t, 1, mock.stops)
}

func TestControlCharging_Schedule_WindowTransition(t *testing.T) {
	origTimeNow := timeNow
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	windowStart := time.Date(2026, 4, 23, 2, 0, 0, 0, time.Local)
	windowEnd := time.Date(2026, 4, 23, 4, 0, 0, 0, time.Local)

	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: "2026-04-23",
			Periods: []SchedulePeriod{{
				Start: windowStart,
				End:   windowEnd,
				Power: 11000,
				Price: 0.30,
			}},
		}},
	}

	// Before window, no start
	timeNow = func() time.Time { return windowStart.Add(-1 * time.Hour) }
	s.controlCharging()
	assert.Equal(t, 0, mock.starts, "before window: should not start")

	// Window opens: starts
	timeNow = func() time.Time { return windowStart.Add(5 * time.Minute) }
	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "window open: should start")

	// Simulate transaction active
	mock.setStatus("Charging", 1)

	// Still in window, no duplicate start
	timeNow = func() time.Time { return windowStart.Add(1 * time.Hour) }
	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "still in window, no duplicate")
	assert.Equal(t, 0, mock.stops, "still in window, no stop")

	// Window closes below target: pause (hold), not stop.
	timeNow = func() time.Time { return windowEnd.Add(5 * time.Minute) }
	mock.setStatus("SuspendedEVSE", 1)
	s.controlCharging()
	assert.Equal(t, 0, mock.stops, "window closed below target: keep open")

	// Target reached: now the session ends.
	_ = st.Set("scheduler.current_soc", "80")
	s.controlCharging()
	assert.Equal(t, 1, mock.stops, "target reached: stop")
}

// --- Schedule Mode: SoC scenarios ---

func TestRecompute_SoCReached_NoSchedule(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.current_soc", "85")

	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{{Start: now, End: now.Add(time.Hour), Price: 0.30}}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	assert.Nil(t, s.current, "target reached and only positive prices, nothing to plan")
	assert.Equal(t, "target_reached", s.skipReasonKey)
}

func TestRecompute_SoCAboveSkip_NegativePricesStillSchedule(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.current_soc", "75")
	_ = st.Set("scheduler.departure_time", "07:00")
	_ = st.Set("charger.max_power", "11000")

	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.50},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: -0.20},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: -0.10},
	}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	require.NotNil(t, s.current, "should schedule for negative prices even above skipAboveSoC")
	assert.Len(t, s.current.Slots, 1)
	for _, p := range s.current.Slots[0].Periods {
		assert.True(t, p.Price < 0, "only negative-price periods should be selected")
	}
}

// In-progress preservation: a recompute fired mid-charge must not cancel the
// active slot when conditions (SoC, target reached, no affordable rates) would
// otherwise tell us "skip charging". Charging caused the SoC rise; cancelling
// the slot retroactively would issue a stop mid-session.

// --- Mode Transition Scenarios ---

func TestControlCharging_ForceToOff(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 1}
	s, st := newTestScheduler(t, mock)

	// Start in force mode, car is charging
	_ = st.Set("charger.mode", "force")
	s.controlCharging()
	assert.Equal(t, 0, mock.starts, "already has transaction in force mode")
	assert.Equal(t, 0, mock.stops)

	// Switch to off
	_ = st.Set("charger.mode", "off")
	s.controlCharging()
	assert.Equal(t, 1, mock.stops, "off mode must stop the transaction")
	assert.Equal(t, 0, mock.starts)
}

func TestControlCharging_OffToForce(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)

	// Start in off mode
	_ = st.Set("charger.mode", "off")
	s.controlCharging()
	assert.Equal(t, 0, mock.starts)
	assert.Equal(t, 0, mock.stops)

	// Switch to force
	_ = st.Set("charger.mode", "force")
	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "force mode must start when plugged")
}

func TestControlCharging_ForceToSchedule_OutsideWindow(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 1}
	s, st := newTestScheduler(t, mock)

	// Force mode with active transaction
	_ = st.Set("charger.mode", "force")
	s.controlCharging()
	assert.Equal(t, 0, mock.stops)

	// Switch to schedule mode, no current window
	_ = st.Set("charger.mode", "schedule")
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(2 * time.Hour),
				End:   now.Add(3 * time.Hour),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()
	assert.Equal(t, 0, mock.stops, "future block ahead: pause via profile, don't stop")
	assert.Equal(t, 1, mock.profiles)
}

func TestControlCharging_ForceToSchedule_InsideWindow(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 1}
	s, st := newTestScheduler(t, mock)

	// Force mode with active transaction
	_ = st.Set("charger.mode", "force")
	s.controlCharging()

	// Switch to schedule mode, inside window
	_ = st.Set("charger.mode", "schedule")
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()
	assert.Equal(t, 0, mock.stops, "should keep charging when inside schedule window")
	assert.Equal(t, 0, mock.starts, "should not duplicate start")
}

// --- Edge Cases ---

func TestControlCharging_NoCpID(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Delete("zappi.charge_box_id")

	s.controlCharging()

	assert.Equal(t, 0, mock.starts, "should not act without charge_box_id")
}

func TestControlCharging_Off_RepeatedCalls_OnlyOnce(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	// First call stops
	s.controlCharging()
	assert.Equal(t, 1, mock.stops)

	// Simulate transaction stopped
	mock.setStatus("Available", 0)

	// Second call should be no-op
	s.controlCharging()
	assert.Equal(t, 1, mock.stops, "should not issue redundant stop")
}

func TestControlCharging_Schedule_CancelledSlotDoesNotCharge(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date:      now.Format("2006-01-02"),
			Cancelled: true,
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 0, mock.starts, "cancelled slot should not trigger charge")
}

// --- Vehicle Plug Check ---

func TestControlCharging_PlugCheck_Force_BlocksWhenUnplugged(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "0")
	_ = st.Set("vehicle.battery_timestamp", time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339))

	s.controlCharging()

	assert.Equal(t, 0, mock.starts, "should block start when Renault reports unplugged with fresh data")
}

func TestControlCharging_PlugCheck_Force_AllowsWhenPlugged(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "1")

	s.controlCharging()

	assert.Equal(t, 1, mock.starts, "should allow start when Renault reports plugged")
}

func TestControlCharging_PlugCheck_Force_AllowsWhenUnknown(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "")

	s.controlCharging()

	assert.Equal(t, 1, mock.starts, "should allow start when plug status unknown (fail-open)")
}

func TestControlCharging_PlugCheck_Disabled_AllowsWhenUnplugged(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "false")
	_ = st.Set("vehicle.plug_status", "0")

	s.controlCharging()

	assert.Equal(t, 1, mock.starts, "should allow start when plug check disabled")
}

func TestControlCharging_PlugCheck_Schedule_BlocksWhenUnplugged(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "0")
	_ = st.Set("vehicle.battery_timestamp", now.Add(-1*time.Minute).UTC().Format(time.RFC3339))

	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 0, mock.starts, "should block scheduled start when Renault reports unplugged")
}

func TestControlCharging_PlugCheck_Schedule_AllowsWhenPlugged(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "1")

	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 1, mock.starts, "should allow scheduled start when Renault reports plugged")
}

func TestControlCharging_PlugCheck_DoesNotBlockStop(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "0")
	_ = st.Set("vehicle.battery_timestamp", time.Now().Add(-1*time.Minute).UTC().Format(time.RFC3339))

	s.controlCharging()

	assert.Equal(t, 1, mock.stops, "plug check must not block stop operations")
	assert.Equal(t, 0, mock.starts)
}

// Stale Renault data must not block charging. Reflects the production race:
// Zappi reports Preparing immediately on plug-in, but Renault polls every 15
// minutes: between polls, plug_status="0" is leftover from before plug-in.
func TestControlCharging_PlugCheck_AllowsWhenStale(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "0")
	_ = st.Set("vehicle.poll_interval", "15")
	// Older than 2 × 15min = 30min cap.
	_ = st.Set("vehicle.battery_timestamp", now.Add(-45*time.Minute).UTC().Format(time.RFC3339))

	s.controlCharging()

	assert.Equal(t, 1, mock.starts, "stale Renault data must not block start")
}

func TestControlCharging_PlugCheck_AllowsWhenTimestampMissing(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "0")
	// No battery_timestamp set: empty string parses as error.

	s.controlCharging()

	assert.Equal(t, 1, mock.starts, "missing timestamp must fail open")
}

func TestControlCharging_PlugCheck_AllowsWhenTimestampUnparseable(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "0")
	_ = st.Set("vehicle.battery_timestamp", "not-a-timestamp")

	s.controlCharging()

	assert.Equal(t, 1, mock.starts, "unparseable timestamp must fail open")
}

// Lower-bound: 30min minimum age cap prevents short poll_intervals (e.g. 5min)
// from causing flaky blocks. With poll_interval=5, max age is still 30min.
func TestControlCharging_PlugCheck_BlocksWhenFreshShortInterval(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	_ = st.Set("vehicle.require_plug_check", "true")
	_ = st.Set("vehicle.plug_status", "0")
	_ = st.Set("vehicle.poll_interval", "5")
	// 20min old: would exceed 2×5min=10min, but 30min floor applies.
	_ = st.Set("vehicle.battery_timestamp", now.Add(-20*time.Minute).UTC().Format(time.RFC3339))

	s.controlCharging()

	assert.Equal(t, 0, mock.starts, "30min floor keeps fresh data trusted")
}

// --- SuspendedEV (car done charging) ---
// When the car reports SuspendedEV, don't send any OCPP commands.
// RemoteStopTransaction is ignored by chargers like Zappi in this state.
// RemoteStartTransaction won't work because the car won't accept power.
// The session ends naturally when the car unplugs.

func TestControlCharging_Schedule_NoopOnSuspendedEV(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "SuspendedEV", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	// Inside charge window: should still noop because car is done
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.controlCharging()

	assert.Equal(t, 0, mock.stops, "don't send RemoteStopTransaction on SuspendedEV")
	assert.Equal(t, 0, mock.starts, "don't try to start on SuspendedEV")
}

func TestControlCharging_Force_NoopOnSuspendedEV(t *testing.T) {
	mock := &mockCharger{connected: true, status: "SuspendedEV", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 0, mock.stops, "don't send RemoteStopTransaction on SuspendedEV")
	assert.Equal(t, 0, mock.starts, "don't try to start on SuspendedEV")
}

func TestControlCharging_SuspendedEV_NoTxn_NoStart(t *testing.T) {
	mock := &mockCharger{connected: true, status: "SuspendedEV", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()

	assert.Equal(t, 0, mock.stops, "no transaction to stop")
	assert.Equal(t, 0, mock.starts, "car won't accept power in SuspendedEV")
}

// sendStop must not emit a "stop" chart marker any more: that's now the
// OCPP handler's job, fired off the actual StopTransaction so the marker
// time matches the real stop time and we don't double-mark.
func TestSendStop_DoesNotEmitChartMarker(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	s.controlCharging()
	require.Equal(t, 1, mock.stops, "stop should have been requested")

	markers, err := st.RecentChartMarkers(48)
	require.NoError(t, err)
	for _, m := range markers {
		assert.NotEqual(t, "stop", m.Type,
			"scheduler must not emit stop marker: OCPP layer owns it")
	}
}

// sendStart still emits a "start" marker (out of scope for this PR: kept as
// a regression guard so we notice if someone changes it inadvertently).
func TestSendStart_StillEmitsStartMarker(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.controlCharging()
	require.Equal(t, 1, mock.starts)

	markers, err := st.RecentChartMarkers(48)
	require.NoError(t, err)
	startCount := 0
	for _, m := range markers {
		if m.Type == "start" {
			startCount++
		}
	}
	assert.Equal(t, 1, startCount)
}

// --- OnModeChanged ---

func TestOnModeChanged_OffToForce_StartsTxn(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.OnModeChanged("off", "force")

	assert.Equal(t, 1, mock.starts, "explicit user mode change must start synchronously")
	assert.Equal(t, 0, mock.stops)
}

// User explicitly forces charging in SuspendedEV state. Zappi may ignore
// the RemoteStart, but the user's intent is honored, and the override is
// recorded in the system event for diagnostics.
func TestOnModeChanged_BypassesSuspendedEV(t *testing.T) {
	mock := &mockCharger{connected: true, status: "SuspendedEV", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")

	s.OnModeChanged("schedule", "force")

	assert.Equal(t, 1, mock.starts, "forced mode change must bypass SuspendedEV gate")
}

// A stuck cooldown (e.g., from an earlier RemoteStart that timed out in the
// cloud proxy) must NOT silently swallow a fresh user-driven mode change.
func TestOnModeChanged_ClearsCooldown(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "force")
	s.lastStartSent = time.Now() // simulate a recent stuck send

	s.OnModeChanged("off", "force")

	assert.Equal(t, 1, mock.starts, "mode change must clear stale cooldown and try the start")
}

func TestOnModeChanged_AnyToOff_StopsTxn(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "off")

	s.OnModeChanged("force", "off")

	assert.Equal(t, 1, mock.stops, "Off mode must stop the active transaction")
	assert.Equal(t, 0, mock.starts)
}

// Off→Schedule with a plugged car and an active scheduled window: explicit
// mode change must start within the window (no 5s debounce + 30s tick wait).
func TestOnModeChanged_OffToSchedule_StartsInWindow(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-30 * time.Minute),
				End:   now.Add(30 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.OnModeChanged("off", "schedule")

	assert.Equal(t, 1, mock.starts)
}

// Schedule mode without an active window: OnModeChanged must not start:
// schedule mode still respects the schedule, even when explicitly switched.
func TestOnModeChanged_ToSchedule_NoStartOutsideWindow(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(1 * time.Hour),
				End:   now.Add(2 * time.Hour),
				Power: 11000,
			}},
		}},
	}

	s.OnModeChanged("off", "schedule")

	assert.Equal(t, 0, mock.starts, "schedule mode still respects window even on explicit switch")
}

// --- Recompute during an active transaction (replaces the old freeze) ---

func withNow(t *testing.T, now time.Time) {
	t.Helper()
	orig := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = orig })
}

// Regression 2026-11-15: a force window added during a running transaction
// must reach the live plan immediately (the freeze ignored it and the
// scheduler RemoteStopped the session inside the user's window).
func TestRecompute_ForceOverrideAppliesDuringTransaction(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Charging", txnID: 96}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "10800")
	_ = st.Set("scheduler.current_soc", "80")
	_ = st.Set("scheduler.departure_time", now.Add(8*time.Hour).Format("15:04"))
	s.tariff = &mockTariff{rates: []tariff.Rate{{Start: now.Add(-time.Hour), End: now.Add(5 * time.Hour), Price: 0.5}}}

	_, err := st.InsertOverride(store.ScheduleOverride{Kind: store.OverrideKindForce, Start: now, End: now.Add(3 * time.Hour), PowerW: 10800})
	require.NoError(t, err)

	s.recompute()

	require.NotNil(t, s.current)
	withNow(t, now.Add(2*time.Hour))
	assert.True(t, IsChargeTime(s.current), "force window must be in the live plan while charging")
	s.controlCharging()
	assert.Equal(t, 0, mock.stops, "must not stop inside the user's force window")
}

// A block window added mid-session cuts the preserved in-progress period.
func TestRecompute_BlockOverrideCutsActivePeriod(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Charging", txnID: 5}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.current_soc", "72")
	_ = st.Set("scheduler.departure_time", now.Add(8*time.Hour).Format("15:04"))
	s.tariff = &mockTariff{}
	s.current = &Schedule{Slots: []ScheduleSlot{{Date: "d", Periods: []SchedulePeriod{
		{Start: now.Add(-10 * time.Minute), End: now.Add(50 * time.Minute), Power: 11000, Price: 0.3},
	}}}}
	_, err := st.InsertOverride(store.ScheduleOverride{Kind: store.OverrideKindBlock, Start: now.Add(-time.Minute), End: now.Add(time.Hour)})
	require.NoError(t, err)

	s.recompute()

	assert.False(t, IsChargeTime(s.current), "block window must win over the in-progress period")
}

// --- Pausing between blocks (task: profiles instead of stop/start) ---

func twoBlockPlan(now time.Time) *Schedule {
	return &Schedule{Slots: []ScheduleSlot{{Date: "d", Periods: []SchedulePeriod{
		{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour), Power: 11000, Price: 0.3},
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Power: 11000, Price: 0.3},
	}}}}
}

func TestControl_GapWithFutureBlockPausesInsteadOfStopping(t *testing.T) {
	now := time.Now()
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "SuspendedEVSE", txnID: 7}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = twoBlockPlan(now)

	s.controlCharging() // applies the profile, then pauses

	assert.Equal(t, 1, mock.profiles, "plan profile written once")
	assert.Equal(t, 0, mock.stops, "transaction kept open across the gap")
	s.controlCharging()
	assert.Equal(t, 1, mock.profiles, "no rewrite while in sync")
	assert.Equal(t, 0, mock.stops)
}

func TestControl_GapStopsWhenProfileCannotBeWritten(t *testing.T) {
	now := time.Now()
	withNow(t, now)
	fc := &failingCharger{mockCharger: mockCharger{connected: true, status: "Charging", txnID: 7}, failSet: true}
	s, st := newTestScheduler(t, &fc.mockCharger)
	s.charger = fc
	_ = st.Set("charger.mode", "schedule")
	s.current = twoBlockPlan(now)

	s.controlCharging()

	assert.Equal(t, 1, fc.stops, "without our profile on the charger, fall back to stopping")
}

func TestControl_StopsWhenNothingMorePlanned(t *testing.T) {
	now := time.Now()
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Charging", txnID: 7}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = &Schedule{Slots: []ScheduleSlot{{Date: "d", Periods: []SchedulePeriod{
		{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour), Power: 11000},
	}}}}

	_ = st.Set("scheduler.current_soc", "80")
	s.controlCharging()

	assert.Equal(t, 1, mock.stops)
}

func TestBuildPlanProfile_ZeroGapsAndLeadingZero(t *testing.T) {
	now := time.Date(2026, 11, 16, 20, 30, 0, 0, time.UTC)
	withNow(t, now)
	sched := &Schedule{Slots: []ScheduleSlot{{Periods: []SchedulePeriod{
		{Start: now.Add(30 * time.Minute), End: now.Add(90 * time.Minute), Power: 10800},
		{Start: now.Add(90 * time.Minute), End: now.Add(150 * time.Minute), Power: 10800},
		{Start: now.Add(270 * time.Minute), End: now.Add(330 * time.Minute), Power: 10800},
	}}}}
	p := BuildPlanProfile(sched)
	require.NotNil(t, p)
	assert.Equal(t, now.Truncate(time.Hour), p.ChargingSchedule.StartSchedule.Time)
	var got [][2]float64
	for _, cp := range p.ChargingSchedule.ChargingSchedulePeriod {
		got = append(got, [2]float64{float64(cp.StartPeriod), cp.Limit})
	}
	assert.Equal(t, [][2]float64{{0, 0}, {3600, 10800}, {3 * 3600, 0}, {5 * 3600, 10800}, {6 * 3600, 0}}, got)
}

// The plan hash must not change as time passes within the running block.
func TestPlanHash_StableAcrossTime(t *testing.T) {
	now := time.Now().Truncate(time.Hour).Add(10 * time.Minute)
	sched := twoBlockPlan(now)
	sched.Slots[0].Periods[0].End = now.Add(30 * time.Minute)
	withNow(t, now)
	h1 := planHash(sched)
	withNow(t, now.Add(15*time.Minute))
	h2 := planHash(sched)
	assert.Equal(t, h1, h2)
}

// --- Fix 3: RequestModeChange debounce ---

// A single RequestModeChange must NOT take effect synchronously: the store
// stays on the previous mode, OnModeChanged hasn't fired yet, and the
// pending change is exposed via PendingModeChange.
func TestRequestModeChange_DoesNotApplyImmediately(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	s.RequestModeChange("off")

	assert.Equal(t, "schedule", st.GetDefault("charger.mode", ""),
		"mode must not flip until debounce elapses")
	assert.Equal(t, 0, mock.stops, "no OCPP stop until debounce elapses")
	pending, applyAt := s.PendingModeChange()
	assert.Equal(t, "off", pending, "PendingModeChange must surface the requested mode")
	assert.False(t, applyAt.IsZero(), "applyAt must be populated")
}

// Multiple rapid RequestModeChange calls coalesce, only the LAST mode wins,
// fired once, after modeChangeDebounce of silence from the last call.
func TestRequestModeChange_CoalescesRapidClicks(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")

	s.RequestModeChange("off")
	s.RequestModeChange("force")
	s.RequestModeChange("schedule") // back to current: still scheduled

	// All calls within the debounce window, nothing applied yet.
	assert.Equal(t, "schedule", st.GetDefault("charger.mode", ""))
	assert.Equal(t, 0, mock.starts)
	assert.Equal(t, 0, mock.stops)

	// Drive the timer manually rather than waiting 5s in a test.
	s.modeDebounceMu.Lock()
	if s.modeDebounceTimer != nil {
		s.modeDebounceTimer.Stop()
	}
	s.modeDebounceMu.Unlock()
	s.applyPendingMode()

	// Last requested mode wins. ("schedule" matches current → no OCPP churn,
	// but OnModeChanged still ran which is observable via cooldown clear).
	assert.Equal(t, "schedule", st.GetDefault("charger.mode", ""))
	pending, _ := s.PendingModeChange()
	assert.Empty(t, pending, "pending mode must be cleared after apply")
}

// When the final pending mode differs from the current applied mode, applying
// fires the corresponding OCPP command via OnModeChanged.
func TestRequestModeChange_AppliesLastMode(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date: now.Format("2006-01-02"),
			Periods: []SchedulePeriod{{
				Start: now.Add(-15 * time.Minute),
				End:   now.Add(15 * time.Minute),
				Power: 11000,
			}},
		}},
	}

	s.RequestModeChange("schedule") // no-op intent
	s.RequestModeChange("off")      // final intent

	s.modeDebounceMu.Lock()
	if s.modeDebounceTimer != nil {
		s.modeDebounceTimer.Stop()
	}
	s.modeDebounceMu.Unlock()
	s.applyPendingMode()

	assert.Equal(t, "off", st.GetDefault("charger.mode", ""))
	assert.Equal(t, 1, mock.stops, "off mode applied must send RemoteStopTransaction for the active txn")
}

// Manual windows win over a running session (reverses the old "window edits
// never touch an active transaction" rule, which let the scheduler ignore a
// user's force window on 2026-11-15). Blocking the rest of the only planned
// period ends the session; removing the block lets the plan resume.
func TestBlockWindowOverRunningSessionIsHonoured(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	withNow(t, now)
	mock := &mockCharger{connected: true, status: "Charging", txnID: 99}
	s, st := newTestScheduler(t, mock)
	require.NoError(t, st.Set("charger.mode", "schedule"))
	require.NoError(t, st.Set("scheduler.safe_soc", "70")) // must charge tonight
	s.tariff = &mockTariff{rates: []tariff.Rate{
		{Start: now.Add(-30 * time.Minute), End: now.Add(30 * time.Minute), Price: 0.3},
		{Start: now.Add(30 * time.Minute), End: now.Add(90 * time.Minute), Price: 0.5},
	}}
	s.recompute()
	require.True(t, IsChargeTime(s.current))

	_, err := st.InsertOverride(store.ScheduleOverride{Kind: store.OverrideKindBlock, Start: now.Add(-time.Minute), End: now.Add(30 * time.Minute)})
	require.NoError(t, err)
	s.recompute()
	s.controlCharging()

	assert.False(t, IsChargeTime(s.current), "the block window wins over the running hour")
	assert.Equal(t, 0, mock.stops, "more is planned later: pause via profile, don't stop")
	assert.True(t, s.profileInSync(s.current))
}

func TestRemovingLastForceClearsFutureScheduleWithoutRates(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Available"}
	s, st := newTestScheduler(t, mock)
	s.tariff = &mockTariff{}
	now := time.Now().UTC().Truncate(time.Second)
	id, err := st.InsertOverride(store.ScheduleOverride{Kind: store.OverrideKindForce, Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), PowerW: 11000})
	require.NoError(t, err)
	s.recompute()
	require.NotNil(t, s.current)
	require.NoError(t, st.DeleteOverride(id))
	s.recompute()
	assert.Nil(t, s.current, "deleting the last override must remove its stale schedule even without rates")
	assert.Zero(t, mock.stops)
}
