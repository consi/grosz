package web

import (
	"encoding/json"
	"net/http"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/ocpp"
)

// handleOCPPConsoleActions lists the OCPP 1.6J requests the debug console
// can send, with templates and field hints for suggestions.
func (s *Server) handleOCPPConsoleActions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"actions": ocpp.ConsoleActions()})
}

// handleOCPPConsoleSend sends a raw OCPP request typed in the debug console
// and returns the charge point's response (or the validation / transport
// error). Both messages also land in the OCPP log and the live stream.
func (s *Server) handleOCPPConsoleSend(w http.ResponseWriter, r *http.Request) {
	cpID := r.PathValue("id")
	var req struct {
		Action  string          `json:"action"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "body must be {\"action\": ..., \"payload\": {...}}: "+err.Error(), http.StatusBadRequest)
		return
	}
	if cp := s.ocpp.ChargePoint(cpID); cp == nil || !cp.IsConnected() {
		writeJSONStatus(w, http.StatusConflict, ocpp.ConsoleResult{Action: req.Action, Error: "charge point " + cpID + " is not connected"})
		return
	}
	res := s.ocpp.SendConsoleRequest(cpID, req.Action, req.Payload)
	if s.scheduler != nil && (req.Action == "SetChargingProfile" || req.Action == "ClearChargingProfile") && res.Error == "" {
		s.scheduler.NoteExternalProfileWrite()
	}
	result := map[string]any{"durationMs": res.DurationMs}
	if res.Error != "" {
		result["error"] = res.Error
	}
	s.web.Info(events.ActionOCPPConsoleSend, map[string]any{"cpID": cpID, "action": req.Action}, result)
	writeJSON(w, res)
}
