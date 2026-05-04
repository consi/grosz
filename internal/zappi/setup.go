package zappi

import (
	"log/slog"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"

	"github.com/consi/grosz/internal/ocpp"
	"github.com/consi/grosz/internal/store"
)

// Setup runs the Zappi post-boot configuration sequence.
// It reads settings from the store and configures the Zappi charger via OCPP.
func Setup(srv *ocpp.Server, cpID string, st *store.Store, log *slog.Logger) error {
	log = log.With("component", "zappi", "chargeBox", cpID)
	log.Info("starting Zappi setup sequence")

	// 1. Read current configuration
	current, err := srv.GetConfiguration(cpID, nil)
	if err != nil {
		log.Warn("GetConfiguration failed, continuing with setup", "err", err)
		current = make(map[string]string)
	} else {
		log.Info("current configuration", "keys", len(current))
		for k, v := range current {
			log.Debug("config key", "key", k, "value", v)
		}
	}

	var configured, skipped []string

	// setConfig only sends ChangeConfiguration if the value differs from current
	setConfig := func(key, value, desc string) {
		if cur, ok := current[key]; ok && strings.EqualFold(cur, value) {
			log.Debug("config already set", "key", key, "value", value)
			skipped = append(skipped, key)
			return
		}
		if err := srv.ChangeConfiguration(cpID, key, value); err != nil {
			log.Warn("failed to set config", "key", key, "value", value, "desc", desc, "err", err)
		} else {
			log.Info("config set", "key", key, "value", value)
			configured = append(configured, key)
		}
	}

	// 2. Enable commercial mode
	if st.GetBool("zappi.commercial_mode", true) {
		setConfig(KeyCommercialMode, "True", "enable commercial mode")
	}

	// 3-5. Charging configuration
	setConfig(KeyFreeCharging, "False", "require OCPP authorization")
	setConfig(KeyAuthorizeRemoteTxRequests, "False", "skip auth round-trip on remote start")
	setConfig(KeyRandomSmartChargeDelay, "0", "disable random start delay")

	// Display name and QR URL
	if name := st.GetDefault("zappi.charger_name", ""); name != "" {
		setConfig(KeyChargerName, name, "display name on LCD")
	}
	if qrURL := st.GetDefault("zappi.qr_url", ""); qrURL != "" {
		setConfig(KeyPaymentURL, qrURL, "QR code URL on LCD")
	}

	// 6. Configure meter values
	measurands := st.GetDefault("zappi.measurands", DefaultMeasurands)
	setConfig(KeyMeterValuesSampledData, measurands, "meter value measurands")

	// 7. Configure meter interval
	interval := st.GetDefault("zappi.meter_interval", "10")
	setConfig(KeyMeterValueSampleInterval, interval, "meter sample interval")

	log.Info("Zappi setup complete")
	st.RecordSystemEvent(store.SystemEvent{
		Timestamp: time.Now(), Source: "zappi", Action: "setup",
		Input: map[string]any{"cpID": cpID},
		Result: map[string]any{"configured": configured, "skipped": skipped},
	})
	return nil
}

// PushConfig sends a single ChangeConfiguration to the connected charger.
func PushConfig(srv *ocpp.Server, cpID, key, value string, log *slog.Logger) error {
	log = log.With("component", "zappi", "chargeBox", cpID)
	if err := srv.ChangeConfiguration(cpID, key, value); err != nil {
		log.Warn("failed to push config", "key", key, "value", value, "err", err)
		return err
	}
	log.Info("config pushed", "key", key, "value", value)
	return nil
}

// RegisterBootHook registers the Zappi setup to run after BootNotification
// for charge points that identify as Zappi.
func RegisterBootHook(srv *ocpp.Server, st *store.Store, log *slog.Logger) {
	srv.RegisterBootHook(func(chargePointID string, req *core.BootNotificationRequest) {
		if !IsZappi(req.ChargePointVendor, req.ChargePointModel) {
			log.Debug("charge point is not a Zappi, skipping setup",
				"id", chargePointID,
				"vendor", req.ChargePointVendor,
				"model", req.ChargePointModel,
			)
			return
		}

		if err := Setup(srv, chargePointID, st, log); err != nil {
			log.Error("Zappi setup failed", "id", chargePointID, "err", err)
		}
	})
}
