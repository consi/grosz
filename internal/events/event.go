package events

import "time"

// SystemEvent is the data shape persisted to the system_events table.
// It lives here (rather than in store) so the events package can define
// helpers without importing store, and store self-calls can use the
// same vocabulary as every other component.
type SystemEvent struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"` // e.g. "scheduler", "tariff", "ocpp"
	Action    string    `json:"action"`
	Level     string    `json:"level"` // "info", "warn", "error"
	Input     any       `json:"input"`
	Result    any       `json:"result"`
}
