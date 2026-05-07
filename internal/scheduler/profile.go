package scheduler

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

// timeNow is a package-level variable for testing.
var timeNow = time.Now

// BuildProfile converts a Schedule into an OCPP 1.6 ChargingProfile
// using TxDefaultProfile with absolute schedule.
// Only active (non-cancelled) slots are included. Adjacent same-power
// hourly periods are consolidated into a single OCPP ChargingSchedulePeriod
// so the charger sees one continuous block — the user-facing hourly
// granularity is preserved separately in the Schedule data model.
func BuildProfile(sched *Schedule, maxPowerW float64) *types.ChargingProfile {
	periods := sched.ActivePeriods()
	if len(periods) == 0 {
		return nil
	}

	// Find the earliest start for the schedule
	earliest := periods[0].Start

	var ocppPeriods []types.ChargingSchedulePeriod
	lastEnd := earliest

	for _, p := range periods {
		// Adjacent same-power period: already covered by the previous OCPP
		// emit. Just advance lastEnd so the trailing zero-power closes at
		// the true end of the continuous block.
		if len(ocppPeriods) > 0 && p.Start.Equal(lastEnd) && p.Power == ocppPeriods[len(ocppPeriods)-1].Limit {
			lastEnd = p.End
			continue
		}

		// If there's a gap before this period, insert a zero-power period
		if p.Start.After(lastEnd) {
			startPeriod := int(lastEnd.Sub(earliest).Seconds())
			ocppPeriods = append(ocppPeriods, types.ChargingSchedulePeriod{
				StartPeriod: startPeriod,
				Limit:       0,
			})
		}

		startPeriod := int(p.Start.Sub(earliest).Seconds())
		limit := p.Power
		ocppPeriods = append(ocppPeriods, types.ChargingSchedulePeriod{
			StartPeriod: startPeriod,
			Limit:       limit,
		})
		lastEnd = p.End
	}

	// Add final zero-power period after last charge slot
	ocppPeriods = append(ocppPeriods, types.ChargingSchedulePeriod{
		StartPeriod: int(lastEnd.Sub(earliest).Seconds()),
		Limit:       0,
	})

	profileID := 1
	stackLevel := 0
	rateUnit := types.ChargingRateUnitWatts
	purpose := types.ChargingProfilePurposeTxDefaultProfile
	kind := types.ChargingProfileKindAbsolute
	validFrom := types.NewDateTime(earliest)
	validTo := types.NewDateTime(periods[len(periods)-1].End)

	return &types.ChargingProfile{
		ChargingProfileId:      profileID,
		StackLevel:             stackLevel,
		ChargingProfilePurpose: purpose,
		ChargingProfileKind:    kind,
		ValidFrom:              validFrom,
		ValidTo:                validTo,
		ChargingSchedule: &types.ChargingSchedule{
			ChargingRateUnit:       rateUnit,
			ChargingSchedulePeriod: ocppPeriods,
			StartSchedule:          types.NewDateTime(earliest),
		},
	}
}

// ProfileHash computes a deterministic hash of a ChargingProfile's schedule.
// Returns "" for nil profiles. Two profiles with identical timing and limits
// will always produce the same hash.
func ProfileHash(profile *types.ChargingProfile) string {
	if profile == nil || profile.ChargingSchedule == nil {
		return ""
	}

	var b strings.Builder
	s := profile.ChargingSchedule

	if s.StartSchedule != nil {
		fmt.Fprintf(&b, "start=%d;", s.StartSchedule.Unix())
	}
	fmt.Fprintf(&b, "unit=%s;", s.ChargingRateUnit)
	if profile.ValidFrom != nil {
		fmt.Fprintf(&b, "from=%d;", profile.ValidFrom.Unix())
	}
	if profile.ValidTo != nil {
		fmt.Fprintf(&b, "to=%d;", profile.ValidTo.Unix())
	}
	for _, p := range s.ChargingSchedulePeriod {
		fmt.Fprintf(&b, "%d:%.0f;", p.StartPeriod, p.Limit)
	}

	h := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", h[:8])
}

// ScheduleHash computes a hash based only on future active periods.
// Unlike ProfileHash, this is stable across recomputes when only past
// periods drop out — the hash only changes when future charging behavior changes.
func ScheduleHash(sched *Schedule) string {
	if sched == nil {
		return ""
	}
	now := timeNow()
	var b strings.Builder
	for _, p := range sched.ActivePeriods() {
		if p.End.After(now) {
			fmt.Fprintf(&b, "%d:%d:%.0f;", p.Start.Unix(), p.End.Unix(), p.Power)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	h := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", h[:8])
}

// IsChargeTime returns true if the current time falls within an active
// (non-cancelled) charging period [Start, End).
func IsChargeTime(sched *Schedule) bool {
	if sched == nil {
		return false
	}
	now := timeNow()
	for _, slot := range sched.Slots {
		if slot.Cancelled {
			continue
		}
		for _, p := range slot.Periods {
			if !now.Before(p.Start) && now.Before(p.End) && p.Power > 0 {
				return true
			}
		}
	}
	return false
}

// activeSlot returns a pointer to the slot whose period contains now, or nil.
// Window is [Start, End) on a non-cancelled slot with Power > 0. Used by
// recompute to detect a charging session that must not be cancelled by
// automatic recomputation — only the user can.
func activeSlot(sched *Schedule) *ScheduleSlot {
	if sched == nil {
		return nil
	}
	now := timeNow()
	for i := range sched.Slots {
		slot := &sched.Slots[i]
		if slot.Cancelled {
			continue
		}
		for _, p := range slot.Periods {
			if !now.Before(p.Start) && now.Before(p.End) && p.Power > 0 {
				return slot
			}
		}
	}
	return nil
}
