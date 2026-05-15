package web

import (
	"encoding/json"
	"net/http"

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

// schedulerCacheKeys lists settings whose change must invalidate the
// scheduler's in-memory Config snapshot or its control-loop decisions,
// so the next recompute reflects the new value immediately rather than
// waiting up to 15 minutes for the periodic tick.
var schedulerCacheKeys = map[string]bool{
	"scheduler.max_price":        true,
	"scheduler.deadline_time":    true,
	"scheduler.battery_capacity": true,
	"scheduler.target_soc":       true,
	"scheduler.target_energy":    true,
	"scheduler.skip_above_soc":   true,
	"scheduler.min_soc":          true,
	"scheduler.charge_headroom":  true,
	"scheduler.enabled":          true,
	"charger.max_power":          true,
	"charger.min_power":          true,
	"charger.mode":               true,
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.All()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

	if err := s.store.SetMany(updates); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

	// Invalidate the scheduler's cached Config when any scheduler-relevant
	// key changed, so the new value takes effect on the next recompute
	// instead of being masked by a stale snapshot from SetConfig.
	if s.scheduler != nil {
		for k := range updates {
			if schedulerCacheKeys[k] {
				s.scheduler.ReloadConfig()
				break
			}
		}
	}

	writeJSON(w, map[string]any{"ok": true})
}
