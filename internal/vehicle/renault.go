package vehicle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/consi/grosz/internal/abrp"
	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/store"
)

// gigyaAPIKey and kamereonAPIKey are public reverse-engineered values extracted
// from the official MyRenault mobile app. They identify the client to Gigya
// (auth) and Kamereon (vehicle telemetry) and are required for any third-party
// MyRenault integration. They are not user secrets.
//
// In 2026 Renault migrated EU MyRenault auth from the shared public Gigya
// tenant (accounts.eu1.gigya.com, key 3_2YBjydYRd1...) to their own Gigya
// instance at gigya-prod-eu1.renaultgroup.com with API key 4_e9Jso4A_3lN8E33qSDMwHg.
// Accounts created/migrated via the ID Connect portal no longer resolve on the
// old tenant, so we use the new one here.
const (
	defaultGigyaURL = "https://gigya-prod-eu1.renaultgroup.com"

	defaultKamereonURL = "https://api-wired-prod-1-euw1.wrd-aws.com"
	gigyaAPIKey        = "4_e9Jso4A_3lN8E33qSDMwHg"
	kamereonAPIKey     = "YjkKtHmGfaceeuExUDKGxrLZGGvtVS0J"
)

// gigyaErrPendingTFA is the Gigya error code returned by accounts.login when
// the account has mandatory two-factor auth enabled and this device/session is
// not yet trusted. The response also carries a regToken used to drive the
// email-code completion flow (see renault_tfa.go).
const gigyaErrPendingTFA = 403101

// errRenaultRateLimited marks a Kamereon 429 ("err.func.wired.overloaded" —
// quota exceeded). Callers use errors.Is to distinguish it from other failures
// so they can log it as rate-limiting (the whole reason for the event-based
// poller) and, critically, avoid writing back partial/zeroed values learned
// from a request that never actually succeeded.
var errRenaultRateLimited = errors.New("renault api rate limited (429)")

// gigyaError is a structured Gigya API error (errorCode != 0). It lets callers
// distinguish a definitive API-level rejection (e.g. an invalid login_token, or
// pending TFA) from a transport error, and carries the regToken when present.
type gigyaError struct {
	op       string
	code     int
	message  string
	details  string // Gigya errorDetails — names the offending parameter on 400006
	regToken string
}

func (e *gigyaError) Error() string {
	if e.details != "" {
		return fmt.Sprintf("gigya %s: %s: %s (code %d)", e.op, e.message, e.details, e.code)
	}
	return fmt.Sprintf("gigya %s: %s (code %d)", e.op, e.message, e.code)
}

// CockpitStatus holds the vehicle's cockpit data (odometer).
type CockpitStatus struct {
	TotalMileage float64 `json:"totalMileage"`
}

// BatteryStatus holds the vehicle's battery state.
type BatteryStatus struct {
	Level          int     `json:"batteryLevel"`          // 0-100 %
	Autonomy       int     `json:"batteryAutonomy"`       // km
	PlugStatus     int     `json:"plugStatus"`            // 0=unplugged, 1=plugged
	ChargingStatus float64 `json:"chargingStatus"`        // 0=not charging, >0=charging
	RemainingTime  int     `json:"chargingRemainingTime"` // minutes
	Timestamp      string  `json:"timestamp"`

	// Extended fields used for ABRP telemetry. Both decode from the same
	// battery-status response and are nullable (absent on some models).
	BatteryTemperature         *float64 `json:"batteryTemperature"`         // °C
	ChargingInstantaneousPower *float64 `json:"chargingInstantaneousPower"` // W or kW (model-dependent), only while charging
}

// HvacStatus holds climate data; we use it for ambient (external) temperature.
type HvacStatus struct {
	ExternalTemperature *float64 `json:"externalTemperature"` // °C
}

// LocationStatus holds the vehicle's GPS position. Not supported on all models
// (e.g. Zoe returns 403), so callers treat it as best-effort.
type LocationStatus struct {
	GPSLatitude  float64 `json:"gpsLatitude"`
	GPSLongitude float64 `json:"gpsLongitude"`
}

// SocLevel holds the car-side charge limits exposed by the newer Kamereon "KCM"
// ev/soc-levels endpoint. SocTarget is the charge limit the vehicle enforces
// (charging stops there); SocMin is the lower bound. Only available on newer KCM
// models (e.g. Megane E-Tech, Scenic, R5); older KCA-only cars (Zoe) return
// 404/403, so callers treat it as best-effort and fall back to the local
// Fallback Target SoC setting.
type SocLevel struct {
	SocMin                    int    `json:"socMin"`
	SocTarget                 int    `json:"socTarget"`
	LastEnergyUpdateTimestamp string `json:"lastEnergyUpdateTimestamp"`
}

// Renault polls the MyRenault/Kamereon API for battery SoC.
type Renault struct {
	store       *store.Store
	events      *events.Bound
	log         *slog.Logger
	client      *http.Client
	kamereonURL string
	gigyaURL    string

	mu               sync.RWMutex
	session          string // Gigya session token (login_token), persisted
	gmid             string // Gigya trusted-device id, persisted
	ucid             string // Gigya client id, persisted
	jwt              string
	personID         string
	accountID        string
	jwtExpiry        time.Time
	last             *BatteryStatus
	onUpdate         func(int)    // called with SoC on successful poll
	abrp             *abrp.Client // optional ABRP telemetry pusher
	detailsFetchedAt time.Time
	tfa              *tfaPending // in-flight TFA verification, if any

	// Car-side charge-limit cache. Refreshed by the 30-min soc-level tier and
	// after a successful SetSocTarget write, so SetSocTarget can supply the
	// mandatory socMin in its POST body without an extra getSocLevel call.
	// Read from the web goroutine, so guarded by mu.
	socMin        int
	socTarget     int
	socLevelKnown bool // true once we've read a real (>0) socMin

	cancel    context.CancelFunc
	triggerCh chan struct{} // immediate full poll (TFA link / manual refresh)
	plugCh    chan struct{} // debounced poll after a socket transition
}

// NewRenault creates and starts a Renault SoC poller.
func NewRenault(st *store.Store, log *slog.Logger) *Renault {
	return NewRenaultWithURL(st, log, defaultKamereonURL)
}

// NewRenaultWithURL creates a Renault poller with a custom Kamereon base URL (for testing).
func NewRenaultWithURL(st *store.Store, log *slog.Logger, kamereonURL string) *Renault {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Renault{
		store:       st,
		events:      events.For(events.SourceRenault, st),
		log:         log.With("component", "renault"),
		client:      &http.Client{Timeout: 15 * time.Second},
		kamereonURL: kamereonURL,
		gigyaURL:    defaultGigyaURL,
		cancel:      cancel,
		triggerCh:   make(chan struct{}, 1),
		plugCh:      make(chan struct{}, 1),
	}
	// Restore the durable Gigya credentials so a restart reuses the existing
	// login_token (and trusted device) instead of re-running accounts.login —
	// which, under Renault's mandatory TFA, would force a fresh email-code flow.
	r.session = st.GetDefault("vehicle.renault_session", "")
	r.gmid = st.GetDefault("vehicle.renault_gmid", "")
	r.ucid = st.GetDefault("vehicle.renault_ucid", "")
	// Seed the charge-limit cache from the store so a SetSocTarget issued before
	// the first soc-level poll can still preserve the car's minimum.
	r.socMin = st.GetInt("vehicle.soc_min", 0)
	r.socTarget = st.GetInt("vehicle.soc_target", 0)
	r.socLevelKnown = r.socMin > 0
	go r.loop(ctx)
	return r
}

// Stop shuts down the poller.
func (r *Renault) Stop() { r.cancel() }

// Trigger asks the poll loop to run a full poll immediately, breaking out of its
// inter-poll sleep. Non-blocking; coalesces multiple calls into one poll. Used
// by the TFA flow once a trusted-device login succeeds.
func (r *Renault) Trigger() {
	select {
	case r.triggerCh <- struct{}{}:
	default:
	}
}

// SchedulePlugPoll asks the loop to poll shortly (plugPollDelay) after a socket
// transition — plug, unplug, or charge-complete. Unlike Trigger it does not poll
// immediately: the short settle delay lets the Kamereon API reflect the new plug
// state before we read it, and the poll resets the regular SoC cadence.
// Non-blocking; coalesces a burst of transitions into a single poll.
func (r *Renault) SchedulePlugPoll() {
	select {
	case r.plugCh <- struct{}{}:
	default:
	}
}

// plugPollDelay is how long we wait after a socket transition before polling, so
// the car/API has time to report the new plug state (an immediate read usually
// still returns the pre-event value).
const plugPollDelay = time.Minute

// socLevelBackoff is how long we stop calling the strict-quota KCM soc-level
// endpoint after it returns 429. Retrying against an exhausted quota only keeps
// it exhausted; backing off gives it room to recover so we can read the real
// socMin/socTarget. Store-backed so it survives restarts.
const socLevelBackoff = time.Hour

// socLevelRefresh is how often we re-read the (quasi-static) car-side charge
// limits once known. socMin is one-time car config and socTarget only changes on
// a user action we already write through, so we read once and then refresh
// rarely — just enough to catch a change made directly in MyRenault — instead of
// polling this strict-quota endpoint every slow-tier cycle (the old redundancy
// that tripped the 429).
const socLevelRefresh = 6 * time.Hour

// pollInterval is the SoC cadence while the car is plugged in (charging or ready
// to charge), where a fresh reading is most useful.
func (r *Renault) pollInterval() time.Duration {
	m := r.store.GetInt("vehicle.poll_interval", 15)
	if m < 1 {
		m = 1
	}
	return time.Duration(m) * time.Minute
}

// pollIntervalUnplugged is the (slower) SoC cadence while unplugged — the car
// can't be charged, so frequent polling would only burn rate-limited API calls.
// A plug event still polls within plugPollDelay regardless.
func (r *Renault) pollIntervalUnplugged() time.Duration {
	m := r.store.GetInt("vehicle.poll_interval_unplugged", 30)
	if m < 1 {
		m = 1
	}
	return time.Duration(m) * time.Minute
}

// socLevelInterval is the cadence of the slow tier (charge limits, odometer, and
// ABRP climate/GPS), piggybacked onto a battery poll.
func (r *Renault) socLevelInterval() time.Duration {
	m := r.store.GetInt("vehicle.soc_level_interval", 30)
	if m < 1 {
		m = 1
	}
	return time.Duration(m) * time.Minute
}

// nextInterval picks the SoC cadence based on the last known plug state.
func (r *Renault) nextInterval(plugged bool) time.Duration {
	if plugged {
		return r.pollInterval()
	}
	return r.pollIntervalUnplugged()
}

// socLevelBackedOff reports whether we're within a post-429 cooldown for the KCM
// soc-level endpoint. Store-backed so the backoff survives restarts — otherwise
// every boot Tier-B poll would fire a fresh call at the throttled endpoint and
// keep the quota pinned.
func (r *Renault) socLevelBackedOff() bool {
	ts := r.store.GetDefault("vehicle.soc_level_backoff_until", "")
	if ts == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, ts)
	return err == nil && time.Now().Before(until)
}

// shouldFetchSocLevel reports whether the car-side charge limits are due for a
// read: either we've never learned socMin, or the last successful read is older
// than socLevelRefresh. This keeps us off the strict-quota KCM endpoint on the
// common path — the values almost never change, and our own writes update them.
func (r *Renault) shouldFetchSocLevel() bool {
	if _, known := r.cachedSocMin(); !known {
		return true
	}
	ts := r.store.GetDefault("vehicle.soc_level_read_at", "")
	if ts == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true
	}
	return time.Since(t) >= socLevelRefresh
}

// cachedSocMin returns the cached car-side minimum charge limit and whether it
// has been learned from a real read.
func (r *Renault) cachedSocMin() (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.socMin, r.socLevelKnown
}

// setSocLevelCache stores freshly-read charge limits. Zero values are ignored so
// a partial response never blanks a known limit (mirrors the store writes).
func (r *Renault) setSocLevelCache(socMin, socTarget int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if socMin > 0 {
		r.socMin = socMin
		r.socLevelKnown = true
	}
	if socTarget > 0 {
		r.socTarget = socTarget
	}
}

// setCachedSocTarget reflects a just-written target in the cache so the next
// write/read sees it without waiting for the next soc-level tier.
func (r *Renault) setCachedSocTarget(target int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if target > 0 {
		r.socTarget = target
	}
}

// Last returns the most recent battery status.
func (r *Renault) Last() *BatteryStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.last
}

// SetOnUpdate registers a callback invoked with SoC after each successful poll.
func (r *Renault) SetOnUpdate(fn func(int)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onUpdate = fn
}

// SetABRP registers the ABRP telemetry client used to forward vehicle data
// after each successful poll. Optional — a nil client disables ABRP pushing.
func (r *Renault) SetABRP(c *abrp.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.abrp = c
}

// loop drives the tiered, event-based poller. It keeps a single reusable timer
// and recomputes the next wake each iteration as the earliest of the scheduled
// SoC poll (nextPollDue) and any pending post-event poll (plugPollAt). A socket
// transition (plugCh) arms a short debounced poll; Trigger (triggerCh) forces an
// immediate full poll. Any poll resets the SoC cadence.
func (r *Renault) loop(ctx context.Context) {
	var cache abrpCache       // best-effort ABRP telemetry, refreshed by the slow tier
	var lastTierB time.Time   // last time the slow tier (soc-level/cockpit/…) ran
	var plugPollAt time.Time  // pending post-event poll; zero => none
	nextPollDue := time.Now() // boot: poll immediately, in full
	// Drives the SoC cadence; only updated on a successful poll so a transient
	// failure retries at the last-known cadence (e.g. 15 min while charging)
	// rather than slowing to the unplugged interval. Seeded from the store so a
	// restart mid-charge resumes the fast cadence.
	lastPlugged := r.store.GetDefault("vehicle.plug_status", "") == "1"

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		user := r.store.GetDefault("vehicle.renault_user", "")
		pass := r.store.GetDefault("vehicle.renault_password", "")
		vin := r.store.GetDefault("vehicle.vin", "")

		if user == "" || pass == "" || vin == "" {
			// Not configured yet: wait cheaply, but stay responsive to creds
			// being entered (Trigger) and drain any queued socket event.
			select {
			case <-ctx.Done():
				return
			case <-r.triggerCh:
			case <-r.plugCh:
			case <-time.After(30 * time.Second):
			}
			continue
		}

		// Next wake = earliest of the scheduled poll and a pending event poll.
		wake := nextPollDue
		if !plugPollAt.IsZero() && plugPollAt.Before(wake) {
			wake = plugPollAt
		}
		d := time.Until(wake)
		if d < 0 {
			d = 0
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)

		var doPoll, forceTierB bool
		var reason string
		select {
		case <-ctx.Done():
			return
		case <-r.triggerCh:
			// Immediate full poll (TFA link / manual refresh).
			doPoll, forceTierB, reason = true, true, "trigger"
		case <-r.plugCh:
			// Socket transition: confirm state after a short settle delay. The
			// poll itself happens on a later timer wake.
			plugPollAt = time.Now().Add(plugPollDelay)
		case <-timer.C:
			now := time.Now()
			switch {
			case !plugPollAt.IsZero() && !now.Before(plugPollAt):
				doPoll, reason = true, "socket-event"
			case !now.Before(nextPollDue):
				doPoll, reason = true, "scheduled"
			}
		}

		if !doPoll {
			continue
		}

		includeTierB := forceTierB || lastTierB.IsZero() || time.Since(lastTierB) >= r.socLevelInterval()
		plugged, err := r.pollOnce(user, pass, vin, includeTierB, reason, &cache)
		if err == nil {
			lastPlugged = plugged
			if includeTierB {
				lastTierB = time.Now()
			}
		}
		plugPollAt = time.Time{}
		nextPollDue = time.Now().Add(r.nextInterval(lastPlugged))
	}
}

// abrpCache holds the best-effort telemetry refreshed by the slow (soc-level)
// tier and reused on every ABRP push, so a battery-only poll still forwards
// recent climate/GPS/odometer instead of re-fetching them each cycle.
type abrpCache struct {
	hvac       *HvacStatus
	loc        *LocationStatus
	odometerKm float64
}

// pollOnce runs one poll cycle. Tier A (battery-status) always runs. The slow
// tier (soc-level, cockpit, and — when ABRP is on — hvac/location) runs only
// when includeTierB, refreshing the in-memory caches. reason labels the trigger
// in the system event. It returns whether the car is plugged, which sets the
// cadence for the next scheduled poll.
func (r *Renault) pollOnce(user, pass, vin string, includeTierB bool, reason string, cache *abrpCache) (bool, error) {
	// Authenticate if needed
	if err := r.ensureAuth(user, pass); err != nil {
		err = fmt.Errorf("auth: %w", err)
		r.recordPollFailure(vin, reason, err)
		return false, err
	}

	// Get battery status
	status, err := r.getBatteryStatus(vin)
	if err != nil {
		// JWT might be expired, re-auth and retry
		r.mu.Lock()
		r.jwt = ""
		r.mu.Unlock()
		if err := r.ensureAuth(user, pass); err != nil {
			err = fmt.Errorf("re-auth: %w", err)
			r.recordPollFailure(vin, reason, err)
			return false, err
		}
		status, err = r.getBatteryStatus(vin)
		if err != nil {
			err = fmt.Errorf("battery status: %w", err)
			r.recordPollFailure(vin, reason, err)
			return false, err
		}
	}

	r.mu.Lock()
	r.last = status
	r.mu.Unlock()

	// Update SoC, plug status, and battery details in store
	_ = r.store.Set("vehicle.plug_status", fmt.Sprintf("%d", status.PlugStatus))
	_ = r.store.Set("vehicle.battery_autonomy", fmt.Sprintf("%d", status.Autonomy))
	_ = r.store.Set("vehicle.charging_status", fmt.Sprintf("%g", status.ChargingStatus))
	_ = r.store.Set("vehicle.charging_remaining_time", fmt.Sprintf("%d", status.RemainingTime))
	_ = r.store.Set("vehicle.battery_timestamp", status.Timestamp)
	if status.Level >= 0 && status.Level <= 100 {
		_ = r.store.Set("scheduler.current_soc", fmt.Sprintf("%d", status.Level))
		r.log.Info("SoC updated from Renault",
			"soc", status.Level,
			"autonomy", status.Autonomy,
			"plugged", status.PlugStatus == 1,
			"reason", reason,
			"tierB", includeTierB,
		)
	}

	// Fetch vehicle details (model name + image), refresh daily
	if r.shouldRefreshDetails() {
		r.fetchVehicleDetails(vin)
	}

	// Slow tier: charge limits, odometer, and ABRP climate/GPS. Best-effort.
	if includeTierB {
		r.pollTierB(vin, cache)
	}

	r.mu.RLock()
	cb := r.onUpdate
	socMin, socTarget := r.socMin, r.socTarget
	r.mu.RUnlock()
	if cb != nil {
		cb(status.Level)
	}

	vinSuffix := vin
	if len(vinSuffix) > 4 {
		vinSuffix = vinSuffix[len(vinSuffix)-4:]
	}
	r.events.Info(events.ActionRenaultPoll,
		map[string]any{"vin": vinSuffix, "reason": reason},
		map[string]any{
			"soc":            status.Level,
			"autonomy":       status.Autonomy,
			"plugStatus":     status.PlugStatus,
			"chargingStatus": status.ChargingStatus,
			"totalMileage":   cache.odometerKm,
			"tierB":          includeTierB,
			"socMin":         socMin,
			"socTarget":      socTarget,
		},
	)

	// A successful poll proves auth is healthy — clear any stale TFA flag.
	if r.store.GetBool("vehicle.renault_tfa_required", false) {
		_ = r.store.Set("vehicle.renault_tfa_required", "false")
	}

	// Forward telemetry to ABRP (no-op unless a token is configured) from the
	// cache — best-effort, so a battery-only poll still pushes recent GPS/temp.
	r.mu.RLock()
	ac := r.abrp
	r.mu.RUnlock()
	if ac != nil && ac.Enabled() {
		r.pushABRP(ac, status, cache.hvac, cache.loc, cache.odometerKm)
	}

	return status.PlugStatus == 1, nil
}

// pollTierB refreshes the slow-changing group: the car-side charge limits
// (socMin/socTarget), the odometer, and — when ABRP is enabled — the climate and
// GPS caches used for telemetry. All best-effort: an unsupported endpoint (older
// KCA models) or a transient error simply leaves the previous cached values.
func (r *Renault) pollTierB(vin string, cache *abrpCache) {
	// Car-side charge limit (newer KCM models only). Cached so SetSocTarget can
	// supply the mandatory socMin without its own read; the scheduler/dashboard
	// keep reading the store keys and fall back to the local target for KCA cars.
	switch {
	case !r.shouldFetchSocLevel():
		r.log.Debug("soc-level fetch skipped (cached, not due for refresh)")
	case r.socLevelBackedOff():
		r.log.Debug("soc-level fetch skipped (429 backoff active)")
	default:
		if lvl, err := r.getSocLevel(vin); err != nil {
			if errors.Is(err, errRenaultRateLimited) {
				// Surface rate-limiting explicitly (it's the signal we're driving
				// to zero) and pause soc-level calls so we stop pinning the quota.
				until := time.Now().Add(socLevelBackoff)
				_ = r.store.Set("vehicle.soc_level_backoff_until", until.UTC().Format(time.RFC3339))
				r.log.Warn("soc-level rate limited by Renault (429); backing off",
					"until", until.Format(time.RFC3339), "err", err)
				r.events.Warn(events.ActionRenaultPoll,
					map[string]any{"endpoint": "soc-level"},
					map[string]any{"rateLimited": true, "backoffMinutes": int(socLevelBackoff.Minutes())},
				)
			} else {
				r.log.Debug("soc-level unavailable", "err", err)
			}
		} else {
			_ = r.store.Delete("vehicle.soc_level_backoff_until")
			_ = r.store.Set("vehicle.soc_level_read_at", time.Now().UTC().Format(time.RFC3339))
			if lvl.SocTarget > 0 {
				_ = r.store.Set("vehicle.soc_target", fmt.Sprintf("%d", lvl.SocTarget))
			}
			if lvl.SocMin > 0 {
				_ = r.store.Set("vehicle.soc_min", fmt.Sprintf("%d", lvl.SocMin))
			}
			r.setSocLevelCache(lvl.SocMin, lvl.SocTarget)
			r.log.Info("soc-level read from Renault", "socMin", lvl.SocMin, "socTarget", lvl.SocTarget)
		}
	}

	// Odometer.
	if cockpit, err := r.getCockpitStatus(vin); err != nil {
		r.log.Debug("cockpit data unavailable", "err", err)
	} else if cockpit != nil && cockpit.TotalMileage > 0 {
		cache.odometerKm = cockpit.TotalMileage
		if err := r.store.InsertOdometerReading(store.OdometerReading{
			Timestamp: time.Now(),
			Mileage:   cockpit.TotalMileage,
		}); err != nil {
			r.log.Warn("failed to store odometer reading", "err", err)
		}
	}

	// Climate + GPS are only consumed by ABRP: skip the calls entirely when it's
	// off, otherwise refresh the cache for subsequent battery-only pushes.
	r.mu.RLock()
	ac := r.abrp
	r.mu.RUnlock()
	if ac != nil && ac.Enabled() {
		if hvac, err := r.getHvacStatus(vin); err != nil {
			r.log.Debug("abrp: hvac-status unavailable", "err", err)
		} else {
			cache.hvac = hvac
		}
		if loc, err := r.getLocation(vin); err != nil {
			r.log.Debug("abrp: location unavailable", "err", err)
		} else {
			cache.loc = loc
		}
	}
}

// recordPollFailure logs and records a failed poll without disturbing the last
// good store values. A transient failure (e.g. a one-minute Gigya hiccup) used
// to reset SoC to 0, which made the scheduler plan a panic-sized slot anchored
// at the current expensive hour; the merge logic then preserved that pulled-back
// start even after SoC recovered. Leaving the store untouched lets the next
// successful poll overwrite naturally, and the plug-check stale_data guard
// already handles prolonged outages.
func (r *Renault) recordPollFailure(vin, reason string, err error) {
	r.log.Warn("renault poll failed", "err", err, "reason", reason)
	vinSuffix := vin
	if len(vinSuffix) > 4 {
		vinSuffix = vinSuffix[len(vinSuffix)-4:]
	}
	r.events.Warn(events.ActionRenaultPoll,
		map[string]any{"vin": vinSuffix, "reason": reason},
		map[string]any{"error": err.Error(), "socPreserved": true},
	)
}

func (r *Renault) ensureAuth(user, pass string) error {
	r.mu.RLock()
	hasJWT := r.jwt != "" && time.Now().Before(r.jwtExpiry)
	hasAccount := r.accountID != ""
	r.mu.RUnlock()

	if hasJWT && hasAccount {
		return nil
	}

	// Step 1: Gigya login
	r.mu.RLock()
	hasSession := r.session != ""
	r.mu.RUnlock()

	if !hasSession {
		session, err := r.gigyaLogin(user, pass)
		if err != nil {
			// A 403101 means the account now requires TFA and this device is
			// not trusted. Flag it so the UI can surface the email-code flow;
			// the background poll can't complete it (needs a code from the user).
			var ge *gigyaError
			if errors.As(err, &ge) && ge.code == gigyaErrPendingTFA {
				_ = r.store.Set("vehicle.renault_tfa_required", "true")
			}
			return err
		}
		r.mu.Lock()
		r.session = session
		r.mu.Unlock()
		_ = r.store.Set("vehicle.renault_session", session)
	}

	r.mu.RLock()
	session := r.session
	r.mu.RUnlock()

	// Step 2: Get person ID
	r.mu.RLock()
	hasPerson := r.personID != ""
	r.mu.RUnlock()

	if !hasPerson {
		personID, err := r.gigyaGetPersonID(session)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.personID = personID
		r.mu.Unlock()
	}

	// Step 3: Get JWT
	if !hasJWT {
		jwt, err := r.gigyaGetJWT(session)
		if err != nil {
			// Session expired, clear and fail.
			r.mu.Lock()
			r.session = ""
			r.mu.Unlock()
			// Only drop the persisted login_token when Gigya positively rejected
			// it (a structured API error), not on a transient transport error —
			// otherwise a network blip would force an unnecessary fresh login and
			// re-trigger TFA. A fresh login next cycle re-mints (and may need TFA).
			var ge *gigyaError
			if errors.As(err, &ge) {
				_ = r.store.Delete("vehicle.renault_session")
			}
			return err
		}
		r.mu.Lock()
		r.jwt = jwt
		r.jwtExpiry = time.Now().Add(50 * time.Minute) // JWT typically valid ~1h
		r.mu.Unlock()
	}

	// Step 4: Get account ID
	if !hasAccount {
		r.mu.RLock()
		personID := r.personID
		jwt := r.jwt
		r.mu.RUnlock()

		accountID, err := r.getAccountID(personID, jwt)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.accountID = accountID
		r.mu.Unlock()
	}

	return nil
}

func (r *Renault) gigyaLogin(user, pass string) (string, error) {
	form := url.Values{
		"apiKey":   {gigyaAPIKey},
		"loginID":  {user},
		"password": {pass},
	}
	// Present the trusted-device context (set once via the TFA flow) so Gigya
	// recognises this device and skips TFA for the ~30-day trust window.
	r.addDevice(form)

	resp, err := r.client.PostForm(r.gigyaURL+"/accounts.login", form)
	if err != nil {
		return "", fmt.Errorf("gigya login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		SessionInfo struct {
			CookieValue string `json:"cookieValue"`
		} `json:"sessionInfo"`
		RegToken     string `json:"regToken"`
		ErrorCode    int    `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
		ErrorDetails string `json:"errorDetails"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("gigya login decode: %w", err)
	}
	if result.ErrorCode != 0 {
		return "", &gigyaError{op: "login", code: result.ErrorCode, message: result.ErrorMessage, details: result.ErrorDetails, regToken: result.RegToken}
	}
	if result.SessionInfo.CookieValue == "" {
		return "", fmt.Errorf("gigya login: empty session token")
	}

	r.log.Debug("gigya login ok")
	return result.SessionInfo.CookieValue, nil
}

func (r *Renault) gigyaGetPersonID(session string) (string, error) {
	form := url.Values{
		"ApiKey":      {gigyaAPIKey},
		"login_token": {session},
	}
	resp, err := r.client.PostForm(r.gigyaURL+"/accounts.getAccountInfo", form)
	if err != nil {
		return "", fmt.Errorf("gigya getAccountInfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Data struct {
			PersonID string `json:"personId"`
		} `json:"data"`
		ErrorCode int `json:"errorCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("gigya getAccountInfo decode: %w", err)
	}
	if result.ErrorCode != 0 || result.Data.PersonID == "" {
		return "", fmt.Errorf("gigya getAccountInfo: no personId (code %d)", result.ErrorCode)
	}

	r.log.Debug("got person ID", "personId", result.Data.PersonID)
	return result.Data.PersonID, nil
}

func (r *Renault) gigyaGetJWT(session string) (string, error) {
	form := url.Values{
		"ApiKey":      {gigyaAPIKey},
		"login_token": {session},
		"fields":      {"data.personId,data.gigyaDataCenter"},
		"expiration":  {"3600"},
	}
	resp, err := r.client.PostForm(r.gigyaURL+"/accounts.getJWT", form)
	if err != nil {
		return "", fmt.Errorf("gigya getJWT: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		IDToken      string `json:"id_token"`
		ErrorCode    int    `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
		ErrorDetails string `json:"errorDetails"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("gigya getJWT decode: %w", err)
	}
	if result.ErrorCode != 0 || result.IDToken == "" {
		msg := result.ErrorMessage
		if msg == "" {
			msg = "no token"
		}
		return "", &gigyaError{op: "getJWT", code: result.ErrorCode, message: msg, details: result.ErrorDetails}
	}

	r.log.Debug("got JWT")
	return result.IDToken, nil
}

func (r *Renault) getAccountID(personID, jwt string) (string, error) {
	reqURL := fmt.Sprintf("%s/commerce/v1/persons/%s?country=PL", r.kamereonURL, personID)
	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("x-gigya-id_token", jwt)
	req.Header.Set("apikey", kamereonAPIKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get accounts: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Accounts []struct {
			AccountID   string `json:"accountId"`
			AccountType string `json:"accountType"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("get accounts decode: %w", err)
	}
	if len(result.Accounts) == 0 {
		return "", fmt.Errorf("no accounts found")
	}

	// Prefer MYRENAULT account (has vehicle links with images), fall back to first
	accountID := result.Accounts[0].AccountID
	for _, a := range result.Accounts {
		if a.AccountType == "MYRENAULT" {
			accountID = a.AccountID
			break
		}
	}
	r.log.Debug("got account ID", "accountId", accountID)
	return accountID, nil
}

func (r *Renault) getBatteryStatus(vin string) (*BatteryStatus, error) {
	r.mu.RLock()
	accountID := r.accountID
	jwt := r.jwt
	r.mu.RUnlock()

	vin = strings.ToUpper(vin)
	reqURL := fmt.Sprintf("%s/commerce/v1/accounts/%s/kamereon/kca/car-adapter/v2/cars/%s/battery-status?country=PL",
		r.kamereonURL, accountID, vin)

	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("x-gigya-id_token", jwt)
	req.Header.Set("apikey", kamereonAPIKey)
	req.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("battery status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("battery status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			Attributes BatteryStatus `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("battery status decode: %w", err)
	}

	return &result.Data.Attributes, nil
}

// getSocLevel reads the car's charge limits from the newer Kamereon KCM
// endpoint. The resource is spelled plural (ev/soc-levels) on current gateways —
// the singular ev/soc-level 404s ("url does not exist") — so we try plural first
// and fall back to the singular spelling on a 404, mirroring putSocLevel. Best
// effort: unsupported on older (KCA-only) models (404/403), so the caller falls
// back to the local Fallback Target SoC setting.
func (r *Renault) getSocLevel(vin string) (*SocLevel, error) {
	r.mu.RLock()
	accountID := r.accountID
	jwt := r.jwt
	r.mu.RUnlock()

	vin = strings.ToUpper(vin)

	var lastErr error
	for _, path := range []string{"ev/soc-levels", "ev/soc-level"} {
		reqURL := fmt.Sprintf("%s/commerce/v1/accounts/%s/kamereon/kcm/v1/vehicles/%s/%s?country=PL",
			r.kamereonURL, accountID, vin, path)

		req, _ := http.NewRequest("GET", reqURL, nil)
		req.Header.Set("x-gigya-id_token", jwt)
		req.Header.Set("apikey", kamereonAPIKey)
		req.Header.Set("Content-Type", "application/vnd.api+json")

		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("soc-level: %w", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: %s", errRenaultRateLimited, string(body))
		}
		if resp.StatusCode == 200 {
			return parseSocLevel(body)
		}
		lastErr = fmt.Errorf("soc-level %d: %s", resp.StatusCode, string(body))
		// Only a missing-URL 404 is worth retrying on the other spelling;
		// 403 (entitlement) / 5xx are terminal for this VIN.
		if resp.StatusCode != http.StatusNotFound {
			break
		}
	}
	return nil, lastErr
}

// parseSocLevel decodes a KCM charge-limit response. Unlike the KCA car-adapter
// endpoints (which wrap payloads in data.attributes), this response is a bare
// top-level object {"socMin":20,"socTarget":80,"lastEnergyUpdateTimestamp":".."}
// (per hacf-fr/renault-api docs). Parse that; fall back to the wrapped shape for
// any gateway that still returns data.attributes.
func parseSocLevel(body []byte) (*SocLevel, error) {
	var flat SocLevel
	if err := json.Unmarshal(body, &flat); err != nil {
		return nil, fmt.Errorf("soc-level decode: %w", err)
	}
	if flat.SocMin == 0 && flat.SocTarget == 0 {
		var wrapped struct {
			Data struct {
				Attributes SocLevel `json:"attributes"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapped); err == nil &&
			(wrapped.Data.Attributes.SocMin != 0 || wrapped.Data.Attributes.SocTarget != 0) {
			return &wrapped.Data.Attributes, nil
		}
	}
	return &flat, nil
}

// SetSocTarget writes the car-side charge limit (socTarget, a percentage) via
// the KCM ev/soc-level endpoint, preserving the car's current socMin from the
// in-memory cache so the write costs no extra read. The caller is responsible
// for clamping target to a sensible range (30–100). Only supported on newer KCM
// models; older cars return an error and the caller keeps the local fallback.
//
// The write is a POST to the (plural) ev/soc-levels resource with a bare
// {"socMin":..,"socTarget":..} body — NOT a JSON:API data/attributes envelope.
// This matches hacf-fr/renault-api's set_battery_soc and real KCM owners'
// captures; a successful write returns 200 {"type":"SOC_SYNCH",...}. We retry
// the singular spelling if a VIN's gateway only routes ev/soc-level.
func (r *Renault) SetSocTarget(target int) (err error) {
	// Record the outcome under the renault source — it's a Renault API action, not
	// a web action. One event per call (success or failure) via the named return.
	defer func() {
		if err != nil {
			r.events.Warn(events.ActionRenaultSocTarget,
				map[string]any{"target": target},
				map[string]any{"error": err.Error()},
			)
		} else {
			r.events.Info(events.ActionRenaultSocTarget,
				map[string]any{"target": target}, nil,
			)
		}
	}()

	user := r.store.GetDefault("vehicle.renault_user", "")
	pass := r.store.GetDefault("vehicle.renault_password", "")
	vin := r.store.GetDefault("vehicle.vin", "")
	if user == "" || pass == "" || vin == "" {
		return fmt.Errorf("renault credentials not configured")
	}
	if err := r.ensureAuth(user, pass); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Preserve the car's current minimum (a POST must carry both socMin and
	// socTarget). Use the cached value refreshed by the soc-level tier so we
	// don't add an API call to every write; fall back to the durable stored
	// value, then to a single live read only if we've never learned it.
	socMin, known := r.cachedSocMin()
	if !known {
		if stored := r.store.GetInt("vehicle.soc_min", 0); stored > 0 {
			socMin, known = stored, true
		} else if lvl, err := r.getSocLevel(vin); err == nil && lvl.SocMin > 0 {
			socMin, known = lvl.SocMin, true
			r.setSocLevelCache(lvl.SocMin, lvl.SocTarget)
		}
	}
	// Refuse to write a fabricated minimum. The POST carries both fields, so an
	// unknown (0) socMin would overwrite the car's real floor with 0. socMin is
	// unknown when the soc-level GET has only ever been rate-limited (429) — the
	// user can retry once the quota recovers, or once a background poll reads it.
	if !known || socMin <= 0 {
		return fmt.Errorf("charge limit not changed: the car's minimum SoC isn't known yet " +
			"(Renault soc-level API is rate-limited); please try again in a few minutes")
	}

	if err := r.putSocLevel(vin, socMin, target); err != nil {
		// JWT might be expired; re-auth once and retry, mirroring pollOnce().
		r.mu.Lock()
		r.jwt = ""
		r.mu.Unlock()
		if err2 := r.ensureAuth(user, pass); err2 != nil {
			return fmt.Errorf("re-auth: %w", err2)
		}
		if err2 := r.putSocLevel(vin, socMin, target); err2 != nil {
			return err2
		}
	}

	_ = r.store.Set("vehicle.soc_target", fmt.Sprintf("%d", target))
	if socMin > 0 {
		_ = r.store.Set("vehicle.soc_min", fmt.Sprintf("%d", socMin))
	}
	// Reflect the just-written target in the cache so the next scheduler read and
	// any subsequent write see it without waiting for the next soc-level tier.
	r.setCachedSocTarget(target)
	r.log.Info("charge limit set on Renault", "socTarget", target, "socMin", socMin)
	return nil
}

func (r *Renault) putSocLevel(vin string, socMin, socTarget int) error {
	r.mu.RLock()
	accountID := r.accountID
	jwt := r.jwt
	r.mu.RUnlock()

	vin = strings.ToUpper(vin)

	// Bare body — the KCM ev/soc-levels write is plain JSON, not a JSON:API
	// data/attributes envelope. Both fields are mandatory.
	body, _ := json.Marshal(map[string]int{
		"socMin":    socMin,
		"socTarget": socTarget,
	})

	// Prefer the plural resource (the proven hacf-fr/renault-api contract); fall
	// back to the singular spelling only on a routing 404.
	var lastErr error
	for _, path := range []string{"ev/soc-levels", "ev/soc-level"} {
		reqURL := fmt.Sprintf("%s/commerce/v1/accounts/%s/kamereon/kcm/v1/vehicles/%s/%s?country=PL",
			r.kamereonURL, accountID, vin, path)

		req, _ := http.NewRequest("POST", reqURL, bytes.NewReader(body))
		req.Header.Set("x-gigya-id_token", jwt)
		req.Header.Set("apikey", kamereonAPIKey)
		req.Header.Set("Content-Type", "application/vnd.api+json")

		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("set soc-level: %w", err)
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()

		if resp.StatusCode == 200 || resp.StatusCode == 204 {
			return nil
		}
		lastErr = fmt.Errorf("set soc-level %d: %s", resp.StatusCode, string(respBody))
		// Only a missing-URL 404 is worth retrying on the other spelling;
		// 400 (range) / 403 (entitlement) are terminal for this VIN.
		if resp.StatusCode != http.StatusNotFound {
			break
		}
	}
	return lastErr
}

func (r *Renault) getCockpitStatus(vin string) (*CockpitStatus, error) {
	r.mu.RLock()
	accountID := r.accountID
	jwt := r.jwt
	r.mu.RUnlock()

	vin = strings.ToUpper(vin)

	// Try v2 first, fall back to v1
	for _, version := range []string{"v2", "v1"} {
		reqURL := fmt.Sprintf("%s/commerce/v1/accounts/%s/kamereon/kca/car-adapter/%s/cars/%s/cockpit?country=PL",
			r.kamereonURL, accountID, version, vin)

		req, _ := http.NewRequest("GET", reqURL, nil)
		req.Header.Set("x-gigya-id_token", jwt)
		req.Header.Set("apikey", kamereonAPIKey)
		req.Header.Set("Content-Type", "application/vnd.api+json")

		resp, err := r.client.Do(req)
		if err != nil {
			continue
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != 200 {
			_, _ = io.ReadAll(io.LimitReader(resp.Body, 256))
			continue
		}

		var result struct {
			Data struct {
				Attributes CockpitStatus `json:"attributes"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			continue
		}

		if result.Data.Attributes.TotalMileage > 0 {
			return &result.Data.Attributes, nil
		}
		// Response decoded but has no mileage data — try next version
	}

	return nil, fmt.Errorf("cockpit unavailable on v1 and v2")
}

// getHvacStatus fetches climate data (we use ambient/external temperature) from
// the car-adapter hvac-status endpoint. Best-effort: callers ignore errors.
func (r *Renault) getHvacStatus(vin string) (*HvacStatus, error) {
	r.mu.RLock()
	accountID := r.accountID
	jwt := r.jwt
	r.mu.RUnlock()

	vin = strings.ToUpper(vin)
	reqURL := fmt.Sprintf("%s/commerce/v1/accounts/%s/kamereon/kca/car-adapter/v1/cars/%s/hvac-status?country=PL",
		r.kamereonURL, accountID, vin)

	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("x-gigya-id_token", jwt)
	req.Header.Set("apikey", kamereonAPIKey)
	req.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hvac status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("hvac status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			Attributes HvacStatus `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("hvac status decode: %w", err)
	}

	return &result.Data.Attributes, nil
}

// getLocation fetches the vehicle's GPS position. Not supported on all models
// (e.g. Zoe returns 403), so callers treat it as best-effort.
func (r *Renault) getLocation(vin string) (*LocationStatus, error) {
	r.mu.RLock()
	accountID := r.accountID
	jwt := r.jwt
	r.mu.RUnlock()

	vin = strings.ToUpper(vin)
	reqURL := fmt.Sprintf("%s/commerce/v1/accounts/%s/kamereon/kca/car-adapter/v1/cars/%s/location?country=PL",
		r.kamereonURL, accountID, vin)

	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("x-gigya-id_token", jwt)
	req.Header.Set("apikey", kamereonAPIKey)
	req.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("location: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("location %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			Attributes LocationStatus `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("location decode: %w", err)
	}

	return &result.Data.Attributes, nil
}

// buildABRPTelemetry assembles an ABRP telemetry frame from a battery-status
// response plus best-effort hvac/location data (either may be nil). It is a pure
// function (no I/O) so the field-mapping logic stays unit-testable.
func buildABRPTelemetry(status *BatteryStatus, hvac *HvacStatus, loc *LocationStatus, capacityKwh, odometerKm float64) abrp.Telemetry {
	f := func(v float64) *float64 { return &v }

	charging := status.ChargingStatus == 1.0
	tlm := abrp.Telemetry{
		UTC: time.Now().Unix(),
		SoC: float64(status.Level),
	}
	if charging {
		tlm.IsCharging = 1
	}

	// is_parked: plugged in or charging implies the car is stationary. Renault
	// exposes no ignition/speed signal, so we only assert parked when confident
	// and otherwise leave the field absent.
	if status.PlugStatus == 1 || charging {
		one := 1
		tlm.IsParked = &one
	}

	// Charging power. Renault reports chargingInstantaneousPower in W on some
	// models and kW on others; auto-detect by magnitude (no EV draws >=1 MW, and
	// sub-kW "charging" is negligible). ABRP expects negative power for charging.
	if charging && status.ChargingInstantaneousPower != nil {
		p := *status.ChargingInstantaneousPower
		if p >= 1000 {
			p /= 1000 // W → kW
		}
		tlm.Power = f(-p)
	}

	if status.Autonomy > 0 {
		tlm.EstRange = f(float64(status.Autonomy))
	}
	if capacityKwh > 0 {
		tlm.Capacity = f(capacityKwh)
	}
	if status.BatteryTemperature != nil {
		tlm.BattTemp = f(*status.BatteryTemperature)
	}
	if hvac != nil && hvac.ExternalTemperature != nil {
		tlm.ExtTemp = f(*hvac.ExternalTemperature)
	}
	if loc != nil && (loc.GPSLatitude != 0 || loc.GPSLongitude != 0) {
		tlm.Lat = f(loc.GPSLatitude)
		tlm.Lon = f(loc.GPSLongitude)
	}
	if odometerKm > 0 {
		tlm.Odometer = f(odometerKm)
	}
	return tlm
}

// pushABRP assembles an ABRP telemetry frame from the fresh battery status plus
// the best-effort cached climate/GPS/odometer (refreshed by the soc-level tier)
// and forwards it. Called at the end of a successful poll, only when ABRP is
// enabled. Cached sources may be nil/zero on a cold start; buildABRPTelemetry
// omits absent fields, so the push never fails on missing best-effort data.
func (r *Renault) pushABRP(ac *abrp.Client, status *BatteryStatus, hvac *HvacStatus, loc *LocationStatus, odometerKm float64) {
	capacity := r.store.GetFloat("scheduler.battery_capacity", 0)
	tlm := buildABRPTelemetry(status, hvac, loc, capacity, odometerKm)

	if err := ac.Send(context.Background(), tlm); err != nil {
		r.log.Warn("abrp: send failed", "err", err)
	}
}

func (r *Renault) shouldRefreshDetails() bool {
	r.mu.RLock()
	fetchedAt := r.detailsFetchedAt
	r.mu.RUnlock()

	if fetchedAt.IsZero() {
		if ts := r.store.GetDefault("vehicle.details_fetched_at", ""); ts != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				r.mu.Lock()
				r.detailsFetchedAt = t
				r.mu.Unlock()
				fetchedAt = t
			}
		}
	}

	if fetchedAt.IsZero() {
		return true
	}
	return time.Since(fetchedAt) > 24*time.Hour
}

func (r *Renault) fetchVehicleDetails(vin string) {
	r.mu.RLock()
	accountID := r.accountID
	jwt := r.jwt
	r.mu.RUnlock()

	vin = strings.ToUpper(vin)
	reqURL := fmt.Sprintf("%s/commerce/v1/accounts/%s/vehicles?country=PL",
		r.kamereonURL, accountID)

	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("x-gigya-id_token", jwt)
	req.Header.Set("apikey", kamereonAPIKey)
	req.Header.Set("Content-Type", "application/vnd.api+json")

	resp, err := r.client.Do(req)
	if err != nil {
		r.log.Warn("vehicle details fetch failed", "err", err)
		r.markDetailsFetched(time.Now().Add(-23 * time.Hour)) // retry in ~1h
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		r.log.Warn("vehicle details non-200", "status", resp.StatusCode, "body", string(body))
		r.markDetailsFetched(time.Now().Add(-23 * time.Hour)) // retry in ~1h
		return
	}

	var result struct {
		VehicleLinks []struct {
			VIN            string `json:"vin"`
			VehicleDetails struct {
				Model struct {
					Label string `json:"label"`
				} `json:"model"`
				Assets []struct {
					AssetType  string `json:"assetType"`
					Viewpoint  string `json:"viewpoint"`
					Renditions []struct {
						ResolutionType string `json:"resolutionType"`
						URL            string `json:"url"`
					} `json:"renditions"`
				} `json:"assets"`
			} `json:"vehicleDetails"`
		} `json:"vehicleLinks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		r.log.Warn("vehicle details decode failed", "err", err)
		r.markDetailsFetched(time.Now().Add(-23 * time.Hour)) // retry in ~1h
		return
	}

	for _, vl := range result.VehicleLinks {
		if strings.ToUpper(vl.VIN) != vin {
			continue
		}

		modelName := vl.VehicleDetails.Model.Label
		if modelName != "" {
			_ = r.store.Set("vehicle.model_name", modelName)
		}

		// Find car image: prefer mybrand_2 (top 3/4 view), LARGE rendition
		var imageURL string
		for _, asset := range vl.VehicleDetails.Assets {
			if asset.AssetType != "PICTURE" || asset.Viewpoint != "mybrand_2" {
				continue
			}
			for _, rend := range asset.Renditions {
				if rend.ResolutionType == "ONE_MYRENAULT_LARGE" {
					imageURL = rend.URL
					break
				}
			}
			if imageURL == "" && len(asset.Renditions) > 0 {
				imageURL = asset.Renditions[0].URL
			}
			break
		}

		// Download and cache the image in SQLite
		if imageURL != "" {
			if imgData, err := r.downloadImage(imageURL); err == nil {
				_ = r.store.Set("vehicle.picture_data", base64.StdEncoding.EncodeToString(imgData))
			} else {
				r.log.Warn("vehicle image download failed", "err", err)
			}
		}

		r.markDetailsFetched(time.Now())

		r.events.Info(events.ActionVehicleDetails, nil,
			map[string]any{"model": modelName, "hasImage": imageURL != ""},
		)

		r.log.Info("vehicle details fetched", "model", modelName, "hasImage", imageURL != "")
		return
	}

	r.log.Warn("vehicle not found in account", "vin", vin)
	r.markDetailsFetched(time.Now().Add(-23 * time.Hour)) // retry in ~1h
}

func (r *Renault) markDetailsFetched(t time.Time) {
	r.mu.Lock()
	r.detailsFetchedAt = t
	r.mu.Unlock()
	_ = r.store.Set("vehicle.details_fetched_at", t.UTC().Format(time.RFC3339))
}

func (r *Renault) downloadImage(imageURL string) ([]byte, error) {
	resp, err := r.client.Get(imageURL)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}

	// Limit to 1MB to prevent abuse
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return data, nil
}
