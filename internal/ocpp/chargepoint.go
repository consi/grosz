package ocpp

import (
	"sync"
	"time"
)

// BootInfo holds charger identification from BootNotification.
type BootInfo struct {
	Vendor          string `json:"vendor"`
	Model           string `json:"model"`
	SerialNumber    string `json:"serialNumber,omitempty"`
	FirmwareVersion string `json:"firmwareVersion,omitempty"`
	MeterType       string `json:"meterType,omitempty"`
}

// ChargePoint represents a connected OCPP charge point.
type ChargePoint struct {
	mu          sync.RWMutex
	ID          string                `json:"id"`
	Connected   bool                  `json:"connected"`
	ConnectedAt time.Time             `json:"connectedAt,omitempty"`
	BootInfo    *BootInfo             `json:"bootInfo,omitempty"`
	Connectors  map[int]*Connector    `json:"connectors"`
}

// NewChargePoint creates a new ChargePoint in connected state.
func NewChargePoint(id string) *ChargePoint {
	return &ChargePoint{
		ID:          id,
		Connected:   true,
		ConnectedAt: time.Now(),
		Connectors:  make(map[int]*Connector),
	}
}

// SetConnected updates the connection state.
func (cp *ChargePoint) SetConnected(connected bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.Connected = connected
	if connected {
		cp.ConnectedAt = time.Now()
	}
}

// IsConnected returns the connection state.
func (cp *ChargePoint) IsConnected() bool {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	return cp.Connected
}

// SetBootInfo stores the boot notification data.
func (cp *ChargePoint) SetBootInfo(info *BootInfo) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.BootInfo = info
}

// GetConnector returns the connector for the given ID, creating it if needed.
func (cp *ChargePoint) GetConnector(id int) *Connector {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	c, ok := cp.Connectors[id]
	if !ok {
		c = NewConnector(id)
		cp.Connectors[id] = c
	}
	return c
}

// Snapshot returns a read-only copy of the charge point state.
func (cp *ChargePoint) Snapshot() ChargePointSnapshot {
	cp.mu.RLock()
	defer cp.mu.RUnlock()

	snap := ChargePointSnapshot{
		ID:          cp.ID,
		Connected:   cp.Connected,
		ConnectedAt: cp.ConnectedAt,
		BootInfo:    cp.BootInfo,
		Connectors:  make(map[int]ConnectorSnapshot),
	}
	for id, c := range cp.Connectors {
		snap.Connectors[id] = c.Snapshot()
	}
	return snap
}

// ChargePointSnapshot is a serializable snapshot of charge point state.
type ChargePointSnapshot struct {
	ID          string                     `json:"id"`
	Connected   bool                       `json:"connected"`
	ConnectedAt time.Time                  `json:"connectedAt,omitempty"`
	BootInfo    *BootInfo                  `json:"bootInfo,omitempty"`
	Connectors  map[int]ConnectorSnapshot  `json:"connectors"`
}
