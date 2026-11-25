package web

import (
	"encoding/json"
	"net/http"

	"github.com/consi/grosz/internal/camera"
	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/zappi"
)

// settingToOCPPKey maps grosz setting keys to Zappi OCPP configuration keys.
var settingToOCPPKey = map[string]string{
	"zappi.charger_name":   zappi.KeyChargerName,
	"zappi.meter_interval": zappi.KeyMeterValueSampleInterval,
	"zappi.id_tag":         zappi.KeyPlugAndChargeId,
	"zappi.qr_url":         zappi.KeyPaymentURL,
}

// schedulerCacheKeys lists settings whose change must trigger an immediate
// re-plan rather than waiting up to 15 minutes for the periodic tick.
var schedulerCacheKeys = map[string]bool{
	"scheduler.max_price":        true,
	"scheduler.departure_time":   true,
	"scheduler.battery_capacity": true,
	"scheduler.target_soc":       true,
	"scheduler.safe_soc":         true,
	"scheduler.min_soc":          true,
	"scheduler.cheap_fill_price": true,
	"scheduler.cheap_fill_soc":   true,
	"scheduler.enabled":          true,
	"charger.max_power":          true,
	"charger.min_power":          true,
	"charger.mode":               true,
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.All()
	if err != nil {
		s.internalError(w, "failed to load settings", err)
		return
	}
	// Redact sensitive values
	for k := range sensitiveKeys {
		if _, ok := all[k]; ok {
			all[k] = ""
		}
	}
	writeJSON(w, all)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var updates map[string]string
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Hash password before storing
	if pw, ok := updates["auth.password"]; ok {
		if pw == "" {
			// Empty means "no change" (redacted field submitted unchanged)
			delete(updates, "auth.password")
		} else {
			hashed, err := hashPassword(pw)
			if err != nil {
				http.Error(w, "failed to hash password", http.StatusInternalServerError)
				return
			}
			updates["auth.password"] = hashed
		}
	}

	// Camera: validate the URL and move credentials typed into it out of the
	// stored (and logged) value.
	cameraChanged, err := camera.NormalizeUpdates(updates)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":    err.Error(),
			"errorKey": "settings.cameraUrlInvalid",
		})
		return
	}

	if err := s.store.SetMany(updates); err != nil {
		s.internalError(w, "failed to save settings", err)
		return
	}

	// Log what changed (redact sensitive values)
	redacted := events.RedactSettings(updates)
	for k, v := range redacted {
		s.log.Info("setting updated", "key", k, "value", v)
	}
	changedKeys := make([]string, 0, len(updates))
	for k := range updates {
		changedKeys = append(changedKeys, k)
	}
	s.web.Info(events.ActionSettingsUpdated, redacted,
		map[string]any{"changedKeys": changedKeys},
	)

	// Push changed settings to connected charger via OCPP ChangeConfiguration
	cpID := s.store.GetDefault("zappi.charge_box_id", "")
	if cpID != "" {
		if cp := s.ocpp.ChargePoint(cpID); cp != nil && cp.IsConnected() {
			for settingKey, value := range updates {
				if ocppKey, ok := settingToOCPPKey[settingKey]; ok {
					go func() { _ = zappi.PushConfig(s.ocpp, cpID, ocppKey, value, s.log) }()
				}
			}
		}
	}

	// Re-plan right away when a scheduler-relevant key changed.
	if s.scheduler != nil {
		for k := range updates {
			if schedulerCacheKeys[k] {
				s.scheduler.ReloadConfig()
				break
			}
		}
	}

	// Reconnect with the new source and let open dashboards show or hide the
	// preview right away.
	if cameraChanged {
		s.camera.Reload()
		s.BroadcastStatus()
	}

	writeJSON(w, map[string]any{"ok": true})
}
