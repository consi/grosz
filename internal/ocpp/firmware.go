package ocpp

import (
	"fmt"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"

	"github.com/consi/grosz/internal/events"
)

// OnFirmwareStatusNotification logs charge-point firmware update progress.
// Zappi reports: Downloading, Downloaded, Installing, Installed,
// DownloadFailed, InstallationFailed (Idle is only sent in response to a
// Trigger when not updating).
func (s *Server) OnFirmwareStatusNotification(chargePointId string, request *firmware.FirmwareStatusNotificationRequest) (*firmware.FirmwareStatusNotificationConfirmation, error) {
	s.log.Info("firmware status notification", "id", chargePointId, "status", request.Status)

	s.recordEvent("recv", chargePointId, "FirmwareStatusNotification", request)

	if s.store != nil {
		input := map[string]any{"cpID": chargePointId}
		result := map[string]any{"status": string(request.Status)}
		switch request.Status {
		case firmware.FirmwareStatusDownloadFailed, firmware.FirmwareStatusInstallationFailed:
			s.events.Error(events.ActionFirmwareStatus, input, fmt.Errorf("status=%s", request.Status))
			_ = result
		default:
			s.events.Info(events.ActionFirmwareStatus, input, result)
		}
	}

	return firmware.NewFirmwareStatusNotificationConfirmation(), nil
}

// OnDiagnosticsStatusNotification logs charge-point diagnostics upload progress.
// Required by the FirmwareManagement profile though we don't request diagnostics today.
func (s *Server) OnDiagnosticsStatusNotification(chargePointId string, request *firmware.DiagnosticsStatusNotificationRequest) (*firmware.DiagnosticsStatusNotificationConfirmation, error) {
	s.log.Info("diagnostics status notification", "id", chargePointId, "status", request.Status)

	s.recordEvent("recv", chargePointId, "DiagnosticsStatusNotification", request)

	if s.store != nil {
		s.events.Info(events.ActionDiagnosticsStatus,
			map[string]any{"cpID": chargePointId},
			map[string]any{"status": string(request.Status)},
		)
	}

	return firmware.NewDiagnosticsStatusNotificationConfirmation(), nil
}
