package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ConnectorState is the persisted last-known state of a single connector.
// Used to detect real status transitions across app restarts and websocket
// reconnects, so chart markers (plug/unplug) only fire on real changes.
type ConnectorState struct {
	ChargeBox              string    `json:"chargeBox"`
	ConnectorID            int       `json:"connectorId"`
	Status                 string    `json:"status"`
	StatusAt               time.Time `json:"statusAt"`
	ErrorCode              string    `json:"errorCode,omitempty"`
	TransactionID          int       `json:"transactionId,omitempty"`
	IdTag                  string    `json:"idTag,omitempty"`
	LastStatusNotification time.Time `json:"lastStatusNotification"`
}

// GetConnectorState returns the persisted state for a (charge_box, connector_id)
// pair, or nil if no row exists yet.
func (s *Store) GetConnectorState(chargeBox string, connectorID int) (*ConnectorState, error) {
	row := s.db.QueryRow(
		`SELECT charge_box, connector_id, status, status_at, error_code,
		        transaction_id, id_tag, last_status_notification
		 FROM connector_state WHERE charge_box = ? AND connector_id = ?`,
		chargeBox, connectorID,
	)
	cs, err := scanConnectorState(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return cs, err
}

// UpsertConnectorState writes or updates the persisted state for a connector.
// All fields are overwritten with the provided values.
func (s *Store) UpsertConnectorState(cs ConnectorState) error {
	_, err := s.db.Exec(
		`INSERT INTO connector_state
		 (charge_box, connector_id, status, status_at, error_code, transaction_id, id_tag, last_status_notification)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(charge_box, connector_id) DO UPDATE SET
		   status = excluded.status,
		   status_at = excluded.status_at,
		   error_code = excluded.error_code,
		   transaction_id = excluded.transaction_id,
		   id_tag = excluded.id_tag,
		   last_status_notification = excluded.last_status_notification`,
		cs.ChargeBox, cs.ConnectorID, cs.Status,
		formatTimeUTC(cs.StatusAt),
		cs.ErrorCode, cs.TransactionID, cs.IdTag,
		formatTimeUTC(cs.LastStatusNotification),
	)
	if err != nil {
		return fmt.Errorf("upsert connector_state: %w", err)
	}
	return nil
}

// AllConnectorStates returns all persisted connector states.
func (s *Store) AllConnectorStates() ([]ConnectorState, error) {
	rows, err := s.db.Query(
		`SELECT charge_box, connector_id, status, status_at, error_code,
		        transaction_id, id_tag, last_status_notification
		 FROM connector_state`,
	)
	if err != nil {
		return nil, fmt.Errorf("query connector_state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ConnectorState, 0)
	for rows.Next() {
		cs, err := scanConnectorState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *cs)
	}
	return out, rows.Err()
}

// TouchStatusNotification updates only the last_status_notification timestamp
// for a connector. Used when the same status is re-reported (so we don't emit
// a duplicate chart marker but still record that the charger is alive).
// Inserts a row with empty status if none exists yet.
func (s *Store) TouchStatusNotification(chargeBox string, connectorID int, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO connector_state (charge_box, connector_id, last_status_notification)
		 VALUES (?, ?, ?)
		 ON CONFLICT(charge_box, connector_id) DO UPDATE SET
		   last_status_notification = excluded.last_status_notification`,
		chargeBox, connectorID, formatTimeUTC(at),
	)
	if err != nil {
		return fmt.Errorf("touch connector_state: %w", err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanConnectorState(sc scanner) (*ConnectorState, error) {
	var cs ConnectorState
	var statusAtStr, lastStr string
	if err := sc.Scan(
		&cs.ChargeBox, &cs.ConnectorID, &cs.Status, &statusAtStr,
		&cs.ErrorCode, &cs.TransactionID, &cs.IdTag, &lastStr,
	); err != nil {
		return nil, err
	}
	cs.StatusAt = parseTimeUTC(statusAtStr)
	cs.LastStatusNotification = parseTimeUTC(lastStr)
	return &cs, nil
}

func formatTimeUTC(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTimeUTC(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, s)
	}
	return t
}
