package scheduler

import (
	"time"
)

// timeNow is a package-level variable for testing.
var timeNow = time.Now

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
// automatic recomputation, only the user can.
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
