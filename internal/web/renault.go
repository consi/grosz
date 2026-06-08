package web

import (
	"encoding/json"
	"errors"
	"net/http"

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
