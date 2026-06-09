// Package abrp pushes vehicle telemetry to A Better Route Planner (ABRP)
// via its open Iternio telemetry API. It is driven by the Renault poller:
// each successful poll assembles a Telemetry value and calls Send, which is a
// no-op unless the user has configured a personal ABRP token in settings.
package abrp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/consi/grosz/internal/events"
	"github.com/consi/grosz/internal/store"
)

// genericAPIKey is ABRP's public, documented DIY/"generic" application key.
// It identifies the integration type to Iternio and is NOT a user secret —
// every generic third-party sender uses the same value (same posture as the
// reverse-engineered Renault app keys). The per-user token is what's private,
// and that lives in settings (abrp.token), never in code.
const genericAPIKey = "6f6a554f-d8c8-4c72-8914-d5895f58b1eb"

const defaultBaseURL = "https://api.iternio.com"

// tokenSettingKey is where the user's personal ABRP generic token is stored.
const tokenSettingKey = "abrp.token"

// Telemetry is the subset of ABRP's TLM payload we can source from Renault.
// Optional fields are pointers with omitempty so an absent reading is left out
// of the payload rather than sent as a misleading zero.
type Telemetry struct {
	UTC        int64    `json:"utc"`
	SoC        float64  `json:"soc"`
	IsCharging int      `json:"is_charging"`
	IsParked   *int     `json:"is_parked,omitempty"`
	Power      *float64 `json:"power,omitempty"`             // kW, negative = charging
	EstRange   *float64 `json:"est_battery_range,omitempty"` // km
	Capacity   *float64 `json:"capacity,omitempty"`          // kWh, total pack
	BattTemp   *float64 `json:"batt_temp,omitempty"`         // °C
	ExtTemp    *float64 `json:"ext_temp,omitempty"`          // °C
	Lat        *float64 `json:"lat,omitempty"`
	Lon        *float64 `json:"lon,omitempty"`
	Odometer   *float64 `json:"odometer,omitempty"` // km
}

// Client posts Telemetry to the ABRP/Iternio telemetry endpoint.
type Client struct {
	store   *store.Store
	events  *events.Bound
	log     *slog.Logger
	http    *http.Client
	baseURL string
}

// New constructs an ABRP client bound to the store (for the token) and the
// system event log.
func New(st *store.Store, log *slog.Logger) *Client {
	return NewWithURL(st, log, defaultBaseURL)
}

// NewWithURL is New with a custom Iternio base URL, used by tests to point the
// client at an httptest server.
func NewWithURL(st *store.Store, log *slog.Logger, baseURL string) *Client {
	return &Client{
		store:   st,
		events:  events.For(events.SourceABRP, st),
		log:     log.With("component", "abrp"),
		http:    &http.Client{Timeout: 10 * time.Second},
		baseURL: baseURL,
	}
}

// Enabled reports whether a personal ABRP token is configured. Callers use this
// to skip the extra Renault endpoint fetches (hvac, location) when ABRP is off.
func (c *Client) Enabled() bool {
	return c.store.GetDefault(tokenSettingKey, "") != ""
}

// Send pushes one telemetry frame to ABRP. It is a no-op (returns nil, emits no
// event) when no token is configured. On success it records an info event with
// the pushed values; on failure an error event.
func (c *Client) Send(ctx context.Context, tlm Telemetry) error {
	token := c.store.GetDefault(tokenSettingKey, "")
	if token == "" {
		return nil
	}

	input := eventInput(tlm)

	payload, err := json.Marshal(tlm)
	if err != nil {
		c.events.Error(events.ActionABRPSend, input, err)
		return fmt.Errorf("marshal telemetry: %w", err)
	}

	q := url.Values{"api_key": {genericAPIKey}, "token": {token}}
	endpoint := c.baseURL + "/1/tlm/send?" + q.Encode()
	form := url.Values{"tlm": {string(payload)}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		c.events.Error(events.ActionABRPSend, input, err)
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		c.events.Error(events.ActionABRPSend, input, err)
		return fmt.Errorf("abrp send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("abrp send status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		c.events.Error(events.ActionABRPSend, input, err)
		return err
	}

	var parsed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Status != "ok" {
		err := fmt.Errorf("abrp send rejected: %s", strings.TrimSpace(parsed.Error+" "+string(body)))
		c.events.Error(events.ActionABRPSend, input, err)
		return err
	}

	c.events.Info(events.ActionABRPSend, input, map[string]any{"status": parsed.Status})
	return nil
}

// eventInput renders a Telemetry into a compact, readable map for the system
// event log — the "details about the update" surfaced in the System Log tab.
func eventInput(tlm Telemetry) map[string]any {
	in := map[string]any{
		"soc":        tlm.SoC,
		"isCharging": tlm.IsCharging,
		"hasGps":     tlm.Lat != nil && tlm.Lon != nil,
	}
	if tlm.Power != nil {
		in["powerKw"] = *tlm.Power
	}
	if tlm.EstRange != nil {
		in["rangeKm"] = *tlm.EstRange
	}
	if tlm.Capacity != nil {
		in["capacityKwh"] = *tlm.Capacity
	}
	if tlm.BattTemp != nil {
		in["battTempC"] = *tlm.BattTemp
	}
	if tlm.ExtTemp != nil {
		in["extTempC"] = *tlm.ExtTemp
	}
	if tlm.Odometer != nil {
		in["odometerKm"] = *tlm.Odometer
	}
	return in
}
