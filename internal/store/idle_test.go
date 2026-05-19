package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedDay returns the UTC midnight of a fixed reference date used by these
// tests. Choosing a past day keeps SnapshotDailyIdle's "clip to now" guard
// out of the way — it only kicks in for today.
func fixedDay() time.Time {
	return time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
}

// seedMeter inserts a synthetic meter series for one day, ramping the
// cumulative energy_wh counter linearly from `startWh` to `endWh` over 24
// readings (one per hour). With one reading per hour the per-window
// max-min math is unambiguous and the test stays readable.
func seedMeter(t *testing.T, s *Store, day time.Time, startWh, endWh float64) {
	t.Helper()
	steps := 24
	step := (endWh - startWh) / float64(steps-1)
	for i := 0; i < steps; i++ {
		ts := day.Add(time.Duration(i) * time.Hour)
		require.NoError(t, s.InsertMeterReading(MeterReading{
			Timestamp: ts,
			PowerW:    100,
			EnergyWh:  startWh + step*float64(i),
		}))
	}
}

// seedRate inserts a single hourly tariff covering [from, to) so cost
// calls don't return zero.
func seedRate(t *testing.T, s *Store, from, to time.Time, price float64) {
	t.Helper()
	rates := []Rate{}
	cursor := from
	for cursor.Before(to) {
		next := cursor.Add(time.Hour)
		if next.After(to) {
			next = to
		}
		rates = append(rates, Rate{Start: cursor, End: next, Price: price})
		cursor = next
	}
	require.NoError(t, s.SaveRates("pstryk", rates))
}

func TestSnapshotDailyIdle_NoSessions(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedMeter(t, s, day, 0, 23000) // 23 kWh ramped over the day
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)

	require.NoError(t, s.SnapshotDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 23.0, rows[0].EnergyKWh, 0.01)
	assert.Greater(t, rows[0].Cost, 0.0)
}

func TestSnapshotDailyIdle_FullDaySession(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedMeter(t, s, day, 0, 23000)
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)

	stop := day.Add(24 * time.Hour)
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 1,
		StartTime: day, MeterStart: 0,
	}))
	require.NoError(t, s.StopSession(1, stop, 23, 23, 23))

	require.NoError(t, s.SnapshotDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 0, rows[0].EnergyKWh, 0.001)
	assert.InDelta(t, 0, rows[0].Cost, 0.001)
}

func TestSnapshotDailyIdle_PartialDaySession(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedMeter(t, s, day, 0, 23000) // 1 kWh per hour
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)

	// Session 06:00-12:00 — six hours in the middle.
	start := day.Add(6 * time.Hour)
	stop := day.Add(12 * time.Hour)
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 1,
		StartTime: start, MeterStart: 0,
	}))
	require.NoError(t, s.StopSession(1, stop, 6, 6, 6))

	require.NoError(t, s.SnapshotDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// seedMeter places 24 hourly readings ramping 0..23 kWh, so each hour
	// boundary delta is exactly 1 kWh.
	// Non-charging windows: [00:00, 06:00) → readings 0..5 → 5 kWh.
	// [12:00, 24:00) → readings 12..23 → 11 kWh. Total 16 kWh.
	assert.InDelta(t, 16.0, rows[0].EnergyKWh, 0.5)
}

func TestSnapshotDailyIdle_OverlappingSessions(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedMeter(t, s, day, 0, 23000)
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)

	// Two sessions that overlap: 06-10 and 09-14 — merged to 06-14.
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 1,
		StartTime: day.Add(6 * time.Hour),
	}))
	require.NoError(t, s.StopSession(1, day.Add(10*time.Hour), 4, 4, 4))
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 2,
		StartTime: day.Add(9 * time.Hour),
	}))
	require.NoError(t, s.StopSession(2, day.Add(14*time.Hour), 5, 5, 5))

	require.NoError(t, s.SnapshotDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// Non-charging: [00:00, 06:00) (5 kWh) + [14:00, 24:00) (9 kWh) = 14 kWh
	assert.InDelta(t, 14.0, rows[0].EnergyKWh, 0.5)
}

func TestSnapshotDailyIdle_StraddlesMidnight(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedMeter(t, s, day, 0, 23000)
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)

	// Session spans yesterday 22:00 through today 04:00 — only the first
	// 4 hours of "today" should be subtracted.
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 1,
		StartTime: day.Add(-2 * time.Hour),
	}))
	require.NoError(t, s.StopSession(1, day.Add(4*time.Hour), 6, 6, 6))

	require.NoError(t, s.SnapshotDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(48*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// Non-charging today: [04:00, 24:00) = readings 4..23 → 19 kWh
	assert.InDelta(t, 19.0, rows[0].EnergyKWh, 0.5)
}

func TestSnapshotDailyIdle_ActiveSessionToday(t *testing.T) {
	s := testStore(t)
	// Snapshot for today, with an active session that started 3h ago.
	now := time.Now().UTC().Truncate(time.Second)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if now.Sub(day) < 4*time.Hour {
		t.Skip("test requires at least 4h elapsed today")
	}

	// Insert a few meter readings spread across the day so the snapshot
	// has enough rows to compute against.
	for i := 0; i < 4; i++ {
		ts := day.Add(time.Duration(i) * time.Hour)
		if !ts.Before(now) {
			break
		}
		require.NoError(t, s.InsertMeterReading(MeterReading{
			Timestamp: ts, PowerW: 50, EnergyWh: float64(i) * 1000,
		}))
	}
	require.NoError(t, s.InsertMeterReading(MeterReading{
		Timestamp: now.Add(-time.Minute), PowerW: 50, EnergyWh: 5000,
	}))
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)

	// Active session running for the past 3h — clipped to dayStart..now.
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 99,
		StartTime: now.Add(-3 * time.Hour),
	}))

	require.NoError(t, s.SnapshotDailyIdle(now))

	rows, err := s.DailyIdleByDateRange(day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// Energy should be non-negative; the active session zeros out its window.
	assert.GreaterOrEqual(t, rows[0].EnergyKWh, 0.0)
}

func TestSnapshotDailyIdle_NoMeterRowsPreservesExisting(t *testing.T) {
	s := testStore(t)
	day := fixedDay()

	// Pre-existing snapshot.
	require.NoError(t, s.UpsertDailyIdle("2026-04-15", 7.5, 4.2))

	// No meter readings inserted for the day — snapshot must be a no-op.
	require.NoError(t, s.SnapshotDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 7.5, rows[0].EnergyKWh, 0.001)
	assert.InDelta(t, 4.2, rows[0].Cost, 0.001)
}

func TestSnapshotDailyIdle_FutureDayNoOp(t *testing.T) {
	s := testStore(t)
	future := time.Now().Add(48 * time.Hour).UTC()
	require.NoError(t, s.SnapshotDailyIdle(future))

	rows, err := s.DailyIdleByDateRange(future, future.Add(24*time.Hour))
	require.NoError(t, err)
	assert.Len(t, rows, 0)
}

func TestSnapshotDailyIdle_CostMatchesPerWindowSum(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedMeter(t, s, day, 0, 24000) // 1 kWh per hour exactly
	// Two-tier rate: cheap morning, pricey afternoon.
	rates := []Rate{
		{Start: day, End: day.Add(12 * time.Hour), Price: 0.5},
		{Start: day.Add(12 * time.Hour), End: day.Add(24 * time.Hour), Price: 1.5},
	}
	require.NoError(t, s.SaveRates("pstryk", rates))

	// Session 10-14 — straddles the rate boundary, so a price-aware
	// per-window cost matters.
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 1,
		StartTime: day.Add(10 * time.Hour),
	}))
	require.NoError(t, s.StopSession(1, day.Add(14*time.Hour), 4, 4, 4))

	require.NoError(t, s.SnapshotDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Manually compute expected cost: window 00-10 ≈ 10 kWh @ 0.5 = 5.0;
	// window 14-end ≈ 9.5..10 kWh @ 1.5 ≈ 14.25..15.0. Allow 0.5 PLN slack
	// for the seedMeter integer rounding at hour boundaries.
	assert.InDelta(t, 19.5, rows[0].Cost, 1.0)
}

// seedPstryk inserts hourly pstryk_consumption rows starting at `day` with a
// constant per-hour Wh value. Mirrors seedMeter but for the new backfill path.
func seedPstryk(t *testing.T, s *Store, day time.Time, hours int, perHourWh float64) {
	t.Helper()
	frames := make([]PstrykConsumption, hours)
	for i := 0; i < hours; i++ {
		frames[i] = PstrykConsumption{
			Hour:     day.Add(time.Duration(i) * time.Hour),
			EnergyWh: perHourWh,
		}
	}
	require.NoError(t, s.UpsertPstrykConsumption(frames))
}

func TestRebuildDailyIdle_NoPstrykPreservesExisting(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	require.NoError(t, s.UpsertDailyIdle("2026-04-15", 9.9, 5.5))

	require.NoError(t, s.RebuildDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 9.9, rows[0].EnergyKWh, 0.001)
	assert.InDelta(t, 5.5, rows[0].Cost, 0.001)
}

func TestRebuildDailyIdle_FullDayNoSessions(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)
	seedPstryk(t, s, day, 24, 500) // 500 Wh/hour → 12 kWh/day

	require.NoError(t, s.RebuildDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 12.0, rows[0].EnergyKWh, 0.01)
	assert.Greater(t, rows[0].Cost, 0.0)
}

func TestRebuildDailyIdle_PartialDaySession(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)
	seedPstryk(t, s, day, 24, 1000) // 1 kWh/hour, 24 kWh total

	// Session 06:00-12:00 — six hours.
	require.NoError(t, s.StartSession(Session{
		ChargeBox: "CP", ConnectorID: 1, TransactionID: 1,
		StartTime: day.Add(6 * time.Hour),
	}))
	require.NoError(t, s.StopSession(1, day.Add(12*time.Hour), 6, 6, 6))

	require.NoError(t, s.RebuildDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	// Non-charging windows: 6h + 12h = 18h × 1 kWh = 18 kWh.
	assert.InDelta(t, 18.0, rows[0].EnergyKWh, 0.01)
}

func TestRebuildDailyIdle_OverwritesPartialSnapshot(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	seedRate(t, s, day, day.Add(24*time.Hour), 1.0)

	// Stale snapshot from before downtime — only 3 kWh recorded.
	require.NoError(t, s.UpsertDailyIdle("2026-04-15", 3.0, 1.5))

	// Pstryk backfill brought in the full day's data.
	seedPstryk(t, s, day, 24, 1000) // 24 kWh

	require.NoError(t, s.RebuildDailyIdle(day))

	rows, err := s.DailyIdleByDateRange(day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.InDelta(t, 24.0, rows[0].EnergyKWh, 0.01,
		"rebuild must replace partial snapshot, not add to it")
}

func TestPstrykEnergyKWh_PartialHourProrated(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	// Two consecutive hours, 1000 Wh each.
	require.NoError(t, s.UpsertPstrykConsumption([]PstrykConsumption{
		{Hour: day, EnergyWh: 1000},
		{Hour: day.Add(time.Hour), EnergyWh: 1000},
	}))

	// Half of the first hour and full second hour: 0.5 + 1.0 = 1.5 kWh.
	got, err := s.PstrykEnergyKWh(day.Add(30*time.Minute), day.Add(2*time.Hour))
	require.NoError(t, err)
	assert.InDelta(t, 1.5, got, 0.001)
}

func TestHourlyConsumption_PrefersPstryk(t *testing.T) {
	s := testStore(t)
	day := fixedDay()
	// Both sources populated; pstryk should win.
	seedMeter(t, s, day, 0, 23000) // would yield ~1 kWh/hour deltas
	require.NoError(t, s.UpsertPstrykConsumption([]PstrykConsumption{
		{Hour: time.Now().UTC().Truncate(time.Hour).Add(-time.Hour), EnergyWh: 555},
	}))

	got, err := s.HourlyConsumption(48)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	// Exact value match confirms it came from pstryk, not the meter ramp.
	assert.InDelta(t, 555.0, got[0].EnergyWh, 0.01)
	assert.InDelta(t, 555.0, got[0].PowerW, 0.01, "powerW equals Wh for hourly buckets")
}

func TestHourlyConsumption_FallsBackToMeter(t *testing.T) {
	s := testStore(t)
	// Use today so the 48h cutoff in HourlyConsumption keeps the rows visible.
	now := time.Now().UTC().Truncate(time.Hour)
	day := now.Add(-12 * time.Hour)
	seedMeter(t, s, day, 0, 12000) // 12h ramp; some readings fall within last 48h

	// No pstryk_consumption rows — fall back to aggregating meter_readings.
	got, err := s.HourlyConsumption(48)
	require.NoError(t, err)
	require.NotEmpty(t, got, "meter fallback should produce hourly rows")
}

func TestNonChargingWindows_InversionMath(t *testing.T) {
	// Direct unit test of the helper — gives us precise coverage on the
	// merge/invert edge cases without going through SQL.
	day := fixedDay()
	end := day.Add(24 * time.Hour)
	cases := []struct {
		name string
		in   []TimeWindow
		want []TimeWindow
	}{
		{
			name: "empty",
			in:   nil,
			want: []TimeWindow{{day, end}},
		},
		{
			name: "single in middle",
			in:   []TimeWindow{{day.Add(6 * time.Hour), day.Add(12 * time.Hour)}},
			want: []TimeWindow{
				{day, day.Add(6 * time.Hour)},
				{day.Add(12 * time.Hour), end},
			},
		},
		{
			name: "covers start",
			in:   []TimeWindow{{day, day.Add(8 * time.Hour)}},
			want: []TimeWindow{{day.Add(8 * time.Hour), end}},
		},
		{
			name: "covers full",
			in:   []TimeWindow{{day, end}},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := invertWindows(day, end, mergeWindows(tc.in))
			assert.Equal(t, tc.want, got)
		})
	}
}
