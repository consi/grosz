package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/vehicle"
)

// handleRenaultTFAStatus reports the current Renault two-factor verification
// state for the settings UI.
func (s *Server) handleRenaultTFAStatus(w http.ResponseWriter, _ *http.Request) {
	if s.renault == nil {
		http.Error(w, "renault not configured", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, s.renault.TFAStatus())
}

// handleRenaultTFAStart triggers the email-code flow. It logs in with the
// stored credentials; if Renault demands TFA it emails a code and returns the
// obfuscated destination address. If login already succeeds (device still
// trusted) it reports alreadyAuthenticated and no code is needed.
func (s *Server) handleRenaultTFAStart(w http.ResponseWriter, _ *http.Request) {
	if s.renault == nil {
		http.Error(w, "renault not configured", http.StatusServiceUnavailable)
		return
	}
	user := s.store.GetDefault("vehicle.renault_user", "")
	pass := s.store.GetDefault("vehicle.renault_password", "")
	if user == "" || pass == "" {
		http.Error(w, "renault credentials not configured", http.StatusBadRequest)
		return
	}

	obfuscated, err := s.renault.StartTFA(user, pass)
	if errors.Is(err, vehicle.ErrTFANotRequired) {
		writeJSON(w, map[string]any{"alreadyAuthenticated": true})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"obfuscatedEmail": obfuscated})
}

// handleRenaultTFAVerify submits the emailed verification code, completing the
// trusted-device registration and persisting the resulting session token.
func (s *Server) handleRenaultTFAVerify(w http.ResponseWriter, r *http.Request) {
	if s.renault == nil {
		http.Error(w, "renault not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.renault.CompleteTFA(req.Code); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSetSocTarget writes the car's charge limit (target SoC) to the Renault
// API via Kamereon. The value is clamped to 30–100% (matching the Dashboard
// stepper). On success the scheduler config is reloaded so the new live target
// feeds the next recompute, and the change is recorded in the system log.
func (s *Server) handleSetSocTarget(w http.ResponseWriter, r *http.Request) {
	if s.renault == nil {
		http.Error(w, "renault not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Target int `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Target < 30 || req.Target > 100 {
		http.Error(w, "target must be between 30 and 100", http.StatusBadRequest)
		return
	}

	if err := s.renault.SetSocTarget(req.Target); err != nil {
		// The outcome is recorded by SetSocTarget under the renault source.
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Pick up the new live charge limit on the next scheduler recompute instead
	// of waiting for the periodic poll.
	if s.scheduler != nil {
		s.scheduler.ReloadConfig()
	}

	writeJSON(w, map[string]any{"ok": true})
}

// handleStartTrip prepares the car for a trip: raises the car's charge limit
// (default 100%) and records when it must be ready (default: next departure).
// The planner then reaches the limit by that time in the cheapest hours, and
// the limit returns to the normal target when the car is unplugged.
func (s *Server) handleStartTrip(w http.ResponseWriter, r *http.Request) {
	if s.renault == nil {
		http.Error(w, "renault not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Target  int    `json:"target"`
		ReadyBy string `json:"readyBy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Target == 0 {
		req.Target = 100
	}
	if req.Target < 50 || req.Target > 100 {
		http.Error(w, "target must be between 50 and 100", http.StatusBadRequest)
		return
	}
	readyBy := ""
	if req.ReadyBy != "" {
		t, err := time.Parse(time.RFC3339, req.ReadyBy)
		if err != nil || !t.After(time.Now()) {
			http.Error(w, "readyBy must be a future RFC3339 time", http.StatusBadRequest)
			return
		}
		readyBy = t.UTC().Format(time.RFC3339)
	}
	if err := s.renault.SetSocTarget(req.Target); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.store.Set("vehicle.soc_target", strconv.Itoa(req.Target))
	_ = s.store.Set("scheduler.fill_raised_from", "")
	_ = s.store.Set("scheduler.trip_ready_by", readyBy)
	s.web.Info(events.ActionTripSet, map[string]any{"target": req.Target, "readyBy": readyBy}, nil)
	if s.scheduler != nil {
		s.scheduler.ReloadConfig()
	}
	s.BroadcastStatus()
	writeJSON(w, map[string]any{"ok": true})
}

// handleCancelTrip restores the normal charge limit and clears the trip.
func (s *Server) handleCancelTrip(w http.ResponseWriter, r *http.Request) {
	if s.renault == nil {
		http.Error(w, "renault not configured", http.StatusServiceUnavailable)
		return
	}
	normal := s.store.GetInt("scheduler.target_soc", 80)
	if err := s.renault.SetSocTarget(normal); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = s.store.Set("vehicle.soc_target", strconv.Itoa(normal))
	_ = s.store.Set("scheduler.trip_ready_by", "")
	s.web.Info(events.ActionTripCancelled, map[string]any{"target": normal}, nil)
	if s.scheduler != nil {
		s.scheduler.ReloadConfig()
	}
	s.BroadcastStatus()
	writeJSON(w, map[string]any{"ok": true})
}
