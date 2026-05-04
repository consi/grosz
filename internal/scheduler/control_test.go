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
func (m *mockTariff) Name() string                   { return "mock" }
func (m *mockTariff) Stop()                          {}

// --- helpers ---

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.New(dbPath, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestScheduler(t *testing.T, mock *mockCharger) (*Scheduler, *store.Store) {
	t.Helper()
	st := testStore(t)
	_ = st.Set("zappi.charge_box_id", "CP001")
	_ = st.Set("zappi.id_tag", "grosz")

	s := &Scheduler{
		charger:  mock,
		store:    st,
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

	assert.Equal(t, 1, mock.stops)
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

func TestControlCharging_Schedule_StopsWhenNoSchedule(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	s.current = nil

	s.controlCharging()

	assert.Equal(t, 1, mock.stops, "should stop when schedule is nil")
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

	// Before window — no start
	timeNow = func() time.Time { return windowStart.Add(-1 * time.Hour) }
	s.controlCharging()
	assert.Equal(t, 0, mock.starts, "before window: should not start")

	// Window opens — starts
	timeNow = func() time.Time { return windowStart.Add(5 * time.Minute) }
	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "window open: should start")

	// Simulate transaction active
	mock.setStatus("Charging", 1)

	// Still in window — no duplicate start
	timeNow = func() time.Time { return windowStart.Add(1 * time.Hour) }
	s.controlCharging()
	assert.Equal(t, 1, mock.starts, "still in window: no duplicate")
	assert.Equal(t, 0, mock.stops, "still in window: no stop")

	// Window closes — stops
	timeNow = func() time.Time { return windowEnd.Add(5 * time.Minute) }
	s.controlCharging()
	assert.Equal(t, 1, mock.stops, "window closed: should stop")
}

// --- Schedule Mode: SoC scenarios ---

func TestRecompute_SoCReached_NoSchedule(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.skip_above_soc", "80")
	_ = st.Set("scheduler.current_soc", "85")
	_ = st.Set("scheduler.deadline_time", "07:00")

	now := time.Now().Truncate(time.Hour)
	rates := []tariff.Rate{{Start: now, End: now.Add(time.Hour), Price: 0.30}}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	assert.Nil(t, s.current, "schedule should be nil when SoC above skip threshold with only positive prices")
	assert.Equal(t, "soc_above_threshold", s.skipReasonKey)
}

func TestRecompute_SoCAboveSkip_NegativePricesStillSchedule(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.skip_above_soc", "70")
	_ = st.Set("scheduler.current_soc", "75")
	_ = st.Set("scheduler.deadline_time", "07:00")
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

func TestRecompute_PreservesActiveSlot_OnSoCSkip(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.skip_above_soc", "70")
	_ = st.Set("scheduler.current_soc", "70")
	_ = st.Set("scheduler.deadline_time", "07:00")

	// In-progress slot whose period covers now.
	activePeriod := SchedulePeriod{
		Start: now.Add(-30 * time.Minute),
		End:   now.Add(30 * time.Minute),
		Power: 11000,
		Price: 0.30,
	}
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date:     now.Format("2006-01-02"),
			Deadline: now.Add(time.Hour),
			Periods:  []SchedulePeriod{activePeriod},
			Cost:     3.30,
			Energy:   11,
		}},
		Cost:     3.30,
		Energy:   11,
		Deadline: now.Add(time.Hour),
	}

	rates := []tariff.Rate{{Start: now, End: now.Add(time.Hour), Price: 0.30}}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	require.NotNil(t, s.current, "active slot must survive SoC-skip recompute")
	require.Len(t, s.current.Slots, 1, "only the active slot should remain")
	assert.Equal(t, now.Format("2006-01-02"), s.current.Slots[0].Date)
	require.Len(t, s.current.Slots[0].Periods, 1)
	assert.Equal(t, activePeriod, s.current.Slots[0].Periods[0],
		"period must match the original in-progress one byte-for-byte")
	assert.Empty(t, s.skipReasonKey, "skipReason must be cleared when schedule is preserved")
	assert.Empty(t, s.skipReason)

	// A subsequent controlCharging must not stop the active txn.
	s.controlCharging()
	mock.mu.Lock()
	defer mock.mu.Unlock()
	assert.Equal(t, 0, mock.stops, "preserved slot must keep IsChargeTime true → no stop issued")
}

func TestRecompute_PreservesActiveSlot_OnTargetReached(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.current_soc", "80") // target reached → TargetEnergy = 0
	_ = st.Set("scheduler.deadline_time", "07:00")

	activePeriod := SchedulePeriod{
		Start: now.Add(-15 * time.Minute),
		End:   now.Add(45 * time.Minute),
		Power: 11000,
		Price: 0.25,
	}
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date:     now.Format("2006-01-02"),
			Deadline: now.Add(time.Hour),
			Periods:  []SchedulePeriod{activePeriod},
			Cost:     2.75,
			Energy:   11,
		}},
	}

	rates := []tariff.Rate{{Start: now, End: now.Add(time.Hour), Price: 0.25}}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	require.NotNil(t, s.current, "active slot must survive target-reached recompute")
	require.Len(t, s.current.Slots, 1)
	require.Len(t, s.current.Slots[0].Periods, 1)
	assert.Equal(t, activePeriod, s.current.Slots[0].Periods[0])
	assert.Empty(t, s.skipReasonKey)
}

func TestRecompute_PreservesActiveSlot_OnNoAffordableRates(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.current_soc", "50")
	_ = st.Set("scheduler.max_price", "0.10") // every rate filtered
	_ = st.Set("scheduler.deadline_time", "07:00")
	_ = st.Set("charger.max_power", "11000")

	activePeriod := SchedulePeriod{
		Start: now.Add(-10 * time.Minute),
		End:   now.Add(50 * time.Minute),
		Power: 11000,
		Price: 0.05, // already-locked-in cheap period
	}
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date:     now.Format("2006-01-02"),
			Deadline: now.Add(time.Hour),
			Periods:  []SchedulePeriod{activePeriod},
			Cost:     0.55,
			Energy:   11,
		}},
	}

	// Every available rate is above max_price → ComputeSchedule returns nil.
	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.50},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.60},
	}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	require.NotNil(t, s.current, "active slot must survive no-affordable-rates recompute")
	require.Len(t, s.current.Slots, 1)
	require.Len(t, s.current.Slots[0].Periods, 1)
	assert.Equal(t, activePeriod, s.current.Slots[0].Periods[0])
	assert.Empty(t, s.skipReasonKey)
}

func TestRecompute_HappyPath_MergesActiveSlot(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Charging", txnID: 42}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.target_energy", "10")
	_ = st.Set("scheduler.deadline_time", now.Add(24*time.Hour).Format("15:04"))

	// prev: today's slot is in progress with a "locked-in" period.
	todayDate := now.Format("2006-01-02")
	activePeriod := SchedulePeriod{
		Start: now.Add(-20 * time.Minute),
		End:   now.Add(40 * time.Minute),
		Power: 11000,
		Price: 0.20,
	}
	s.current = &Schedule{
		Slots: []ScheduleSlot{{
			Date:     todayDate,
			Deadline: now.Add(time.Hour),
			Periods:  []SchedulePeriod{activePeriod},
			Cost:     2.20,
			Energy:   11,
		}},
	}

	// Rates that ComputeSchedule will favour — different from the active period.
	rates := []tariff.Rate{
		{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), Price: 0.10},
		{Start: now.Add(3 * time.Hour), End: now.Add(4 * time.Hour), Price: 0.15},
	}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	require.NotNil(t, s.current)

	// Find today's slot in the merged schedule and assert its periods are the
	// in-progress ones, not whatever ComputeSchedule produced.
	var todaySlot *ScheduleSlot
	for i, slot := range s.current.Slots {
		if slot.Date == todayDate {
			todaySlot = &s.current.Slots[i]
			break
		}
	}
	require.NotNil(t, todaySlot, "today's slot must be present in merged schedule")
	require.Len(t, todaySlot.Periods, 1)
	assert.Equal(t, activePeriod, todaySlot.Periods[0],
		"today's slot must remain byte-for-byte the in-progress one")
}

func TestRecompute_HappyPath_NoActiveSlot_UsesNewScheduleAsIs(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("charger.max_power", "11000")
	_ = st.Set("scheduler.target_energy", "10")
	_ = st.Set("scheduler.deadline_time", now.Add(24*time.Hour).Format("15:04"))

	// No prev schedule, no in-progress slot.
	s.current = nil

	rates := []tariff.Rate{
		{Start: now, End: now.Add(time.Hour), Price: 0.30},
		{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), Price: 0.50},
	}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	require.NotNil(t, s.current, "freshly computed schedule must replace nil prev")
	require.NotEmpty(t, s.current.Slots)
	// Slot should reflect ComputeSchedule's pick (the cheaper 0.30 rate).
	assert.Equal(t, 0.30, s.current.Slots[0].Periods[0].Price)
}

func TestRecompute_DoesNotPreserveCancelledSlot(t *testing.T) {
	now := time.Now()
	origTimeNow := timeNow
	timeNow = func() time.Time { return now }
	defer func() { timeNow = origTimeNow }()

	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.skip_above_soc", "70")
	_ = st.Set("scheduler.current_soc", "75")
	_ = st.Set("scheduler.deadline_time", "07:00")

	// Slot covers now but is cancelled — must not be treated as in progress.
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

	rates := []tariff.Rate{{Start: now, End: now.Add(time.Hour), Price: 0.30}}
	s.tariff = &mockTariff{rates: rates}

	s.recompute()

	assert.Nil(t, s.current, "cancelled slot must not shield destructive recompute")
	assert.Equal(t, "soc_above_threshold", s.skipReasonKey)
}

func TestRecompute_SoCBelowMin_IgnoresMaxPrice(t *testing.T) {
	mock := &mockCharger{connected: true, status: "Preparing", txnID: 0}
	s, st := newTestScheduler(t, mock)
	_ = st.Set("charger.mode", "schedule")
	_ = st.Set("scheduler.battery_capacity", "52")
	_ = st.Set("scheduler.target_soc", "80")
	_ = st.Set("scheduler.current_soc", "10")
	_ = st.Set("scheduler.min_soc", "20")
	_ = st.Set("scheduler.max_price", "0.10") // very low max price
	_ = st.Set("scheduler.deadline_time", "07:00")
	_ = st.Set("charger.max_power", "11000")

	now := time.Now().Truncate(time.Hour)
	deadline := now.Add(24 * time.Hour)
	rates := make([]tariff.Rate, 24)
	for i := 0; i < 24; i++ {
		rates[i] = tariff.Rate{
			Start: now.Add(time.Duration(i) * time.Hour),
			End:   now.Add(time.Duration(i+1) * time.Hour),
			Price: 0.50, // all above max_price
		}
	}
	s.tariff = &mockTariff{rates: rates}

	// Manually set config with SoC below min — MaxPrice should be zeroed
	cfg := &Config{
		TargetEnergy:    36.4, // (80-10)/100 * 52
		Deadline:        deadline,
		MaxPower:        11000,
		BatteryCapacity: 52,
		TargetSoC:       80,
		CurrentSoC:      10,
		MinSoC:          20,
		MaxPrice:        0.10,
	}
	s.config = cfg
	s.recompute()

	assert.NotNil(t, s.current, "should compute schedule despite expensive rates when SoC below min")
}

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
	assert.Equal(t, 1, mock.stops, "schedule mode must stop outside window")
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
// minutes — between polls, plug_status="0" is leftover from before plug-in.
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
	// No battery_timestamp set — empty string parses as error.

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
	// 20min old — would exceed 2×5min=10min, but 30min floor applies.
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

	// Inside charge window — should still noop because car is done
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

// sendStop must not emit a "stop" chart marker any more — that's now the
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
			"scheduler must not emit stop marker — OCPP layer owns it")
	}
}

// sendStart still emits a "start" marker (out of scope for this PR — kept as
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
