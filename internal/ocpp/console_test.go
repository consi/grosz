package ocpp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsoleTemplatesParse(t *testing.T) {
	actions := ConsoleActions()
	require.Len(t, actions, len(consoleTemplates), "every template maps to a registered feature")
	for _, a := range actions {
		_, err := ParseConsoleRequest(a.Action, a.Template)
		assert.NoError(t, err, a.Action)
		assert.NotEmpty(t, a.Profile, a.Action)
	}
}

func TestConsoleRejectsUnknownFieldsAndActions(t *testing.T) {
	_, err := ParseConsoleRequest("Reset", json.RawMessage(`{"type":"Soft","bogus":1}`))
	assert.ErrorContains(t, err, "bogus")
	_, err = ParseConsoleRequest("Reset", json.RawMessage(`{"type":`))
	assert.Error(t, err)
	_, err = ParseConsoleRequest("BootNotification", json.RawMessage(`{}`))
	assert.ErrorContains(t, err, "not a Central System to Charge Point action")
}

func TestConsoleFieldHints(t *testing.T) {
	var reset, scp ConsoleAction
	for _, a := range ConsoleActions() {
		switch a.Action {
		case "Reset":
			reset = a
		case "SetChargingProfile":
			scp = a
		}
	}
	require.NotEmpty(t, reset.Fields)
	assert.Equal(t, ConsoleField{Path: "type", Type: "string", Required: true, Enum: []string{"Hard", "Soft"}}, reset.Fields[0])
	paths := map[string]ConsoleField{}
	for _, f := range scp.Fields {
		paths[f.Path] = f
	}
	assert.Contains(t, paths, "csChargingProfiles.chargingSchedule.chargingSchedulePeriod[].limit")
	assert.Equal(t, []string{"W", "A"}, paths["csChargingProfiles.chargingSchedule.chargingRateUnit"].Enum)
}
