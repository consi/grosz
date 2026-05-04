package ocpp

import (
	"fmt"
	"strings"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

const (
	requestTimeout = 30 * time.Second
	maxRetries     = 2
	retryDelay     = 5 * time.Second
)

func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "timeout") || strings.Contains(s, "timed out")
}

func (s *Server) withRetry(action string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil || !isTimeoutErr(lastErr) {
			return lastErr
		}
		if attempt < maxRetries {
			s.log.Warn("OCPP request timed out, retrying",
				"action", action, "attempt", attempt+1, "maxRetries", maxRetries)
			time.Sleep(retryDelay)
		}
	}
	return lastErr
}

// ChangeConfiguration sends a ChangeConfiguration request and waits for the response.
func (s *Server) ChangeConfiguration(cpID, key, value string) error {
	return s.withRetry("ChangeConfiguration", func() error {
		rc := make(chan error, 1)
		err := s.cs.ChangeConfiguration(cpID, func(conf *core.ChangeConfigurationConfirmation, err error) {
			if err != nil {
				rc <- err
				return
			}
			if conf.Status != core.ConfigurationStatusAccepted {
				rc <- fmt.Errorf("ChangeConfiguration %s: %s", key, conf.Status)
				return
			}
			rc <- nil
		}, key, value)
		if err != nil {
			return fmt.Errorf("send ChangeConfiguration: %w", err)
		}

		s.recordEvent("send", cpID, "ChangeConfiguration", map[string]string{"key": key, "value": value})

		select {
		case err = <-rc:
			return err
		case <-time.After(requestTimeout):
			return fmt.Errorf("timeout waiting for ChangeConfiguration %s", key)
		}
	})
}

// GetConfiguration sends a GetConfiguration request and waits for the response.
func (s *Server) GetConfiguration(cpID string, keys []string) (map[string]string, error) {
	type result struct {
		conf map[string]string
		err  error
	}
	rc := make(chan result, 1)

	err := s.cs.GetConfiguration(cpID, func(conf *core.GetConfigurationConfirmation, err error) {
		if err != nil {
			rc <- result{err: err}
			return
		}
		m := make(map[string]string)
		if conf.ConfigurationKey != nil {
			for _, kv := range conf.ConfigurationKey {
				if kv.Value != nil {
					m[kv.Key] = *kv.Value
				}
			}
		}
		rc <- result{conf: m}
	}, keys)
	if err != nil {
		return nil, fmt.Errorf("send GetConfiguration: %w", err)
	}

	s.recordEvent("send", cpID, "GetConfiguration", map[string]any{"keys": keys})

	select {
	case r := <-rc:
		return r.conf, r.err
	case <-time.After(requestTimeout):
		return nil, fmt.Errorf("timeout waiting for GetConfiguration")
	}
}

// RemoteStartTransaction sends a RemoteStartTransaction and waits for response.
// No retry: the myenergi cloud proxy queues commands, so retries cause duplicates.
func (s *Server) RemoteStartTransaction(cpID, idTag string, connectorID int) error {
	rc := make(chan error, 1)
	err := s.cs.RemoteStartTransaction(cpID, func(conf *core.RemoteStartTransactionConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		if conf.Status != types.RemoteStartStopStatusAccepted {
			rc <- fmt.Errorf("RemoteStartTransaction: %s", conf.Status)
			return
		}
		rc <- nil
	}, idTag, func(req *core.RemoteStartTransactionRequest) {
		req.ConnectorId = &connectorID
	})
	if err != nil {
		return fmt.Errorf("send RemoteStartTransaction: %w", err)
	}

	s.recordEvent("send", cpID, "RemoteStartTransaction", map[string]any{"idTag": idTag, "connectorId": connectorID})

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for RemoteStartTransaction")
	}
}

// RemoteStopTransaction sends a RemoteStopTransaction and waits for response.
// No retry: the myenergi cloud proxy queues commands, so retries cause duplicates.
func (s *Server) RemoteStopTransaction(cpID string, transactionID int) error {
	rc := make(chan error, 1)
	err := s.cs.RemoteStopTransaction(cpID, func(conf *core.RemoteStopTransactionConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		if conf.Status != types.RemoteStartStopStatusAccepted {
			rc <- fmt.Errorf("RemoteStopTransaction: %s", conf.Status)
			return
		}
		rc <- nil
	}, transactionID)
	if err != nil {
		return fmt.Errorf("send RemoteStopTransaction: %w", err)
	}

	s.recordEvent("send", cpID, "RemoteStopTransaction", map[string]any{"transactionId": transactionID})

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for RemoteStopTransaction")
	}
}

// SetChargingProfile sends a SetChargingProfile and waits for response.
// No retry: the myenergi cloud proxy queues commands, so retries cause duplicates.
func (s *Server) SetChargingProfile(cpID string, connectorID int, profile *types.ChargingProfile) error {
	rc := make(chan error, 1)
	err := s.cs.SetChargingProfile(cpID, func(conf *smartcharging.SetChargingProfileConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		if conf.Status != smartcharging.ChargingProfileStatusAccepted {
			rc <- fmt.Errorf("SetChargingProfile: %s", conf.Status)
			return
		}
		rc <- nil
	}, connectorID, profile)
	if err != nil {
		return fmt.Errorf("send SetChargingProfile: %w", err)
	}

	s.recordEvent("send", cpID, "SetChargingProfile", map[string]any{"connectorId": connectorID})

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for SetChargingProfile")
	}
}

// ClearChargingProfile sends a ClearChargingProfile and waits for response.
// No retry: the myenergi cloud proxy queues commands, so retries cause duplicates.
func (s *Server) ClearChargingProfile(cpID string) error {
	rc := make(chan error, 1)
	err := s.cs.ClearChargingProfile(cpID, func(conf *smartcharging.ClearChargingProfileConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		// ClearChargingProfile may return "Unknown" if no profile to clear, which is fine
		rc <- nil
	})
	if err != nil {
		return fmt.Errorf("send ClearChargingProfile: %w", err)
	}

	s.recordEvent("send", cpID, "ClearChargingProfile", nil)

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for ClearChargingProfile")
	}
}

// ChangeAvailability sends a ChangeAvailability and waits for response.
func (s *Server) ChangeAvailability(cpID string, connectorID int, availType core.AvailabilityType) error {
	rc := make(chan error, 1)
	err := s.cs.ChangeAvailability(cpID, func(conf *core.ChangeAvailabilityConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		if conf.Status == core.AvailabilityStatusRejected {
			rc <- fmt.Errorf("ChangeAvailability: rejected")
			return
		}
		rc <- nil
	}, connectorID, availType)
	if err != nil {
		return fmt.Errorf("send ChangeAvailability: %w", err)
	}

	s.recordEvent("send", cpID, "ChangeAvailability", map[string]any{"connectorId": connectorID, "type": availType})

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for ChangeAvailability")
	}
}

// Reset sends a Reset request (Hard or Soft) and waits for response.
// No retry: the myenergi cloud proxy queues commands.
func (s *Server) Reset(cpID string, resetType core.ResetType) error {
	rc := make(chan error, 1)
	err := s.cs.Reset(cpID, func(conf *core.ResetConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		if conf.Status != core.ResetStatusAccepted {
			rc <- fmt.Errorf("Reset: %s", conf.Status)
			return
		}
		rc <- nil
	}, resetType)
	if err != nil {
		return fmt.Errorf("send Reset: %w", err)
	}

	s.recordEvent("send", cpID, "Reset", map[string]any{"type": resetType})

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for Reset")
	}
}

// ClearCache sends a ClearCache request and waits for response.
// No retry: the myenergi cloud proxy queues commands.
func (s *Server) ClearCache(cpID string) error {
	rc := make(chan error, 1)
	err := s.cs.ClearCache(cpID, func(conf *core.ClearCacheConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		if conf.Status != core.ClearCacheStatusAccepted {
			rc <- fmt.Errorf("ClearCache: %s", conf.Status)
			return
		}
		rc <- nil
	})
	if err != nil {
		return fmt.Errorf("send ClearCache: %w", err)
	}

	s.recordEvent("send", cpID, "ClearCache", nil)

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for ClearCache")
	}
}

// UpdateFirmware sends an UpdateFirmware request. retrieveDate is forced to
// UTC since Zappi requires it. The location URL is required by the OCPP 1.6
// schema but is ignored by Zappi (firmware always comes from myenergi servers).
// No retry: the myenergi cloud proxy queues commands.
func (s *Server) UpdateFirmware(cpID, location string, retrieveDate time.Time) error {
	rc := make(chan error, 1)
	utcDate := types.NewDateTime(retrieveDate.UTC())
	err := s.cs.UpdateFirmware(cpID, func(conf *firmware.UpdateFirmwareConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		// UpdateFirmwareConfirmation has no fields; receipt of any non-error
		// response is acceptance.
		_ = conf
		rc <- nil
	}, location, utcDate)
	if err != nil {
		return fmt.Errorf("send UpdateFirmware: %w", err)
	}

	s.recordEvent("send", cpID, "UpdateFirmware", map[string]any{
		"location":     location,
		"retrieveDate": utcDate.FormatTimestamp(),
	})

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for UpdateFirmware")
	}
}

// TriggerMessage sends a TriggerMessage and waits for response.
func (s *Server) TriggerMessage(cpID string, requestedMessage remotetrigger.MessageTrigger) error {
	rc := make(chan error, 1)
	err := s.cs.TriggerMessage(cpID, func(conf *remotetrigger.TriggerMessageConfirmation, err error) {
		if err != nil {
			rc <- err
			return
		}
		if conf.Status == remotetrigger.TriggerMessageStatusRejected {
			rc <- fmt.Errorf("TriggerMessage %s: rejected", requestedMessage)
			return
		}
		rc <- nil
	}, requestedMessage)
	if err != nil {
		return fmt.Errorf("send TriggerMessage: %w", err)
	}

	s.recordEvent("send", cpID, "TriggerMessage", map[string]any{"message": requestedMessage})

	select {
	case err = <-rc:
		return err
	case <-time.After(requestTimeout):
		return fmt.Errorf("timeout waiting for TriggerMessage %s", requestedMessage)
	}
}
