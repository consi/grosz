package testutil

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/lorenzodonini/ocpp-go/ws"
)

// TestChargePoint simulates an OCPP 1.6 charge point for integration testing.
type TestChargePoint struct {
	cp  ocpp16.ChargePoint
	t   *testing.T
	log *slog.Logger

	mu              sync.Mutex
	configStore     map[string]string
	triggerCh       chan remotetrigger.MessageTrigger
	remoteStartResp types.RemoteStartStopStatus
	remoteStopResp  types.RemoteStartStopStatus
	resetCh         chan core.ResetType
	clearCacheCh    chan struct{}
	updateFirmwareCh chan *firmware.UpdateFirmwareRequest
}

// NewTestChargePoint creates and starts a test charge point that connects to the given URL.
func NewTestChargePoint(t *testing.T, serverURL, id string) *TestChargePoint {
	t.Helper()

	client := ws.NewClient()
	cp := ocpp16.NewChargePoint(id, nil, client)

	tcp := &TestChargePoint{
		cp:  cp,
		t:   t,
		log: slog.Default().With("cp", id),
		configStore: map[string]string{
			"CommercialMode":          "False",
			"FreeCharging":            "True",
			"MeterValuesSampledData":  "",
			"MeterValueSampleInterval": "30",
		},
		triggerCh:       make(chan remotetrigger.MessageTrigger, 10),
		remoteStartResp: types.RemoteStartStopStatusAccepted,
		remoteStopResp:  types.RemoteStartStopStatusAccepted,
		resetCh:         make(chan core.ResetType, 4),
		clearCacheCh:    make(chan struct{}, 4),
		updateFirmwareCh: make(chan *firmware.UpdateFirmwareRequest, 4),
	}

	cp.SetCoreHandler(tcp)
	cp.SetSmartChargingHandler(tcp)
	cp.SetRemoteTriggerHandler(tcp)
	cp.SetFirmwareManagementHandler(tcp)

	err := cp.Start(serverURL)
	if err != nil {
		t.Fatalf("charge point start: %v", err)
	}
	t.Cleanup(func() { cp.Stop() })

	return tcp
}

// SendBootNotification sends a BootNotification and returns the confirmation.
func (tcp *TestChargePoint) SendBootNotification(vendor, model string) (*core.BootNotificationConfirmation, error) {
	conf, err := tcp.cp.BootNotification(model, vendor)
	if err != nil {
		return nil, fmt.Errorf("boot notification: %w", err)
	}
	return conf, nil
}

// SendStatusNotification sends a StatusNotification.
func (tcp *TestChargePoint) SendStatusNotification(connectorID int, status core.ChargePointStatus) error {
	_, err := tcp.cp.StatusNotification(connectorID, core.NoError, status)
	return err
}

// SendHeartbeat sends a Heartbeat.
func (tcp *TestChargePoint) SendHeartbeat() (*core.HeartbeatConfirmation, error) {
	return tcp.cp.Heartbeat()
}

// SendStartTransaction sends a StartTransaction.
func (tcp *TestChargePoint) SendStartTransaction(connectorID int, idTag string, meterStart int) (*core.StartTransactionConfirmation, error) {
	return tcp.cp.StartTransaction(connectorID, idTag, meterStart, types.NewDateTime(time.Now()))
}

// SendStopTransaction sends a StopTransaction.
func (tcp *TestChargePoint) SendStopTransaction(txnID, meterStop int) (*core.StopTransactionConfirmation, error) {
	return tcp.cp.StopTransaction(meterStop, types.NewDateTime(time.Now()), txnID)
}

// SendMeterValues sends MeterValues.
func (tcp *TestChargePoint) SendMeterValues(connectorID int, values []types.MeterValue) error {
	_, err := tcp.cp.MeterValues(connectorID, values)
	return err
}

// SendAuthorize sends an Authorize request.
func (tcp *TestChargePoint) SendAuthorize(idTag string) (*core.AuthorizeConfirmation, error) {
	return tcp.cp.Authorize(idTag)
}

// WaitTrigger waits for a TriggerMessage from the server (with timeout).
func (tcp *TestChargePoint) WaitTrigger(timeout time.Duration) (remotetrigger.MessageTrigger, error) {
	select {
	case msg := <-tcp.triggerCh:
		return msg, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for trigger message")
	}
}

// SetRemoteStartResponse controls the response to RemoteStartTransaction.
func (tcp *TestChargePoint) SetRemoteStartResponse(status types.RemoteStartStopStatus) {
	tcp.mu.Lock()
	defer tcp.mu.Unlock()
	tcp.remoteStartResp = status
}

// SetConfig sets a config key in the test charge point's config store.
func (tcp *TestChargePoint) SetConfig(key, value string) {
	tcp.mu.Lock()
	defer tcp.mu.Unlock()
	tcp.configStore[key] = value
}

// --- core.ChargePointHandler ---

func (tcp *TestChargePoint) OnChangeAvailability(request *core.ChangeAvailabilityRequest) (*core.ChangeAvailabilityConfirmation, error) {
	tcp.log.Debug("received ChangeAvailability", "connector", request.ConnectorId, "type", request.Type)
	return core.NewChangeAvailabilityConfirmation(core.AvailabilityStatusAccepted), nil
}

func (tcp *TestChargePoint) OnChangeConfiguration(request *core.ChangeConfigurationRequest) (*core.ChangeConfigurationConfirmation, error) {
	tcp.log.Debug("received ChangeConfiguration", "key", request.Key, "value", request.Value)
	tcp.mu.Lock()
	tcp.configStore[request.Key] = request.Value
	tcp.mu.Unlock()
	return core.NewChangeConfigurationConfirmation(core.ConfigurationStatusAccepted), nil
}

func (tcp *TestChargePoint) OnClearCache(request *core.ClearCacheRequest) (*core.ClearCacheConfirmation, error) {
	tcp.clearCacheCh <- struct{}{}
	return core.NewClearCacheConfirmation(core.ClearCacheStatusAccepted), nil
}

func (tcp *TestChargePoint) OnDataTransfer(request *core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}

func (tcp *TestChargePoint) OnGetConfiguration(request *core.GetConfigurationRequest) (*core.GetConfigurationConfirmation, error) {
	tcp.mu.Lock()
	defer tcp.mu.Unlock()

	var keys []core.ConfigurationKey
	if len(request.Key) == 0 {
		for k, v := range tcp.configStore {
			val := v
			keys = append(keys, core.ConfigurationKey{Key: k, Readonly: false, Value: &val})
		}
	} else {
		for _, k := range request.Key {
			if v, ok := tcp.configStore[k]; ok {
				val := v
				keys = append(keys, core.ConfigurationKey{Key: k, Readonly: false, Value: &val})
			}
		}
	}
	return core.NewGetConfigurationConfirmation(keys), nil
}

func (tcp *TestChargePoint) OnRemoteStartTransaction(request *core.RemoteStartTransactionRequest) (*core.RemoteStartTransactionConfirmation, error) {
	tcp.mu.Lock()
	resp := tcp.remoteStartResp
	tcp.mu.Unlock()
	tcp.log.Debug("received RemoteStartTransaction", "idTag", request.IdTag, "resp", resp)
	return core.NewRemoteStartTransactionConfirmation(resp), nil
}

func (tcp *TestChargePoint) OnRemoteStopTransaction(request *core.RemoteStopTransactionRequest) (*core.RemoteStopTransactionConfirmation, error) {
	tcp.mu.Lock()
	resp := tcp.remoteStopResp
	tcp.mu.Unlock()
	tcp.log.Debug("received RemoteStopTransaction", "txn", request.TransactionId, "resp", resp)
	return core.NewRemoteStopTransactionConfirmation(resp), nil
}

func (tcp *TestChargePoint) OnReset(request *core.ResetRequest) (*core.ResetConfirmation, error) {
	tcp.resetCh <- request.Type
	return core.NewResetConfirmation(core.ResetStatusAccepted), nil
}

func (tcp *TestChargePoint) OnUnlockConnector(request *core.UnlockConnectorRequest) (*core.UnlockConnectorConfirmation, error) {
	return core.NewUnlockConnectorConfirmation(core.UnlockStatusUnlocked), nil
}

// --- smartcharging.ChargePointHandler ---

func (tcp *TestChargePoint) OnSetChargingProfile(request *smartcharging.SetChargingProfileRequest) (*smartcharging.SetChargingProfileConfirmation, error) {
	tcp.log.Debug("received SetChargingProfile", "connector", request.ConnectorId)
	return smartcharging.NewSetChargingProfileConfirmation(smartcharging.ChargingProfileStatusAccepted), nil
}

func (tcp *TestChargePoint) OnClearChargingProfile(request *smartcharging.ClearChargingProfileRequest) (*smartcharging.ClearChargingProfileConfirmation, error) {
	tcp.log.Debug("received ClearChargingProfile")
	return smartcharging.NewClearChargingProfileConfirmation(smartcharging.ClearChargingProfileStatusAccepted), nil
}

func (tcp *TestChargePoint) OnGetCompositeSchedule(request *smartcharging.GetCompositeScheduleRequest) (*smartcharging.GetCompositeScheduleConfirmation, error) {
	return smartcharging.NewGetCompositeScheduleConfirmation(smartcharging.GetCompositeScheduleStatusAccepted), nil
}

// --- remotetrigger.ChargePointHandler ---

func (tcp *TestChargePoint) OnTriggerMessage(request *remotetrigger.TriggerMessageRequest) (*remotetrigger.TriggerMessageConfirmation, error) {
	tcp.log.Debug("received TriggerMessage", "message", request.RequestedMessage)
	tcp.triggerCh <- request.RequestedMessage
	return remotetrigger.NewTriggerMessageConfirmation(remotetrigger.TriggerMessageStatusAccepted), nil
}

// --- firmware.ChargePointHandler ---

func (tcp *TestChargePoint) OnGetDiagnostics(request *firmware.GetDiagnosticsRequest) (*firmware.GetDiagnosticsConfirmation, error) {
	tcp.log.Debug("received GetDiagnostics", "location", request.Location)
	return firmware.NewGetDiagnosticsConfirmation(), nil
}

func (tcp *TestChargePoint) OnUpdateFirmware(request *firmware.UpdateFirmwareRequest) (*firmware.UpdateFirmwareConfirmation, error) {
	tcp.log.Debug("received UpdateFirmware", "location", request.Location)
	tcp.updateFirmwareCh <- request
	return firmware.NewUpdateFirmwareConfirmation(), nil
}

// WaitReset blocks until a Reset request arrives or timeout elapses.
func (tcp *TestChargePoint) WaitReset(timeout time.Duration) (core.ResetType, error) {
	select {
	case rt := <-tcp.resetCh:
		return rt, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for Reset")
	}
}

// WaitClearCache blocks until a ClearCache request arrives or timeout elapses.
func (tcp *TestChargePoint) WaitClearCache(timeout time.Duration) error {
	select {
	case <-tcp.clearCacheCh:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for ClearCache")
	}
}

// WaitUpdateFirmware blocks until an UpdateFirmware request arrives or timeout elapses.
func (tcp *TestChargePoint) WaitUpdateFirmware(timeout time.Duration) (*firmware.UpdateFirmwareRequest, error) {
	select {
	case req := <-tcp.updateFirmwareCh:
		return req, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout waiting for UpdateFirmware")
	}
}
