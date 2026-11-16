package scheduler

import (
	"fmt"
	"strconv"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/consi/grosz/internal/events"
)

// CarLimiter writes the car's charge limit (Renault). Optional.
type CarLimiter interface {
	SetSocTarget(target int) error
}

// SetCarLimiter wires the car charge-limit writer (cheap fill / trip reset).
func (s *Scheduler) SetCarLimiter(cl CarLimiter) {
	s.mu.Lock()
	s.carLimiter = cl
	s.mu.Unlock()
}

const (
	// settingFillRaisedFrom holds the normal limit while grosz has raised the
	// car's limit for a cheap fill ("" = not raised). Distinguishes our own
	// raise from a user-raised limit (a trip).
	settingFillRaisedFrom = "scheduler.fill_raised_from"
	settingTripReadyBy    = "scheduler.trip_ready_by" // RFC3339, optional

	// Renault's API has a strict quota; never write the limit more often.
	carLimitCooldown = 10 * time.Minute
)

// normalTarget is the everyday charge target (%).
func (s *Scheduler) normalTarget() float64 {
	if v := s.store.GetFloat("scheduler.target_soc", 0); v > 0 {
		return v
	}
	return 80
}

// manageCarLimit keeps the car's charge limit in line with the plan:
//   - raises it for a cheap-fill period that is active now,
//   - restores the normal limit once no cheap fill is active or imminent,
//   - on unplug, restores it after a cheap fill or a trip (user-raised limit).
func (s *Scheduler) manageCarLimit(sched *Schedule, plugged, justUnplugged bool) {
	s.mu.RLock()
	cl := s.carLimiter
	last := s.lastCarLimitWrite
	s.mu.RUnlock()
	if cl == nil {
		return
	}
	now := timeNow()
	if !last.IsZero() && now.Sub(last) < carLimitCooldown {
		return
	}
	normal := s.normalTarget()
	car := s.store.GetFloat("vehicle.soc_target", 0)
	raisedFrom := s.store.GetDefault(settingFillRaisedFrom, "")

	write := func(target float64, why string) {
		s.mu.Lock()
		s.lastCarLimitWrite = now
		s.mu.Unlock()
		err := cl.SetSocTarget(int(target))
		res := map[string]any{"target": target, "reason": why}
		if err != nil {
			res["error"] = err.Error()
			s.events.Warn(events.ActionControlCharging, map[string]any{"carLimit": car}, res)
			return
		}
		_ = s.store.Set("vehicle.soc_target", strconv.Itoa(int(target)))
		s.events.Info(events.ActionControlCharging, map[string]any{"carLimit": car}, res)
	}

	if justUnplugged {
		switch {
		case raisedFrom != "":
			_ = s.store.Set(settingFillRaisedFrom, "")
			write(normal, "restore_after_fill")
		case car > normal+0.5:
			_ = s.store.Set(settingTripReadyBy, "")
			write(normal, "restore_after_trip")
		}
		return
	}
	if !plugged {
		return
	}

	want := 0.0
	soon := false
	for _, p := range sched.ActivePeriods() {
		if p.Reason != ReasonCheapFill || p.FillSoC <= 0 {
			continue
		}
		if !now.Before(p.Start) && now.Before(p.End) {
			want = p.FillSoC
		} else if p.Start.After(now) && p.Start.Sub(now) <= 2*time.Hour {
			soon = true
		}
	}
	switch {
	case want > 0 && car < want && (raisedFrom != "" || car <= normal+0.5):
		if raisedFrom == "" {
			_ = s.store.Set(settingFillRaisedFrom, fmt.Sprintf("%.0f", normal))
		}
		write(want, "cheap_fill")
	case want == 0 && !soon && raisedFrom != "":
		_ = s.store.Set(settingFillRaisedFrom, "")
		write(normal, "restore_after_fill")
	}
}

// buildHoldProfile keeps an open transaction paused (0 W) for a day when
// nothing is planned, instead of clearing the profile (which would let the
// charger run at full power) or stopping the transaction (which parks Zappi
// in Finishing until the cable is re-plugged).
func buildHoldProfile(now time.Time) *types.ChargingProfile {
	start := now.Truncate(time.Hour)
	return &types.ChargingProfile{
		ChargingProfileId:      1,
		StackLevel:             0,
		ChargingProfilePurpose: types.ChargingProfilePurposeTxDefaultProfile,
		ChargingProfileKind:    types.ChargingProfileKindAbsolute,
		ValidFrom:              types.NewDateTime(start),
		ValidTo:                types.NewDateTime(start.Add(48 * time.Hour)),
		ChargingSchedule: &types.ChargingSchedule{
			ChargingRateUnit:       types.ChargingRateUnitWatts,
			ChargingSchedulePeriod: []types.ChargingSchedulePeriod{{StartPeriod: 0, Limit: 0}},
			StartSchedule:          types.NewDateTime(start),
		},
	}
}
