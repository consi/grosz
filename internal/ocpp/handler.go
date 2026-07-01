package ocpp

import (
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/store"
)

// OnBootNotification handles the BootNotification from a charge point.
func (s *Server) OnBootNotification(chargePointId string, request *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error) {
	s.log.Info("boot notification",
		"id", chargePointId,
		"vendor", request.ChargePointVendor,
		"model", request.ChargePointModel,
		"serial", request.ChargeBoxSerialNumber,
		"firmware", request.FirmwareVersion,
	)

	s.recordEvent("recv", chargePointId, "BootNotification", request)

	cp := s.ChargePoint(chargePointId)
	if cp == nil {
		// Should not happen — SetNewChargePointHandler runs before BootNotification
		s.mu.Lock()
		cp = NewChargePoint(chargePointId)
		s.points[chargePointId] = cp
		s.mu.Unlock()
	}

	cp.SetBootInfo(&BootInfo{
		Vendor:          request.ChargePointVendor,
		Model:           request.ChargePointModel,
		SerialNumber:    request.ChargeBoxSerialNumber,
		FirmwareVersion: request.FirmwareVersion,
		MeterType:       request.MeterType,
	})
	cp.SetConnected(true)

	conf := core.NewBootNotificationConfirmation(types.NewDateTime(timeNow()), 120, core.RegistrationStatusAccepted)
	s.recordEvent("send", chargePointId, "BootNotification", conf)
	s.events.Info(events.ActionChargerBoot,
		map[string]any{"cpID": chargePointId},
		map[string]any{
			"vendor":   request.ChargePointVendor,
			"model":    request.ChargePointModel,
			"serial":   request.ChargeBoxSerialNumber,
			"firmware": request.FirmwareVersion,
		},
	)

	// Fire boot hooks asynchronously
	go s.fireBootHooks(chargePointId, request)

	return conf, nil
}

// OnHeartbeat handles periodic heartbeats.
func (s *Server) OnHeartbeat(chargePointId string, request *core.HeartbeatRequest) (*core.HeartbeatConfirmation, error) {
	s.log.Debug("heartbeat", "id", chargePointId)
	s.recordEvent("recv", chargePointId, "Heartbeat", request)

	conf := core.NewHeartbeatConfirmation(types.NewDateTime(timeNow()))
	return conf, nil
}

// OnStatusNotification handles status changes from a connector.
func (s *Server) OnStatusNotification(chargePointId string, request *core.StatusNotificationRequest) (*core.StatusNotificationConfirmation, error) {
	s.log.Info("status notification",
		"id", chargePointId,
		"connector", request.ConnectorId,
		"status", request.Status,
		"error", request.ErrorCode,
	)

	s.recordEvent("recv", chargePointId, "StatusNotification", request)

	ts := timeNow()
	if request.Timestamp != nil {
		ts = request.Timestamp.Time
	}
	cp := s.ChargePoint(chargePointId)
	if cp != nil {
		conn := cp.GetConnector(request.ConnectorId)
		conn.UpdateStatus(string(request.Status), string(request.ErrorCode), ts)
	}

	// Diff against persisted state so chart markers only fire on real
	// transitions — not on app restarts or websocket reconnects, which
	// would otherwise cause Zappi to re-emit the current status.
	newStatus := string(request.Status)
	if s.store != nil && request.ConnectorId > 0 {
		prev, _ := s.store.GetConnectorState(chargePointId, request.ConnectorId)
		isRealChange := prev == nil || prev.Status != newStatus
		if isRealChange {
			fromStatus := ""
			if prev != nil {
				fromStatus = prev.Status
			}
			s.events.Info(events.ActionConnectorStatusChanged,
				map[string]any{
					"cpID":        chargePointId,
					"connectorID": request.ConnectorId,
					"from":        fromStatus,
					"to":          newStatus,
					"errorCode":   string(request.ErrorCode),
				},
				nil,
			)
			switch request.Status {
			case core.ChargePointStatusPreparing:
				_ = s.store.InsertChartMarker("plug", ts)
				// Plug-in: poll the car (after a settle delay) for a fresh SoC/plug.
				s.firePlugEventHook(chargePointId, request.ConnectorId, fromStatus, newStatus)
			case core.ChargePointStatusAvailable:
				// Don't emit "unplug" if we have no prior state (first ever
				// notification after fresh DB) — we can't claim something was
				// previously plugged.
				if prev != nil && prev.Status != "" && prev.Status != string(core.ChargePointStatusAvailable) {
					_ = s.store.InsertChartMarker("unplug", ts)
					// Unplug: confirm the disconnect and capture the final SoC.
					s.firePlugEventHook(chargePointId, request.ConnectorId, fromStatus, newStatus)
				}
			case core.ChargePointStatusSuspendedEV:
				// Charge-complete (battery full / EV paused): grab the final SoC.
				s.firePlugEventHook(chargePointId, request.ConnectorId, fromStatus, newStatus)
			}
		}

		// Always persist current state + last_status_notification so the
		// stale-state checker can tell if Zappi is silent.
		cs := store.ConnectorState{
			ChargeBox:              chargePointId,
			ConnectorID:            request.ConnectorId,
			Status:                 newStatus,
			StatusAt:               ts,
			ErrorCode:              string(request.ErrorCode),
			LastStatusNotification: timeNow(),
		}
		if prev != nil {
			cs.TransactionID = prev.TransactionID
			cs.IdTag = prev.IdTag
		}
		_ = s.store.UpsertConnectorState(cs)
	}

	// Zappi keeps the OCPP transaction open until unplug; on a full battery the
	// EV reports SuspendedEV and never resumes. Without this, the session would
	// stay status='active' in the DB and never appear in reports. Close it now
	// using the latest meter reading. A later real StopTransaction is a no-op
	// because StopSession's UPDATE is gated on status='active'.
	//
	// Generalized to every "no longer charging" state — vehicle pause, charger
	// pause, finishing wind-down, fault — so the cost log always closes out
	// and the chart always shows a stop marker. Available is excluded: it has
	// its own "unplug" marker and OnStopTransaction handles the marker for it.
	if isStopStatus(request.Status) {
		s.finalizeSessionOnStop(chargePointId, request.ConnectorId, ts)
	}

	s.fireStatusHook()
	s.fireConnectorStatusHook(chargePointId, request.ConnectorId, string(request.Status))
	return core.NewStatusNotificationConfirmation(), nil
}

// isStopStatus returns true for charge-point states that mean charging has
// ceased while the cable is still connected. Available is intentionally not
// included here.
func isStopStatus(status core.ChargePointStatus) bool {
	switch status {
	case core.ChargePointStatusSuspendedEV,
		core.ChargePointStatusSuspendedEVSE,
		core.ChargePointStatusFinishing,
		core.ChargePointStatusFaulted:
		return true
	}
	return false
}

// finalizeSessionOnStop closes the DB session early when the connector reports
// any "no longer charging" state. Idempotent: subsequent calls (or a real
// StopTransaction later) are no-ops because status is no longer 'active'.
//
// Reads the transaction id from connector_state rather than in-memory state so
// it works after a websocket reconnect that wiped the live snapshot.
func (s *Server) finalizeSessionOnStop(cpID string, connID int, stopTime time.Time) {
	if s.store == nil {
		return
	}
	cs, _ := s.store.GetConnectorState(cpID, connID)
	if cs == nil || cs.TransactionID == 0 {
		return
	}
	active, err := s.store.ActiveSession()
	if err != nil || active == nil || active.TransactionID != cs.TransactionID {
		return
	}
	meterStopKWh, ok := s.currentEnergyKWh(cpID, connID, cs.TransactionID)
	if !ok {
		s.recordFinalizeWarn(cpID, cs.TransactionID, "no_energy_reading")
		return
	}
	energy := meterStopKWh - active.MeterStart
	if energy <= 0 {
		s.recordFinalizeWarn(cpID, cs.TransactionID, "nonpositive_energy")
		return
	}
	cost := s.store.CalculateSessionCost(active.StartTime, stopTime, energy)
	if err := s.store.StopSession(cs.TransactionID, stopTime, meterStopKWh, energy, cost); err != nil {
		s.log.Warn("finalize on stop: stop session", "txn", cs.TransactionID, "err", err)
		return
	}
	_ = s.store.InsertChartMarker("stop", stopTime)
	s.log.Info("finalized session on stop status",
		"txn", cs.TransactionID, "energy", energy, "cost", cost, "stopTime", stopTime,
	)
	s.events.Info(events.ActionFinalizeSessionOnStop,
		map[string]any{"txn": cs.TransactionID, "cpID": cpID},
		map[string]any{"energy": energy, "cost": cost, "stopTime": stopTime.UTC().Format(time.RFC3339)},
	)
	if s.socUpdater != nil {
		s.socUpdater(energy)
	}
}

// currentEnergyKWh resolves the latest Energy.Active.Import.Register reading
// for a connector. Prefers the in-memory snapshot (freshest); falls back to
// parsing recent ocpp_events, which keeps finalize working when in-memory
// measurements have been wiped by a websocket reconnect.
func (s *Server) currentEnergyKWh(cpID string, connID, txnID int) (float64, bool) {
	if cp := s.ChargePoint(cpID); cp != nil {
		if conn := cp.GetConnector(connID); conn != nil {
			snap := conn.Snapshot()
			if meas, ok := snap.Measurements["Energy.Active.Import.Register"]; ok {
				return meas.Value / 1000.0, true
			}
			// Zappi reports the register with phase=N — the aggregator skips
			// it (only L1/L2/L3 sum to the base key), so fall back manually.
			if meas, ok := snap.Measurements["Energy.Active.Import.Register.N"]; ok {
				return meas.Value / 1000.0, true
			}
		}
	}
	if s.store != nil {
		if kwh, _, ok := s.store.LatestEnergyForTransaction(txnID); ok {
			return kwh, true
		}
	}
	return 0, false
}

func (s *Server) recordFinalizeWarn(cpID string, txnID int, reason string) {
	s.log.Warn("finalize on stop: skipped", "txn", txnID, "reason", reason)
	if s.store == nil {
		return
	}
	s.events.Warn(events.ActionFinalizeSessionOnStop,
		map[string]any{"txn": txnID, "cpID": cpID},
		map[string]any{"skipped": reason},
	)
}

// OnMeterValues handles meter value readings.
func (s *Server) OnMeterValues(chargePointId string, request *core.MeterValuesRequest) (*core.MeterValuesConfirmation, error) {
	s.log.Debug("meter values",
		"id", chargePointId,
		"connector", request.ConnectorId,
		"samples", len(request.MeterValue),
	)

	s.recordEvent("recv", chargePointId, "MeterValues", request)

	cp := s.ChargePoint(chargePointId)
	if cp != nil {
		conn := cp.GetConnector(request.ConnectorId)
		conn.UpdateMeterValues(request.MeterValue)
	}

	s.fireStatusHook()
	return core.NewMeterValuesConfirmation(), nil
}

// OnAuthorize handles authorization requests. Always accepts (Zappi uses virtual tags).
func (s *Server) OnAuthorize(chargePointId string, request *core.AuthorizeRequest) (*core.AuthorizeConfirmation, error) {
	s.log.Info("authorize", "id", chargePointId, "idTag", request.IdTag)

	s.recordEvent("recv", chargePointId, "Authorize", request)

	return core.NewAuthorizationConfirmation(types.NewIdTagInfo(types.AuthorizationStatusAccepted)), nil
}

// OnStartTransaction handles the start of a charging session.
func (s *Server) OnStartTransaction(chargePointId string, request *core.StartTransactionRequest) (*core.StartTransactionConfirmation, error) {
	txnID := s.allocateTxnID()

	s.log.Info("start transaction",
		"id", chargePointId,
		"connector", request.ConnectorId,
		"txn", txnID,
		"idTag", request.IdTag,
		"meterStart", request.MeterStart,
	)

	s.recordEvent("recv", chargePointId, "StartTransaction", request)

	cp := s.ChargePoint(chargePointId)
	if cp != nil {
		conn := cp.GetConnector(request.ConnectorId)
		conn.StartTransaction(txnID, request.IdTag)
	}

	// Persist session
	if s.store != nil {
		var startTime time.Time
		if request.Timestamp != nil {
			startTime = request.Timestamp.Time
		} else {
			startTime = timeNow()
		}
		_ = s.store.StartSession(store.Session{
			ChargeBox:     chargePointId,
			ConnectorID:   request.ConnectorId,
			TransactionID: txnID,
			IdTag:         request.IdTag,
			StartTime:     startTime,
			MeterStart:    float64(request.MeterStart) / 1000.0, // Wh to kWh
		})

		// Track transaction in connector_state so it survives a restart.
		prev, _ := s.store.GetConnectorState(chargePointId, request.ConnectorId)
		cs := store.ConnectorState{
			ChargeBox:     chargePointId,
			ConnectorID:   request.ConnectorId,
			TransactionID: txnID,
			IdTag:         request.IdTag,
		}
		if prev != nil {
			cs.Status = prev.Status
			cs.StatusAt = prev.StatusAt
			cs.ErrorCode = prev.ErrorCode
			cs.LastStatusNotification = prev.LastStatusNotification
		}
		_ = s.store.UpsertConnectorState(cs)
	}

	conf := core.NewStartTransactionConfirmation(
		types.NewIdTagInfo(types.AuthorizationStatusAccepted),
		txnID,
	)
	s.recordEvent("send", chargePointId, "StartTransaction", conf)
	s.events.Info(events.ActionTransactionStarted,
		map[string]any{"cpID": chargePointId, "connectorID": request.ConnectorId, "idTag": request.IdTag},
		map[string]any{"txnID": txnID, "meterStartWh": request.MeterStart},
	)
	s.fireStatusHook()
	return conf, nil
}

// OnStopTransaction handles the end of a charging session.
func (s *Server) OnStopTransaction(chargePointId string, request *core.StopTransactionRequest) (*core.StopTransactionConfirmation, error) {
	s.log.Info("stop transaction",
		"id", chargePointId,
		"txn", request.TransactionId,
		"meterStop", request.MeterStop,
		"reason", request.Reason,
	)

	s.recordEvent("recv", chargePointId, "StopTransaction", request)

	// Find and clear the connector
	var stoppedConnID int
	cp := s.ChargePoint(chargePointId)
	if cp != nil {
		cp.mu.RLock()
		for _, conn := range cp.Connectors {
			snap := conn.Snapshot()
			if snap.TransactionID == request.TransactionId {
				stoppedConnID = snap.ID
				conn.StopTransaction()
				break
			}
		}
		cp.mu.RUnlock()
	}
	if s.store != nil && stoppedConnID > 0 {
		// Clear transaction info in persisted state. Preserve status fields.
		prev, _ := s.store.GetConnectorState(chargePointId, stoppedConnID)
		cs := store.ConnectorState{
			ChargeBox:   chargePointId,
			ConnectorID: stoppedConnID,
		}
		if prev != nil {
			cs.Status = prev.Status
			cs.StatusAt = prev.StatusAt
			cs.ErrorCode = prev.ErrorCode
			cs.LastStatusNotification = prev.LastStatusNotification
		}
		_ = s.store.UpsertConnectorState(cs)
	}

	// Complete session in store
	if s.store != nil {
		var stopTime time.Time
		if request.Timestamp != nil {
			stopTime = request.Timestamp.Time
		} else {
			stopTime = timeNow()
		}
		meterStopKWh := float64(request.MeterStop) / 1000.0

		// Look up start meter to calculate energy and cost
		active, _ := s.store.ActiveSession()
		var energy, cost float64
		if active != nil && active.TransactionID == request.TransactionId {
			energy = meterStopKWh - active.MeterStart
			cost = s.store.CalculateSessionCost(active.StartTime, stopTime, energy)
		}
		stopErr := s.store.StopSession(request.TransactionId, stopTime, meterStopKWh, energy, cost)
		// Only emit the chart marker when this StopTransaction was the one that
		// actually closed the session — if finalizeSessionOnStop ran first, the
		// session is already 'completed' and we'd double-mark.
		if stopErr == nil {
			_ = s.store.InsertChartMarker("stop", stopTime)
		}

		// Update SoC estimate
		if s.socUpdater != nil && energy > 0 {
			s.socUpdater(energy)
		}
	}

	conf := core.NewStopTransactionConfirmation()
	conf.IdTagInfo = types.NewIdTagInfo(types.AuthorizationStatusAccepted)
	s.recordEvent("send", chargePointId, "StopTransaction", conf)
	s.events.Info(events.ActionTransactionStopped,
		map[string]any{"cpID": chargePointId, "txnID": request.TransactionId, "reason": string(request.Reason)},
		map[string]any{"meterStopWh": request.MeterStop},
	)
	s.fireStatusHook()
	s.fireTransactionEndedHook(chargePointId, stoppedConnID, request.TransactionId)
	return conf, nil
}

// OnDataTransfer handles vendor-specific data transfer.
func (s *Server) OnDataTransfer(chargePointId string, request *core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	s.log.Debug("data transfer",
		"id", chargePointId,
		"vendorId", request.VendorId,
		"messageId", request.MessageId,
	)

	s.recordEvent("recv", chargePointId, "DataTransfer", request)

	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}
