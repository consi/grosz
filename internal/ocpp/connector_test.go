package ocpp

import (
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/stretchr/testify/assert"
)

func TestConnectorUpdateStatus(t *testing.T) {
	c := NewConnector(1)
	assert.Equal(t, "Available", c.Snapshot().Status)

	now := time.Now()
	c.UpdateStatus("Charging", "NoError", now)

	snap := c.Snapshot()
	assert.Equal(t, "Charging", snap.Status)
	assert.Equal(t, "NoError", snap.ErrorCode)
	assert.Equal(t, now, snap.StatusAt)
}

func TestConnectorUpdateMeterValues(t *testing.T) {
	c := NewConnector(1)
	now := types.NewDateTime(time.Now())

	c.UpdateMeterValues([]types.MeterValue{
		{
			Timestamp: now,
			SampledValue: []types.SampledValue{
				{Value: "7360", Measurand: types.MeasurandPowerActiveImport, Unit: types.UnitOfMeasureW},
				{Value: "16.0", Measurand: types.MeasurandCurrentImport, Unit: types.UnitOfMeasureA},
				{Value: "230", Measurand: types.MeasurandVoltage, Unit: types.UnitOfMeasureV},
			},
		},
	})

	snap := c.Snapshot()
	assert.InDelta(t, 7360, snap.Measurements["Power.Active.Import"].Value, 0.1)
	assert.InDelta(t, 16.0, snap.Measurements["Current.Import"].Value, 0.1)
	assert.InDelta(t, 230, snap.Measurements["Voltage"].Value, 0.1)
}

func TestConnectorMeterValuesDefaultMeasurand(t *testing.T) {
	c := NewConnector(1)
	now := types.NewDateTime(time.Now())

	// Empty measurand defaults to Energy.Active.Import.Register
	c.UpdateMeterValues([]types.MeterValue{
		{
			Timestamp: now,
			SampledValue: []types.SampledValue{
				{Value: "12345"},
			},
		},
	})

	snap := c.Snapshot()
	assert.InDelta(t, 12345, snap.Measurements["Energy.Active.Import.Register"].Value, 0.1)
}

func TestConnectorStartStopTransaction(t *testing.T) {
	c := NewConnector(1)

	c.StartTransaction(42, "grosz")
	snap := c.Snapshot()
	assert.Equal(t, 42, snap.TransactionID)
	assert.Equal(t, "grosz", snap.IdTag)

	// Add some meter values
	now := types.NewDateTime(time.Now())
	c.UpdateMeterValues([]types.MeterValue{
		{Timestamp: now, SampledValue: []types.SampledValue{
			{Value: "7000", Measurand: types.MeasurandPowerActiveImport},
			{Value: "230", Measurand: types.MeasurandVoltage},
		}},
	})

	c.StopTransaction()
	snap = c.Snapshot()
	assert.Equal(t, 0, snap.TransactionID)
	assert.Equal(t, "", snap.IdTag)

	// Power should be zeroed, voltage should remain
	assert.InDelta(t, 0, snap.Measurements["Power.Active.Import"].Value, 0.1)
	assert.InDelta(t, 230, snap.Measurements["Voltage"].Value, 0.1)
}

func TestConnectorPhaseMeasurands(t *testing.T) {
	c := NewConnector(1)
	now := types.NewDateTime(time.Now())

	c.UpdateMeterValues([]types.MeterValue{
		{
			Timestamp: now,
			SampledValue: []types.SampledValue{
				{Value: "230", Measurand: types.MeasurandVoltage, Phase: types.PhaseL1},
				{Value: "231", Measurand: types.MeasurandVoltage, Phase: types.PhaseL2},
				{Value: "229", Measurand: types.MeasurandVoltage, Phase: types.PhaseL3},
			},
		},
	})

	snap := c.Snapshot()
	assert.InDelta(t, 230, snap.Measurements["Voltage.L1"].Value, 0.1)
	assert.InDelta(t, 231, snap.Measurements["Voltage.L2"].Value, 0.1)
	assert.InDelta(t, 229, snap.Measurements["Voltage.L3"].Value, 0.1)
}

func TestConnectorPhaseAggregation(t *testing.T) {
	c := NewConnector(1)
	now := types.NewDateTime(time.Now())

	c.UpdateMeterValues([]types.MeterValue{
		{
			Timestamp: now,
			SampledValue: []types.SampledValue{
				{Value: "3700", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL1, Unit: types.UnitOfMeasureW},
				{Value: "3800", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL2, Unit: types.UnitOfMeasureW},
				{Value: "3730", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL3, Unit: types.UnitOfMeasureW},
				{Value: "16.1", Measurand: types.MeasurandCurrentImport, Phase: types.PhaseL1, Unit: types.UnitOfMeasureA},
				{Value: "16.5", Measurand: types.MeasurandCurrentImport, Phase: types.PhaseL2, Unit: types.UnitOfMeasureA},
				{Value: "16.2", Measurand: types.MeasurandCurrentImport, Phase: types.PhaseL3, Unit: types.UnitOfMeasureA},
				{Value: "230", Measurand: types.MeasurandVoltage, Phase: types.PhaseL1, Unit: types.UnitOfMeasureV},
				{Value: "231", Measurand: types.MeasurandVoltage, Phase: types.PhaseL2, Unit: types.UnitOfMeasureV},
				{Value: "229", Measurand: types.MeasurandVoltage, Phase: types.PhaseL3, Unit: types.UnitOfMeasureV},
			},
		},
	})

	snap := c.Snapshot()

	// Phase-specific keys should exist
	assert.InDelta(t, 3700, snap.Measurements["Power.Active.Import.L1"].Value, 0.1)
	assert.InDelta(t, 3800, snap.Measurements["Power.Active.Import.L2"].Value, 0.1)
	assert.InDelta(t, 3730, snap.Measurements["Power.Active.Import.L3"].Value, 0.1)

	// Aggregate Power should be sum of phases
	assert.InDelta(t, 11230, snap.Measurements["Power.Active.Import"].Value, 0.1)

	// Aggregate Current should be sum of phases
	assert.InDelta(t, 48.8, snap.Measurements["Current.Import"].Value, 0.1)

	// Aggregate Voltage should be average of phases
	assert.InDelta(t, 230, snap.Measurements["Voltage"].Value, 0.5)
}

func TestConnectorPhaseAggregationSkipsDirectBase(t *testing.T) {
	c := NewConnector(1)
	now := types.NewDateTime(time.Now())

	// If both base and phase-specific values are sent, base should not be overwritten
	c.UpdateMeterValues([]types.MeterValue{
		{
			Timestamp: now,
			SampledValue: []types.SampledValue{
				{Value: "11000", Measurand: types.MeasurandPowerActiveImport, Unit: types.UnitOfMeasureW},
				{Value: "3700", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL1, Unit: types.UnitOfMeasureW},
				{Value: "3800", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL2, Unit: types.UnitOfMeasureW},
			},
		},
	})

	snap := c.Snapshot()
	// Direct base value should be preserved, not overwritten by aggregate
	assert.InDelta(t, 11000, snap.Measurements["Power.Active.Import"].Value, 0.1)
}

func TestConnectorStopTransactionClearsPhaseKeys(t *testing.T) {
	c := NewConnector(1)
	now := types.NewDateTime(time.Now())

	c.StartTransaction(42, "grosz")
	c.UpdateMeterValues([]types.MeterValue{
		{
			Timestamp: now,
			SampledValue: []types.SampledValue{
				{Value: "3700", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL1, Unit: types.UnitOfMeasureW},
				{Value: "3800", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL2, Unit: types.UnitOfMeasureW},
				{Value: "3730", Measurand: types.MeasurandPowerActiveImport, Phase: types.PhaseL3, Unit: types.UnitOfMeasureW},
				{Value: "230", Measurand: types.MeasurandVoltage, Phase: types.PhaseL1, Unit: types.UnitOfMeasureV},
			},
		},
	})

	c.StopTransaction()
	snap := c.Snapshot()

	// Power phase keys should be zeroed
	assert.InDelta(t, 0, snap.Measurements["Power.Active.Import"].Value, 0.1)
	assert.InDelta(t, 0, snap.Measurements["Power.Active.Import.L1"].Value, 0.1)
	assert.InDelta(t, 0, snap.Measurements["Power.Active.Import.L2"].Value, 0.1)
	assert.InDelta(t, 0, snap.Measurements["Power.Active.Import.L3"].Value, 0.1)

	// Voltage should remain
	assert.InDelta(t, 230, snap.Measurements["Voltage.L1"].Value, 0.1)
}

func TestConnectorInvalidMeterValue(t *testing.T) {
	c := NewConnector(1)
	now := types.NewDateTime(time.Now())

	c.UpdateMeterValues([]types.MeterValue{
		{
			Timestamp: now,
			SampledValue: []types.SampledValue{
				{Value: "not-a-number", Measurand: types.MeasurandPowerActiveImport},
			},
		},
	})

	snap := c.Snapshot()
	_, exists := snap.Measurements["Power.Active.Import"]
	assert.False(t, exists)
}
