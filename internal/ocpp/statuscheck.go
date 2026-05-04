package ocpp

import (
	"log/slog"
	"sync"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
)

// statusCheckIdleInterval is how long the loop sleeps when the threshold
// setting is disabled (<=0). Tests may override.
var statusCheckIdleInterval = time.Minute

// StatusChecker periodically scans connected charge points and fires a
// TriggerMessage(StatusNotification) for any whose last_status_notification
// timestamp is older than the configured threshold. This catches the case
// where Zappi (via the myenergi cloud proxy) silently drops a status update
// after BootNotification or during long idle periods.
//
// The threshold doubles as the loop cadence — the check sleeps for
// `threshold` between scans, so the user-visible "Status Check Interval"
// setting matches its name. A per-CP cooldown of the same duration prevents
// trigger storms when Zappi stays silent.
type StatusChecker struct {
	s         *Server
	threshold func() time.Duration
	log       *slog.Logger
	stopCh    chan struct{}
	doneCh    chan struct{}

	mu       sync.Mutex
	lastFire map[string]time.Time

	// Hook used by tests to assert TriggerMessage attempts without going
	// through the real OCPP transport. If nil, the real Server.TriggerMessage
	// is called.
	triggerFn func(cpID string) error
}

// NewStatusChecker constructs a checker. threshold is read on each tick so
// settings changes take effect without a restart.
func NewStatusChecker(s *Server, threshold func() time.Duration, log *slog.Logger) *StatusChecker {
	return &StatusChecker{
		s:         s,
		threshold: threshold,
		log:       log.With("component", "ocpp-statuscheck"),
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
		lastFire:  make(map[string]time.Time),
	}
}

// Start spawns the background loop goroutine.
func (c *StatusChecker) Start() {
	go c.run()
}

// Stop signals the goroutine to exit and waits for it to finish.
func (c *StatusChecker) Stop() {
	select {
	case <-c.stopCh:
		return
	default:
	}
	close(c.stopCh)
	<-c.doneCh
}

func (c *StatusChecker) run() {
	defer close(c.doneCh)
	for {
		d := c.threshold()
		if d <= 0 {
			d = statusCheckIdleInterval
		}
		t := time.NewTimer(d)
		select {
		case <-c.stopCh:
			t.Stop()
			return
		case <-t.C:
			c.tick()
		}
	}
}

// tick scans all known charge points and fires a TriggerMessage for any
// connected connector whose last_status_notification is older than threshold.
func (c *StatusChecker) tick() {
	threshold := c.threshold()
	if threshold <= 0 {
		return
	}
	if c.s.store == nil {
		return
	}
	now := timeNow()
	cutoff := now.Add(-threshold)

	for _, cp := range c.s.ChargePoints() {
		if !cp.Connected {
			continue
		}
		// One TriggerMessage per CP is enough — Zappi will report all
		// connectors. Fire if any persisted connector for this CP is stale,
		// or if no row exists yet at all.
		anyStale := false
		anyKnown := false
		for connID := range cp.Connectors {
			cs, err := c.s.store.GetConnectorState(cp.ID, connID)
			if err != nil || cs == nil {
				anyStale = true
				continue
			}
			anyKnown = true
			if cs.LastStatusNotification.IsZero() || cs.LastStatusNotification.Before(cutoff) {
				anyStale = true
				break
			}
		}
		if !anyStale && anyKnown {
			continue
		}
		if !c.allowFire(cp.ID, now, threshold) {
			continue
		}
		c.log.Info("triggering StatusNotification — stale or unknown state", "cpID", cp.ID)
		go c.fireTrigger(cp.ID)
	}
}

// allowFire enforces a per-CP cooldown so a silent Zappi (one that never
// answers a TriggerMessage with a fresh StatusNotification) doesn't get
// re-triggered on every tick. Returns true if the caller may fire and
// records the timestamp.
func (c *StatusChecker) allowFire(cpID string, now time.Time, cooldown time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.lastFire[cpID]; ok && now.Sub(last) < cooldown {
		return false
	}
	c.lastFire[cpID] = now
	return true
}

func (c *StatusChecker) fireTrigger(cpID string) {
	var err error
	if c.triggerFn != nil {
		err = c.triggerFn(cpID)
	} else {
		err = c.s.TriggerMessage(cpID, remotetrigger.MessageTrigger(core.StatusNotificationFeatureName))
	}
	if err != nil {
		c.log.Warn("status check TriggerMessage failed", "cpID", cpID, "err", err)
	}
}
