package scheduler

import (
	"errors"
	"time"

	"github.com/consi/grosz/internal/events"
)

// errStartCooldown: a RemoteStart was skipped because one is still in flight.
var errStartCooldown = errors.New("remote start cooldown")

const (
	finishingStartInterval     = 30 * time.Minute // plain RemoteStart retries while parked in Finishing
	finishingReconnectInterval = 60 * time.Minute // at most one connection drop per hour
	finishingReconnectSettle   = 15 * time.Second // let the charger resync after reconnecting
)

// recoverFinishing tries to get a Zappi out of Finishing when the plan wants
// to charge. Zappi rejects RemoteStart in Finishing until the cable is
// re-plugged, but on 2026-11-16 the first RemoteStart after the OCPP
// connection was re-established (grosz restart) was accepted without a
// re-plug. So: try a plain start; if it's rejected, drop the connection
// (the myenergi cloud proxy reconnects within a minute) and start again as
// soon as the charger is back. Each step is recorded so we can confirm the
// recovery keeps working.
func (s *Scheduler) recoverFinishing(cpID string) {
	now := time.Now()

	// Back after our reconnect (controlCharging only runs while connected).
	if s.finishingStartPending {
		if now.Sub(s.lastFinishingReconnect) < finishingReconnectSettle {
			return
		}
		s.finishingStartPending = false
		s.lastFinishingStart = now
		s.lastStartSent = time.Time{}
		err := s.sendStartForced(cpID, "schedule", false)
		result := map[string]any{"action": "finishing_recovery_start", "accepted": err == nil}
		if err != nil {
			result["error"] = err.Error()
			s.log.Warn("start after reconnect rejected, Finishing persists", "cpID", cpID, "err", err)
		} else {
			s.log.Info("start after reconnect accepted, left Finishing", "cpID", cpID)
		}
		emitControlCharging(s, err, map[string]any{"mode": "schedule", "cpID": cpID, "status": "Finishing",
			"reconnectAgoSec": int(now.Sub(s.lastFinishingReconnect).Seconds())}, result)
		return
	}

	if now.Sub(s.lastFinishingStart) < finishingStartInterval {
		return
	}
	s.lastFinishingStart = now
	s.lastStartSent = time.Time{}
	err := s.sendStartForced(cpID, "schedule", false)
	if err == nil || errors.Is(err, errStartCooldown) {
		return
	}
	if now.Sub(s.lastFinishingReconnect) < finishingReconnectInterval {
		return
	}
	s.lastFinishingReconnect = now
	if rerr := s.charger.Reconnect(cpID); rerr != nil {
		s.log.Warn("reconnect for Finishing recovery failed", "cpID", cpID, "err", rerr)
		s.events.Warn(events.ActionControlCharging,
			map[string]any{"mode": "schedule", "cpID": cpID, "status": "Finishing"},
			map[string]any{"action": "finishing_recovery_reconnect", "error": rerr.Error()})
		return
	}
	s.finishingStartPending = true
	s.log.Info("start rejected in Finishing, dropped OCPP connection to reset it", "cpID", cpID)
	s.events.Info(events.ActionControlCharging,
		map[string]any{"mode": "schedule", "cpID": cpID, "status": "Finishing"},
		map[string]any{"action": "finishing_recovery_reconnect", "startError": err.Error()})
}
