package ocpp

import (
	"strconv"
	"sync"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

// Measurement holds a single meter reading.
type Measurement struct {
	Value     float64   `json:"value"`
	Unit      string    `json:"unit,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Connector represents a single charge point connector.
type Connector struct {
	mu            sync.RWMutex
	ID            int                    `json:"id"`
	Status        string                 `json:"status"`
	StatusAt      time.Time              `json:"statusAt,omitempty"`
	ErrorCode     string                 `json:"errorCode,omitempty"`
	TransactionID int                    `json:"transactionId,omitempty"`
	IdTag         string                 `json:"idTag,omitempty"`
	Measurements  map[string]Measurement `json:"measurements,omitempty"`
}

// NewConnector creates a connector with the given ID.
func NewConnector(id int) *Connector {
	return &Connector{
		ID:           id,
		Status:       "Available",
		Measurements: make(map[string]Measurement),
	}
}

// UpdateStatus updates the connector status from a StatusNotification.
func (c *Connector) UpdateStatus(status, errorCode string, ts time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Status = status
	c.ErrorCode = errorCode
	c.StatusAt = ts
}

// UpdateMeterValues parses and stores meter values.
func (c *Connector) UpdateMeterValues(meterValues []types.MeterValue) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, mv := range meterValues {
		var ts time.Time
		if mv.Timestamp != nil {
			ts = mv.Timestamp.Time
		} else {
			ts = time.Now()
		}

		// Track which base measurands were set without a phase in this batch,
		// so we don't overwrite them with an aggregate.
		directBase := make(map[string]bool)

		for _, sv := range mv.SampledValue {
			measurand := string(sv.Measurand)
			if measurand == "" {
				measurand = "Energy.Active.Import.Register"
			}
			if sv.Phase != "" {
				measurand += "." + string(sv.Phase)
			} else {
				directBase[measurand] = true
			}

			val, err := strconv.ParseFloat(sv.Value, 64)
			if err != nil {
				continue
			}

			c.Measurements[measurand] = Measurement{
				Value:     val,
				Unit:      string(sv.Unit),
				Timestamp: ts,
			}
		}

		// Aggregate phase-specific measurands into base keys
		aggregatePhases(c.Measurements, "Power.Active.Import", "sum", ts, directBase)
		aggregatePhases(c.Measurements, "Current.Import", "sum", ts, directBase)
		aggregatePhases(c.Measurements, "Voltage", "avg", ts, directBase)
		aggregatePhases(c.Measurements, "Energy.Active.Import.Register", "sum", ts, directBase)
	}
}

// phases are the OCPP phase suffixes to aggregate.
var phases = []string{".L1", ".L2", ".L3"}

// aggregatePhases computes a base measurand from its phase-specific keys.
// mode is "sum" or "avg". Skips if the base key was already set directly
// (without a phase) in this batch.
func aggregatePhases(m map[string]Measurement, base, mode string, ts time.Time, directBase map[string]bool) {
	if directBase[base] {
		return
	}
	var total float64
	var count int
	var unit string
	for _, suffix := range phases {
		if meas, ok := m[base+suffix]; ok {
			total += meas.Value
			count++
			if unit == "" {
				unit = meas.Unit
			}
		}
	}
	if count == 0 {
		return
	}
	value := total
	if mode == "avg" {
		value = total / float64(count)
	}
	m[base] = Measurement{Value: value, Unit: unit, Timestamp: ts}
}

// StartTransaction marks the connector as having an active transaction.
func (c *Connector) StartTransaction(txnID int, idTag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.TransactionID = txnID
	c.IdTag = idTag
}

// RestoreTransaction sets transaction fields without firing any side effects.
// Used during state hydration on server start.
func (c *Connector) RestoreTransaction(txnID int, idTag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.TransactionID = txnID
	c.IdTag = idTag
}

// StopTransaction clears the active transaction and zeroes power/current.
func (c *Connector) StopTransaction() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.TransactionID = 0
	c.IdTag = ""
	// Zero out power and current readings (energy/voltage keep last values)
	for _, base := range []string{"Power.Active.Import", "Current.Import", "Current.Offered"} {
		// Zero base key and all phase-specific keys
		for _, suffix := range append([]string{""}, phases...) {
			key := base + suffix
			if m, ok := c.Measurements[key]; ok {
				m.Value = 0
				c.Measurements[key] = m
			}
		}
	}
}

// Snapshot returns a read-only copy of connector state.
func (c *Connector) Snapshot() ConnectorSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	measurements := make(map[string]Measurement, len(c.Measurements))
	for k, v := range c.Measurements {
		measurements[k] = v
	}

	return ConnectorSnapshot{
		ID:            c.ID,
		Status:        c.Status,
		StatusAt:      c.StatusAt,
		ErrorCode:     c.ErrorCode,
		TransactionID: c.TransactionID,
		IdTag:         c.IdTag,
		Measurements:  measurements,
	}
}

// ConnectorSnapshot is a serializable snapshot of connector state.
type ConnectorSnapshot struct {
	ID            int                    `json:"id"`
	Status        string                 `json:"status"`
	StatusAt      time.Time              `json:"statusAt,omitempty"`
	ErrorCode     string                 `json:"errorCode,omitempty"`
	TransactionID int                    `json:"transactionId,omitempty"`
	IdTag         string                 `json:"idTag,omitempty"`
	Measurements  map[string]Measurement `json:"measurements,omitempty"`
}
