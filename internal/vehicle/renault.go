package vehicle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/consi/grosz/internal/store"
)

// gigyaAPIKey and kamereonAPIKey are public reverse-engineered values extracted
// from the official MyRenault mobile app. They identify the client to Gigya
// (auth) and Kamereon (vehicle telemetry) and are required for any third-party
// MyRenault integration. The same constants are used by python-renault-api,
// the Home Assistant Renault integration, and other open-source projects in
// this space. They are not user secrets.
const (
	gigyaURL = "https://accounts.eu1.gigya.com"

	defaultKamereonURL = "https://api-wired-prod-1-euw1.wrd-aws.com"
	gigyaAPIKey        = "3_2YBjydYRd1shr6bsZdrvA9z7owvSg3W5RHDYDp6AlatXw9hqx7nVoanRn8YGsBN8"
	kamereonAPIKey     = "YjkKtHmGfaceeuExUDKGxrLZGGvtVS0J"
)

// CockpitStatus holds the vehicle's cockpit data (odometer).
type CockpitStatus struct {
	TotalMileage float64 `json:"totalMileage"`
}

// BatteryStatus holds the vehicle's battery state.
type BatteryStatus struct {
	Level         int     `json:"batteryLevel"`     // 0-100 %
	Autonomy      int     `json:"batteryAutonomy"`  // km
	PlugStatus    int     `json:"plugStatus"`       // 0=unplugged, 1=plugged
	ChargingStatus float64 `json:"chargingStatus"`  // 0=not charging, >0=charging
	RemainingTime int     `json:"chargingRemainingTime"` // minutes
	Timestamp     string  `json:"timestamp"`
}

// Renault polls the MyRenault/Kamereon API for battery SoC.
type Renault struct {
	store       *store.Store
	log         *slog.Logger
	client      *http.Client
	kamereonURL string

	mu               sync.RWMutex
	session          string // Gigya session token
	jwt              string
	personID         string
	accountID        string
	jwtExpiry        time.Time
	last             *BatteryStatus
	onUpdate         func(int) // called with SoC on successful poll
	detailsFetchedAt time.Time

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
		log:         log.With("component", "renault"),
		client:      &http.Client{Timeout: 15 * time.Second},
		kamereonURL: kamereonURL,
		cancel:      cancel,
		triggerCh:   make(chan struct{}, 1),
	}
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
			r.log.Warn("renault poll failed", "err", err)
			// Reset SoC to 0 so scheduler assumes full charge needed
			_ = r.store.Set("scheduler.current_soc", "0")
			_ = r.store.Set("vehicle.plug_status", "")
			_ = r.store.Set("vehicle.battery_autonomy", "")
			_ = r.store.Set("vehicle.charging_status", "")
			_ = r.store.Set("vehicle.charging_remaining_time", "")
			_ = r.store.Set("vehicle.battery_timestamp", "")
			r.mu.RLock()
			cb := r.onUpdate
			r.mu.RUnlock()
			if cb != nil {
				cb(0)
			}
			vinSuffix := vin
			if len(vinSuffix) > 4 {
				vinSuffix = vinSuffix[len(vinSuffix)-4:]
			}
			r.store.RecordSystemEvent(store.SystemEvent{
				Timestamp: time.Now(), Source: "renault", Action: "poll", Level: "warn",
				Input:  map[string]any{"vin": vinSuffix},
				Result: map[string]any{"error": err.Error(), "socReset": true},
			})
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
	r.store.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "renault", Action: "poll",
		Input: map[string]any{"vin": vinSuffix},
		Result: map[string]any{
			"soc":            status.Level,
			"autonomy":       status.Autonomy,
			"plugStatus":     status.PlugStatus,
			"chargingStatus": status.ChargingStatus,
			"totalMileage":   totalMileage,
		},
	})

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
			return err
		}
		r.mu.Lock()
		r.session = session
		r.mu.Unlock()
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
			// Session expired, clear and fail
			r.mu.Lock()
			r.session = ""
			r.mu.Unlock()
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
		"apiKey":  {gigyaAPIKey},
		"loginID": {user},
		"password": {pass},
	}
	resp, err := r.client.PostForm(gigyaURL+"/accounts.login", form)
	if err != nil {
		return "", fmt.Errorf("gigya login: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		SessionInfo struct {
			CookieValue string `json:"cookieValue"`
		} `json:"sessionInfo"`
		ErrorCode    int    `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("gigya login decode: %w", err)
	}
	if result.ErrorCode != 0 {
		return "", fmt.Errorf("gigya login: %s (code %d)", result.ErrorMessage, result.ErrorCode)
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
	resp, err := r.client.PostForm(gigyaURL+"/accounts.getAccountInfo", form)
	if err != nil {
		return "", fmt.Errorf("gigya getAccountInfo: %w", err)
	}
	defer resp.Body.Close()

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
	resp, err := r.client.PostForm(gigyaURL+"/accounts.getJWT", form)
	if err != nil {
		return "", fmt.Errorf("gigya getJWT: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		IDToken   string `json:"id_token"`
		ErrorCode int    `json:"errorCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("gigya getJWT decode: %w", err)
	}
	if result.ErrorCode != 0 || result.IDToken == "" {
		return "", fmt.Errorf("gigya getJWT: no token (code %d)", result.ErrorCode)
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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			io.ReadAll(io.LimitReader(resp.Body, 256))
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
	defer resp.Body.Close()

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

		r.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(), Source: "renault", Action: "vehicleDetails",
			Result: map[string]any{"model": modelName, "hasImage": imageURL != ""},
		})

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
	defer resp.Body.Close()

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
