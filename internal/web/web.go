package web

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/consi/grosz/internal/meter"
	"github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/scheduler"
	"github.com/consi/grosz/internal/store"
	"github.com/consi/grosz/internal/tariff"
)

//go:embed dist/*
var staticFiles embed.FS

// Server is the web/API server.
type Server struct {
	srv       *http.Server
	ocpp      *ocpp.Server
	store     *store.Store
	tariff    tariff.Provider
	scheduler *scheduler.Scheduler
	meter     *meter.Poller
	sse       *SSEBroker
	log       *slog.Logger

	sessionsMu sync.RWMutex
	sessions   map[string]bool // active session tokens
	challenges *challengeStore
}

// New creates a new web server.
func New(ocppSrv *ocpp.Server, st *store.Store, tp tariff.Provider, sched *scheduler.Scheduler, mp *meter.Poller, log *slog.Logger) *Server {
	s := &Server{
		ocpp:      ocppSrv,
		store:     st,
		tariff:    tp,
		scheduler: sched,
		meter:     mp,
		sse:       NewSSEBroker(log),
		log:       log.With("component", "web"),
		sessions:   make(map[string]bool),
		challenges: newChallengeStore(),
	}

	s.migratePasswordHash()

	mux := http.NewServeMux()

	// Auth routes (unprotected)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/check", s.handleAuthCheck)

	// WebAuthn login (public)
	mux.HandleFunc("POST /api/webauthn/login/begin", s.handleWebAuthnLoginBegin)
	mux.HandleFunc("POST /api/webauthn/login/complete", s.handleWebAuthnLoginComplete)

	// WebAuthn registration & management (protected)
	mux.HandleFunc("POST /api/webauthn/register/begin", s.requireAuth(s.handleWebAuthnRegisterBegin))
	mux.HandleFunc("POST /api/webauthn/register/complete", s.requireAuth(s.handleWebAuthnRegisterComplete))
	mux.HandleFunc("GET /api/webauthn/credentials", s.requireAuth(s.handleWebAuthnCredentials))
	mux.HandleFunc("DELETE /api/webauthn/credentials/{id}", s.requireAuth(s.handleWebAuthnDeleteCredential))

	// API routes (protected by middleware)
	mux.HandleFunc("GET /api/status", s.requireAuth(s.handleStatus))
	mux.HandleFunc("GET /api/events", s.requireAuth(s.handleEvents))
	mux.HandleFunc("GET /api/events/stream", s.requireAuth(s.sse.ServeHTTP))
	mux.HandleFunc("GET /api/system-events", s.requireAuth(s.handleSystemEvents))
	mux.HandleFunc("GET /api/tariff/rates", s.requireAuth(s.handleRates))
	mux.HandleFunc("GET /api/schedule", s.requireAuth(s.handleGetSchedule))
	mux.HandleFunc("POST /api/schedule", s.requireAuth(s.handleSetSchedule))
	mux.HandleFunc("DELETE /api/schedule", s.requireAuth(s.handleDeleteSchedule))
	mux.HandleFunc("DELETE /api/schedule/{date}", s.requireAuth(s.handleCancelSlot))
	mux.HandleFunc("POST /api/schedule/{date}/restore", s.requireAuth(s.handleRestoreSlot))
	mux.HandleFunc("GET /api/schedule/overrides", s.requireAuth(s.handleListOverrides))
	mux.HandleFunc("POST /api/schedule/overrides", s.requireAuth(s.handleCreateOverride))
	mux.HandleFunc("DELETE /api/schedule/overrides/{id}", s.requireAuth(s.handleDeleteOverride))
	mux.HandleFunc("POST /api/charger/{id}/start", s.requireAuth(s.handleChargerStart))
	mux.HandleFunc("POST /api/charger/{id}/stop", s.requireAuth(s.handleChargerStop))
	mux.HandleFunc("POST /api/charger/{id}/reset", s.requireAuth(s.handleChargerReset))
	mux.HandleFunc("POST /api/charger/{id}/clear-cache", s.requireAuth(s.handleChargerClearCache))
	mux.HandleFunc("POST /api/charger/{id}/update-firmware", s.requireAuth(s.handleChargerUpdateFirmware))
	mux.HandleFunc("GET /api/costlog", s.requireAuth(s.handleCostLog))
	mux.HandleFunc("GET /api/sessions", s.requireAuth(s.handleSessions))
	mux.HandleFunc("GET /api/sessions/report", s.requireAuth(s.handleSessionReport))
	mux.HandleFunc("GET /api/sessions/report/html", s.requireAuth(s.handleSessionReportHTML))
	mux.HandleFunc("GET /api/costs", s.requireAuth(s.handleExternalCosts))
	mux.HandleFunc("POST /api/costs", s.requireAuth(s.handleAddExternalCost))
	mux.HandleFunc("DELETE /api/costs/{id}", s.requireAuth(s.handleDeleteExternalCost))
	mux.HandleFunc("GET /api/meter/hourly", s.requireAuth(s.handleMeterHourly))
	mux.HandleFunc("GET /api/meter/live", s.requireAuth(s.handleMeterLive))
	mux.HandleFunc("GET /api/meter/phases", s.requireAuth(s.handleMeterPhases))
	mux.HandleFunc("GET /api/chart-markers", s.requireAuth(s.handleChartMarkers))
	mux.HandleFunc("GET /api/charger/mode", s.requireAuth(s.handleGetChargerMode))
	mux.HandleFunc("PUT /api/charger/mode", s.requireAuth(s.handleSetChargerMode))
	mux.HandleFunc("GET /api/settings", s.requireAuth(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.requireAuth(s.handlePutSettings))
	mux.HandleFunc("GET /api/vehicle/image", s.handleVehicleImage)

	// Static files (SPA)
	staticFS, err := fs.Sub(staticFiles, "dist")
	if err != nil {
		log.Error("failed to load static files", "err", err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback: serve index.html for non-file paths
		if r.URL.Path != "/" && !strings.Contains(r.URL.Path, ".") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})

	s.srv = &http.Server{Handler: cacheMiddleware(mux)}
	return s
}

// cacheMiddleware sets Cache-Control headers on all responses.
// Hashed assets are cached forever; everything else must revalidate.
func cacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		next.ServeHTTP(w, r)
	})
}

// Start begins serving on the given port.
func (s *Server) Start(port int) {
	s.srv.Addr = fmt.Sprintf(":%d", port)
	s.log.Info("starting web server", "port", port)
	if err := s.srv.ListenAndServe(); err != http.ErrServerClosed {
		s.log.Error("web server error", "err", err)
	}
}

// Stop shuts down the web server.
func (s *Server) Stop() {
	s.sse.Close()
	s.srv.Close()
}

// Broadcast sends an SSE event to all connected clients.
func (s *Server) Broadcast(event, data string) {
	s.sse.Broadcast(event, data)
}

// BroadcastStatus builds the current status JSON and broadcasts it via SSE.
func (s *Server) BroadcastStatus() {
	data, err := json.Marshal(s.buildStatus())
	if err != nil {
		return
	}
	s.sse.Broadcast("status", string(data))
}

func (s *Server) buildStatus() any {
	cps := s.ocpp.ChargePoints()

	type connectorInfo struct {
		ID            int                                `json:"id"`
		Status        string                             `json:"status"`
		TransactionID int                                `json:"transactionId,omitempty"`
		Measurements  map[string]ocpp.Measurement        `json:"measurements,omitempty"`
	}
	type cpInfo struct {
		ID          string          `json:"id"`
		Connected   bool            `json:"connected"`
		ConnectedAt *time.Time      `json:"connectedAt,omitempty"`
		Vendor      string          `json:"vendor,omitempty"`
		Model       string          `json:"model,omitempty"`
		Serial      string          `json:"serial,omitempty"`
		Firmware    string          `json:"firmware,omitempty"`
		Connectors  []connectorInfo `json:"connectors"`
	}
	type statusResponse struct {
		ChargePoints          []cpInfo                  `json:"chargePoints"`
		Schedule              *scheduler.Schedule       `json:"schedule,omitempty"`
		Overrides             []store.ScheduleOverride  `json:"overrides"`
		Charging              bool                      `json:"charging"`
		Mode                  string                    `json:"mode"`
		SoC                   float64                   `json:"soc"`
		MinSoC                float64                   `json:"minSoc"`
		SkipAboveSoC          float64                   `json:"skipAboveSoc"`
		SkipReason            string                    `json:"skipReason,omitempty"`
		SkipReasonKey         string                    `json:"skipReasonKey,omitempty"`
		SkipReasonParams      map[string]string         `json:"skipReasonParams,omitempty"`
		DeadlineTime          string                    `json:"deadlineTime"`
		BatteryAutonomy       int                       `json:"batteryAutonomy"`
		ChargingStatus        float64                   `json:"chargingStatus"`
		PlugStatus            int                       `json:"plugStatus"`
		ChargingRemainingTime int                       `json:"chargingRemainingTime"`
		BatteryTimestamp      string                    `json:"batteryTimestamp,omitempty"`
		VehicleModel          string                    `json:"vehicleModel,omitempty"`
		VehiclePicture        string                    `json:"vehiclePicture,omitempty"`
		Mileage               float64                   `json:"mileage"`
	}

	result := make([]cpInfo, 0)
	for _, snap := range cps {
		cp := cpInfo{
			ID:        snap.ID,
			Connected: snap.Connected,
		}
		if !snap.ConnectedAt.IsZero() {
			t := snap.ConnectedAt
			cp.ConnectedAt = &t
		}
		if snap.BootInfo != nil {
			cp.Vendor = snap.BootInfo.Vendor
			cp.Model = snap.BootInfo.Model
			cp.Serial = snap.BootInfo.SerialNumber
			cp.Firmware = snap.BootInfo.FirmwareVersion
		}
		// Only expose connector state when the websocket is alive — the
		// in-memory connectors are hydrated from DB on startup so they would
		// otherwise carry stale state across the wire.
		if snap.Connected {
			for _, c := range snap.Connectors {
				if c.ID == 0 {
					continue // connector 0 is charge-point level, not a physical plug
				}
				ci := connectorInfo{
					ID:            c.ID,
					Status:        c.Status,
					TransactionID: c.TransactionID,
					Measurements:  c.Measurements,
				}
				cp.Connectors = append(cp.Connectors, ci)
			}
		}
		result = append(result, cp)
	}

	var sched *scheduler.Schedule
	var skipReason, skipReasonKey string
	var skipReasonParams map[string]string
	if s.scheduler != nil {
		sched = s.scheduler.Schedule()
		skipReason, skipReasonKey, skipReasonParams = s.scheduler.SkipReason()
	}

	overrides, err := s.store.LoadOverrides(time.Now())
	if err != nil || overrides == nil {
		overrides = []store.ScheduleOverride{}
	}

	var vehiclePicture string
	if s.store.GetDefault("vehicle.picture_data", "") != "" {
		vehiclePicture = "/api/vehicle/image"
	}

	var mileage float64
	if odo, err := s.store.LatestOdometerReading(); err == nil && odo != nil {
		mileage = odo.Mileage
	}

	return statusResponse{
		ChargePoints:          result,
		Schedule:              sched,
		Overrides:             overrides,
		Charging:              scheduler.IsChargeTime(sched),
		Mode:                  s.store.GetDefault("charger.mode", "schedule"),
		SoC:                   s.store.GetFloat("scheduler.current_soc", 0),
		MinSoC:                s.store.GetFloat("scheduler.min_soc", 0),
		SkipAboveSoC:          s.store.GetFloat("scheduler.skip_above_soc", 0),
		SkipReason:            skipReason,
		SkipReasonKey:         skipReasonKey,
		SkipReasonParams:      skipReasonParams,
		DeadlineTime:          s.store.GetDefault("scheduler.deadline_time", "07:00"),
		BatteryAutonomy:       s.store.GetInt("vehicle.battery_autonomy", 0),
		ChargingStatus:        s.store.GetFloat("vehicle.charging_status", 0),
		PlugStatus:            s.store.GetInt("vehicle.plug_status", 0),
		ChargingRemainingTime: s.store.GetInt("vehicle.charging_remaining_time", 0),
		BatteryTimestamp:      s.store.GetDefault("vehicle.battery_timestamp", ""),
		VehicleModel:          s.store.GetDefault("vehicle.model_name", ""),
		VehiclePicture:        vehiclePicture,
		Mileage:               mileage,
	}
}

func (s *Server) handleVehicleImage(w http.ResponseWriter, r *http.Request) {
	data := s.store.GetDefault("vehicle.picture_data", "")
	if data == "" {
		http.NotFound(w, r)
		return
	}
	imgBytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		http.Error(w, "corrupt image data", http.StatusInternalServerError)
		return
	}
	ct := http.DetectContentType(imgBytes)
	if ct == "application/octet-stream" {
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(imgBytes)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	user := s.store.GetDefault("auth.username", "admin")
	passHash := s.store.GetDefault("auth.password", "")

	if req.Username != user || passHash == "" || !checkPassword(passHash, req.Password) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": "invalid credentials"})
		return
	}

	token := generateToken()
	s.sessionsMu.Lock()
	s.sessions[token] = true
	s.sessionsMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		s.sessionsMu.Lock()
		delete(s.sessions, c.Value)
		s.sessionsMu.Unlock()
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	hasPasskeys, _ := s.store.HasCredentials()
	authed := s.isAuthenticated(r)
	resp := map[string]any{
		"authenticated": authed,
		"passkeys":      hasPasskeys,
	}
	w.Header().Set("Content-Type", "application/json")
	if !authed {
		w.WriteHeader(http.StatusUnauthorized)
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) isAuthenticated(r *http.Request) bool {
	c, err := r.Cookie("session")
	if err != nil {
		return false
	}
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return s.sessions[c.Value]
}

func (s *Server) requireAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthenticated(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": "unauthorized"})
			return
		}
		handler(w, r)
	}
}

func generateToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
