package ocpp

import (
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"

	"github.com/consi/grosz/internal/store"
)

// OnFirmwareStatusNotification logs charge-point firmware update progress.
// Zappi reports: Downloading, Downloaded, Installing, Installed,
// DownloadFailed, InstallationFailed (Idle is only sent in response to a
// Trigger when not updating).
func (s *Server) OnFirmwareStatusNotification(chargePointId string, request *firmware.FirmwareStatusNotificationRequest) (*firmware.FirmwareStatusNotificationConfirmation, error) {
	s.log.Info("firmware status notification", "id", chargePointId, "status", request.Status)

	s.recordEvent("recv", chargePointId, "FirmwareStatusNotification", request)

	if s.store != nil {
		level := "info"
		switch request.Status {
		case firmware.FirmwareStatusDownloadFailed, firmware.FirmwareStatusInstallationFailed:
			level = "error"
		}
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(),
			Source:    "ocpp",
			Action:    "firmwareStatus",
			Level:     level,
			Input:     map[string]any{"cpID": chargePointId},
			Result:    map[string]any{"status": string(request.Status)},
		})
	}

	return firmware.NewFirmwareStatusNotificationConfirmation(), nil
}

// OnDiagnosticsStatusNotification logs charge-point diagnostics upload progress.
// Required by the FirmwareManagement profile though we don't request diagnostics today.
func (s *Server) OnDiagnosticsStatusNotification(chargePointId string, request *firmware.DiagnosticsStatusNotificationRequest) (*firmware.DiagnosticsStatusNotificationConfirmation, error) {
	s.log.Info("diagnostics status notification", "id", chargePointId, "status", request.Status)

	s.recordEvent("recv", chargePointId, "DiagnosticsStatusNotification", request)

	if s.store != nil {
		_ = s.store.RecordSystemEvent(store.SystemEvent{
			Timestamp: time.Now(),
			Source:    "ocpp",
			Action:    "diagnosticsStatus",
			Level:     "info",
			Input:     map[string]any{"cpID": chargePointId},
			Result:    map[string]any{"status": string(request.Status)},
		})
	}

	return firmware.NewDiagnosticsStatusNotificationConfirmation(), nil
}
