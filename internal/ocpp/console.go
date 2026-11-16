package ocpp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	ocppcore "github.com/lorenzodonini/ocpp-go/ocpp"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/localauth"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/reservation"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
)

// consoleTimeout is generous: the myenergi cloud proxy relays slowly.
const consoleTimeout = 60 * time.Second

// ConsoleAction describes a Central System -> Charge Point request the debug
// console can send, with a ready-to-edit template and field hints.
type ConsoleAction struct {
	Action   string          `json:"action"`
	Profile  string          `json:"profile"`
	Template json.RawMessage `json:"template"`
	Fields   []ConsoleField  `json:"fields"`
}

// ConsoleField is one (possibly nested, dotted) request field.
type ConsoleField struct {
	Path     string   `json:"path"`
	Type     string   `json:"type"`
	Required bool     `json:"required,omitempty"`
	Enum     []string `json:"enum,omitempty"`
}

// ConsoleResult is the outcome of a console request.
type ConsoleResult struct {
	Action     string `json:"action"`
	Response   any    `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// consoleProfiles lists the OCPP 1.6J profiles registered by the central
// system, in display order.
var consoleProfiles = []*ocppcore.Profile{
	core.Profile, smartcharging.Profile, remotetrigger.Profile,
	firmware.Profile, localauth.Profile, reservation.Profile,
}

// consoleTemplates are example payloads for every sendable action.
var consoleTemplates = map[string]string{
	"ChangeAvailability":     `{"connectorId": 1, "type": "Operative"}`,
	"ChangeConfiguration":    `{"key": "MeterValueSampleInterval", "value": "10"}`,
	"ClearCache":             `{}`,
	"DataTransfer":           `{"vendorId": "myenergi", "messageId": "", "data": ""}`,
	"GetConfiguration":       `{"key": ["ChargingScheduleMaxPeriods"]}`,
	"RemoteStartTransaction": `{"connectorId": 1, "idTag": "grosz"}`,
	"RemoteStopTransaction":  `{"transactionId": 1}`,
	"Reset":                  `{"type": "Soft"}`,
	"UnlockConnector":        `{"connectorId": 1}`,
	"SetChargingProfile": `{"connectorId": 1, "csChargingProfiles": {"chargingProfileId": 1, "stackLevel": 0,
  "chargingProfilePurpose": "TxDefaultProfile", "chargingProfileKind": "Absolute",
  "chargingSchedule": {"chargingRateUnit": "W", "startSchedule": "2026-01-01T00:00:00Z",
    "chargingSchedulePeriod": [{"startPeriod": 0, "limit": 0}]}}}`,
	"ClearChargingProfile": `{"id": 1}`,
	"GetCompositeSchedule": `{"connectorId": 1, "duration": 86400, "chargingRateUnit": "W"}`,
	"TriggerMessage":       `{"requestedMessage": "StatusNotification", "connectorId": 1}`,
	"GetDiagnostics":       `{"location": "https://example.com/upload"}`,
	"UpdateFirmware":       `{"location": "https://example.com/firmware.bin", "retrieveDate": "2026-01-01T00:00:00Z"}`,
	"GetLocalListVersion":  `{}`,
	"SendLocalList":        `{"listVersion": 1, "updateType": "Full", "localAuthorizationList": []}`,
	"ReserveNow":           `{"connectorId": 1, "expiryDate": "2026-01-01T00:00:00Z", "idTag": "grosz", "reservationId": 1}`,
	"CancelReservation":    `{"reservationId": 1}`,
}

// consoleEnums maps go-playground validator tags used by request types to
// their allowed OCPP 1.6 values, for suggestions.
var consoleEnums = map[string][]string{
	"availabilityType":         {"Inoperative", "Operative"},
	"resetType16":              {"Hard", "Soft"},
	"messageTrigger16":         {"BootNotification", "DiagnosticsStatusNotification", "FirmwareStatusNotification", "Heartbeat", "MeterValues", "StatusNotification"},
	"chargingProfilePurpose16": {"ChargePointMaxProfile", "TxDefaultProfile", "TxProfile"},
	"chargingProfileKind16":    {"Absolute", "Recurring", "Relative"},
	"recurrencyKind16":         {"Daily", "Weekly"},
	"chargingRateUnit16":       {"W", "A"},
	"updateType16":             {"Differential", "Full"},
	"authorizationStatus16":    {"Accepted", "Blocked", "Expired", "Invalid", "ConcurrentTx"},
}

// ConsoleActions returns the sendable actions with templates and field hints.
func ConsoleActions() []ConsoleAction {
	var out []ConsoleAction
	for _, p := range consoleProfiles {
		names := make([]string, 0, len(p.Features))
		for name := range p.Features {
			if _, ok := consoleTemplates[name]; ok {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			f := p.GetFeature(name)
			out = append(out, ConsoleAction{
				Action:   name,
				Profile:  p.Name,
				Template: json.RawMessage(consoleTemplates[name]),
				Fields:   describeFields(f.GetRequestType(), "", 0),
			})
		}
	}
	return out
}

// describeFields walks a request struct and lists its JSON fields.
func describeFields(t reflect.Type, prefix string, depth int) []ConsoleField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || depth > 4 {
		return nil
	}
	var out []ConsoleField
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		name := strings.Split(sf.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		validate := sf.Tag.Get("validate")
		f := ConsoleField{Path: path, Type: jsonType(sf.Type), Required: strings.Contains(validate, "required")}
		for _, rule := range strings.Split(validate, ",") {
			if vals, ok := consoleEnums[rule]; ok {
				f.Enum = vals
			}
		}
		out = append(out, f)
		ft := sf.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.Name() != "DateTime" {
			sub := path
			if jsonType(sf.Type) == "array" {
				sub += "[]"
			}
			out = append(out, describeFields(ft, sub, depth+1)...)
		}
	}
	return out
}

func jsonType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.String:
		return "string"
	case reflect.Struct:
		if t.Name() == "DateTime" {
			return "datetime"
		}
		return "object"
	}
	return t.Kind().String()
}

// ParseConsoleRequest strictly decodes payload into the request type of
// action (unknown fields are rejected).
func ParseConsoleRequest(action string, payload json.RawMessage) (ocppcore.Request, error) {
	if _, ok := consoleTemplates[action]; !ok {
		return nil, fmt.Errorf("%q is not a Central System to Charge Point action in OCPP 1.6J", action)
	}
	for _, p := range consoleProfiles {
		f := p.GetFeature(action)
		if f == nil {
			continue
		}
		v := reflect.New(f.GetRequestType())
		if len(bytes.TrimSpace(payload)) == 0 {
			payload = json.RawMessage(`{}`)
		}
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v.Interface()); err != nil {
			return nil, fmt.Errorf("invalid %s payload: %w", action, err)
		}
		req, ok := v.Interface().(ocppcore.Request)
		if !ok {
			return nil, fmt.Errorf("internal: %s request type does not implement ocpp.Request", action)
		}
		return req, nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

// SendConsoleRequest sends a raw (validated) request from the debug console
// and waits for the charge point's response. Both directions are recorded
// in the OCPP log like any other message.
func (s *Server) SendConsoleRequest(cpID, action string, payload json.RawMessage) ConsoleResult {
	start := time.Now()
	res := ConsoleResult{Action: action}
	req, err := ParseConsoleRequest(action, payload)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	type reply struct {
		resp ocppcore.Response
		err  error
	}
	rc := make(chan reply, 1)
	if err := s.cs.SendRequestAsync(cpID, req, func(resp ocppcore.Response, err error) {
		rc <- reply{resp, err}
	}); err != nil {
		res.Error = err.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}
	s.recordEvent("send", cpID, action, req)
	select {
	case r := <-rc:
		if r.err != nil {
			res.Error = r.err.Error()
			s.recordEvent("recv", cpID, action, map[string]string{"error": r.err.Error()})
		} else {
			res.Response = r.resp
			s.recordEvent("recv", cpID, action, r.resp)
		}
	case <-time.After(consoleTimeout):
		res.Error = fmt.Sprintf("no response within %s", consoleTimeout)
	}
	res.DurationMs = time.Since(start).Milliseconds()
	return res
}
