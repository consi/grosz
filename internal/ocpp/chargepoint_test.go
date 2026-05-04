package ocpp

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChargePointConnectionState(t *testing.T) {
	cp := NewChargePoint("CP001")
	assert.True(t, cp.IsConnected())

	cp.SetConnected(false)
	assert.False(t, cp.IsConnected())

	cp.SetConnected(true)
	assert.True(t, cp.IsConnected())
}

func TestChargePointBootInfo(t *testing.T) {
	cp := NewChargePoint("CP001")
	assert.Nil(t, cp.BootInfo)

	info := &BootInfo{Vendor: "Myenergi", Model: "Zappi", SerialNumber: "S123"}
	cp.SetBootInfo(info)

	cp.mu.RLock()
	assert.Equal(t, "Myenergi", cp.BootInfo.Vendor)
	cp.mu.RUnlock()
}

func TestChargePointGetConnector(t *testing.T) {
	cp := NewChargePoint("CP001")

	c1 := cp.GetConnector(1)
	assert.Equal(t, 1, c1.ID)

	// Same connector returned
	c1Again := cp.GetConnector(1)
	assert.Same(t, c1, c1Again)

	// Different connector
	c2 := cp.GetConnector(2)
	assert.Equal(t, 2, c2.ID)
	assert.NotSame(t, c1, c2)
}

func TestChargePointSnapshot(t *testing.T) {
	cp := NewChargePoint("CP001")
	cp.SetBootInfo(&BootInfo{Vendor: "V", Model: "M"})
	cp.GetConnector(1).UpdateStatus("Charging", "NoError", timeNow())

	snap := cp.Snapshot()
	assert.Equal(t, "CP001", snap.ID)
	assert.True(t, snap.Connected)
	assert.Equal(t, "Charging", snap.Connectors[1].Status)
}

func TestChargePointConcurrentAccess(t *testing.T) {
	cp := NewChargePoint("CP001")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			cp.SetConnected(true)
		}()
		go func() {
			defer wg.Done()
			cp.IsConnected()
		}()
		go func() {
			defer wg.Done()
			cp.Snapshot()
		}()
	}
	wg.Wait()
}
