package ocpp

import (
	"testing"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnFirmwareStatusNotification_RecordsSystemEvent(t *testing.T) {
	cases := []struct {
		status    firmware.FirmwareStatus
		wantLevel string
	}{
		{firmware.FirmwareStatusDownloading, "info"},
		{firmware.FirmwareStatusDownloaded, "info"},
		{firmware.FirmwareStatusInstalling, "info"},
		{firmware.FirmwareStatusInstalled, "info"},
		{firmware.FirmwareStatusDownloadFailed, "error"},
		{firmware.FirmwareStatusInstallationFailed, "error"},
		{firmware.FirmwareStatusIdle, "info"},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			s := testServer(t)

			conf, err := s.OnFirmwareStatusNotification("CP001", &firmware.FirmwareStatusNotificationRequest{
				Status: tc.status,
			})
			require.NoError(t, err)
			assert.NotNil(t, conf)

			events, err := s.store.SystemEventsBySource("ocpp", 50, 0)
			require.NoError(t, err)
			var found bool
			for _, e := range events {
				if e.Action == "firmwareStatus" {
					found = true
					assert.Equal(t, tc.wantLevel, e.Level, "level for %s", tc.status)
					if result, ok := e.Result.(map[string]any); ok {
						assert.Equal(t, string(tc.status), result["status"])
					}
					break
				}
			}
			assert.True(t, found, "firmwareStatus system event must be recorded")
		})
	}
}

func TestOnDiagnosticsStatusNotification_RecordsSystemEvent(t *testing.T) {
	s := testServer(t)

	conf, err := s.OnDiagnosticsStatusNotification("CP001", &firmware.DiagnosticsStatusNotificationRequest{
		Status: firmware.DiagnosticsStatusUploaded,
	})
	require.NoError(t, err)
	assert.NotNil(t, conf)

	events, err := s.store.SystemEventsBySource("ocpp", 50, 0)
	require.NoError(t, err)
	var found bool
	for _, e := range events {
		if e.Action == "diagnosticsStatus" {
			found = true
			break
		}
	}
	assert.True(t, found)
}
