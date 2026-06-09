package vehicle

import (
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

	cancel    context.CancelFunc
	triggerCh chan struct{}
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
	}
	// Restore the durable Gigya credentials so a restart reuses the existing
	// login_token (and trusted device) instead of re-running accounts.login —
	// which, under Renault's mandatory TFA, would force a fresh email-code flow.
	r.session = st.GetDefault("vehicle.renault_session", "")
	r.gmid = st.GetDefault("vehicle.renault_gmid", "")
	r.ucid = st.GetDefault("vehicle.renault_ucid", "")
	go r.loop(ctx)
	return r
}

// Stop shuts down the poller.
func (r *Renault) Stop() { r.cancel() }

// Trigger asks the poll loop to run a poll immediately, breaking out of its
// inter-poll sleep. Non-blocking; coalesces multiple calls into one poll.
func (r *Renault) Trigger() {
	select {
	case r.triggerCh <- struct{}{}:
	default:
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

func (r *Renault) loop(ctx context.Context) {
	for {
		user := r.store.GetDefault("vehicle.renault_user", "")
		pass := r.store.GetDefault("vehicle.renault_password", "")
		vin := r.store.GetDefault("vehicle.vin", "")

		if user == "" || pass == "" || vin == "" {
			select {
			case <-ctx.Done():
				return
			case <-r.triggerCh:
				continue
			case <-time.After(30 * time.Second):
				continue
			}
		}

		if err := r.poll(user, pass, vin); err != nil {
			// Preserve the last successful SoC, plug status, etc. A transient
			// poll failure (e.g. one-minute Gigya hiccup) used to reset SoC to
			// 0, which made the scheduler plan a panic-sized slot anchored at
			// the current expensive hour; the merge logic then preserved that
			// pulled-back start even after SoC recovered. Leaving the store
			// untouched lets the next successful poll overwrite naturally,
			// and the existing plug-check stale_data guard already handles
			// prolonged outages.
			r.log.Warn("renault poll failed", "err", err)
			vinSuffix := vin
			if len(vinSuffix) > 4 {
				vinSuffix = vinSuffix[len(vinSuffix)-4:]
			}
			r.events.Warn(events.ActionRenaultPoll,
				map[string]any{"vin": vinSuffix},
				map[string]any{"error": err.Error(), "socPreserved": true},
			)
		}

		interval := r.store.GetInt("vehicle.poll_interval", 15)
		if interval < 1 {
			interval = 1
		}
		select {
		case <-ctx.Done():
			return
		case <-r.triggerCh:
		case <-time.After(time.Duration(interval) * time.Minute):
		}
	}
}

func (r *Renault) poll(user, pass, vin string) error {
	// Authenticate if needed
	if err := r.ensureAuth(user, pass); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Get battery status
	status, err := r.getBatteryStatus(vin)
	if err != nil {
		// JWT might be expired, re-auth and retry
		r.mu.Lock()
		r.jwt = ""
		r.mu.Unlock()
		if err := r.ensureAuth(user, pass); err != nil {
			return fmt.Errorf("re-auth: %w", err)
		}
		status, err = r.getBatteryStatus(vin)
		if err != nil {
			return fmt.Errorf("battery status: %w", err)
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
		)
	}

	// Fetch vehicle details (model name + image), refresh daily
	if r.shouldRefreshDetails() {
		r.fetchVehicleDetails(vin)
	}

	// Fetch cockpit data for odometer
	var totalMileage float64
	cockpit, err := r.getCockpitStatus(vin)
	if err != nil {
		r.log.Debug("cockpit data unavailable", "err", err)
	} else if cockpit != nil && cockpit.TotalMileage > 0 {
		totalMileage = cockpit.TotalMileage
		if err := r.store.InsertOdometerReading(store.OdometerReading{
			Timestamp: time.Now(),
			Mileage:   cockpit.TotalMileage,
		}); err != nil {
			r.log.Warn("failed to store odometer reading", "err", err)
		}
	}

	r.mu.RLock()
	cb := r.onUpdate
	r.mu.RUnlock()
	if cb != nil {
		cb(status.Level)
	}

	vinSuffix := vin
	if len(vinSuffix) > 4 {
		vinSuffix = vinSuffix[len(vinSuffix)-4:]
	}
	r.events.Info(events.ActionRenaultPoll,
		map[string]any{"vin": vinSuffix},
		map[string]any{
			"soc":            status.Level,
			"autonomy":       status.Autonomy,
			"plugStatus":     status.PlugStatus,
			"chargingStatus": status.ChargingStatus,
			"totalMileage":   totalMileage,
		},
	)

	// A successful poll proves auth is healthy — clear any stale TFA flag.
	if r.store.GetBool("vehicle.renault_tfa_required", false) {
		_ = r.store.Set("vehicle.renault_tfa_required", "false")
	}

	// Forward telemetry to ABRP (no-op unless a token is configured). Gated on
	// Enabled() so the extra hvac/location fetches are skipped when ABRP is off.
	r.mu.RLock()
	ac := r.abrp
	r.mu.RUnlock()
	if ac != nil && ac.Enabled() {
		r.pushABRP(ac, vin, status, totalMileage)
	}

	return nil
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

// pushABRP gathers best-effort telemetry (ambient temperature, GPS) on top of
// the already-fetched battery status and forwards it to ABRP. Called at the end
// of a successful poll, only when ABRP is enabled. Best-effort sources that fail
// are simply omitted; they never fail the push.
func (r *Renault) pushABRP(ac *abrp.Client, vin string, status *BatteryStatus, odometerKm float64) {
	hvac, err := r.getHvacStatus(vin)
	if err != nil {
		r.log.Debug("abrp: hvac-status unavailable", "err", err)
		hvac = nil
	}
	loc, err := r.getLocation(vin)
	if err != nil {
		r.log.Debug("abrp: location unavailable", "err", err)
		loc = nil
	}

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
