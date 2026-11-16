package zappi

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/store"
)

// Settings keys holding the charger's smart-charging capabilities as read
// via GetConfiguration. The scheduler uses MaxSchedulePeriods to keep the
// profiles it sends within what the charger accepts (Zappi rejects oversize
// profiles with "InternalError - Not enough slots").
const (
	SettingCapabilities       = "zappi.capabilities"         // JSON map of all non-secret keys
	SettingMaxSchedulePeriods = "zappi.max_schedule_periods" // ChargingScheduleMaxPeriods
	SettingCapabilitiesReadAt = "zappi.capabilities_read_at" // RFC3339
)

// secretKeys are configuration keys whose values must never be persisted.
var secretKeys = map[string]bool{"AuthorizationKey": true}

// ProbeCapabilities reads the full configuration (read-only) and persists
// the smart-charging relevant keys. Safe to call at any time.
func ProbeCapabilities(srv *ocpp.Server, cpID string, st *store.Store, log *slog.Logger) {
	cfg, err := srv.GetConfiguration(cpID, nil)
	if err != nil {
		log.Warn("capability probe failed", "cpID", cpID, "err", err)
		events.Warn(st, events.SourceZappi, events.ActionZappiCapabilities,
			map[string]any{"cpID": cpID}, map[string]any{"error": err.Error()})
		return
	}
	storeCapabilities(st, cfg)
	result := map[string]any{"keys": len(cfg)}
	for _, k := range []string{"ChargingScheduleMaxPeriods", "MaxChargingProfilesInstalled",
		"ChargeProfileMaxStackLevel", "ChargingScheduleAllowedChargingRateUnit", "SupportedFeatureProfiles"} {
		if v, ok := cfg[k]; ok {
			result[k] = v
		}
	}
	log.Info("charger capabilities read", "cpID", cpID, "caps", result)
	events.Info(st, events.SourceZappi, events.ActionZappiCapabilities, map[string]any{"cpID": cpID}, result)
}

func storeCapabilities(st *store.Store, cfg map[string]string) {
	clean := make(map[string]string, len(cfg))
	for k, v := range cfg {
		if secretKeys[k] {
			continue
		}
		clean[k] = v
	}
	if data, err := json.Marshal(clean); err == nil {
		_ = st.Set(SettingCapabilities, string(data))
	}
	if v, ok := cfg["ChargingScheduleMaxPeriods"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			_ = st.Set(SettingMaxSchedulePeriods, strconv.Itoa(n))
		}
	}
	_ = st.Set(SettingCapabilitiesReadAt, time.Now().UTC().Format(time.RFC3339))
}
