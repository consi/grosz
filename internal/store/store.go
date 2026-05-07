package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	_ "modernc.org/sqlite"
)

// Store wraps a SQLite database for all grosz persistence.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// New opens (or creates) a SQLite database at dbPath and runs migrations.
func New(dbPath string, log *slog.Logger) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{db: db, log: log}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying *sql.DB for advanced usage.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,

		`CREATE TABLE IF NOT EXISTS ocpp_events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp  TEXT NOT NULL,
			direction  TEXT NOT NULL,
			charge_box TEXT NOT NULL,
			action     TEXT NOT NULL,
			payload    TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_timestamp ON ocpp_events(timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_events_action ON ocpp_events(action)`,

		`CREATE TABLE IF NOT EXISTS charging_sessions (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			charge_box     TEXT NOT NULL,
			connector_id   INTEGER NOT NULL,
			transaction_id INTEGER NOT NULL UNIQUE,
			id_tag         TEXT,
			start_time     TEXT NOT NULL,
			stop_time      TEXT,
			meter_start    REAL,
			meter_stop     REAL,
			energy         REAL,
			cost           REAL,
			status         TEXT NOT NULL DEFAULT 'active'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_status ON charging_sessions(status)`,
		`ALTER TABLE charging_sessions ADD COLUMN kwh_per_100km REAL`,
		`ALTER TABLE charging_sessions ADD COLUMN distance_km REAL`,

		`CREATE TABLE IF NOT EXISTS tariff_rates (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			provider   TEXT NOT NULL,
			start_time TEXT NOT NULL,
			end_time   TEXT NOT NULL,
			price      REAL NOT NULL,
			fetched_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(provider, start_time)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rates_time ON tariff_rates(start_time)`,

		`CREATE TABLE IF NOT EXISTS meter_readings (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			power_w   REAL NOT NULL,
			energy_wh REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_meter_timestamp ON meter_readings(timestamp)`,

		`CREATE TABLE IF NOT EXISTS phase_readings (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			phase1_w  REAL NOT NULL,
			phase2_w  REAL NOT NULL,
			phase3_w  REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_phase_timestamp ON phase_readings(timestamp)`,

		`CREATE TABLE IF NOT EXISTS chart_markers (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			type      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_chart_markers_timestamp ON chart_markers(timestamp)`,

		`CREATE TABLE IF NOT EXISTS system_events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp  TEXT NOT NULL,
			source     TEXT NOT NULL,
			action     TEXT NOT NULL,
			level      TEXT NOT NULL DEFAULT 'info',
			input      TEXT NOT NULL DEFAULT '{}',
			result     TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sysevents_timestamp ON system_events(timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_sysevents_source ON system_events(source)`,

		`CREATE TABLE IF NOT EXISTS external_costs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			date        TEXT NOT NULL,
			description TEXT NOT NULL,
			amount      REAL NOT NULL,
			created_at  TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_external_costs_date ON external_costs(date)`,

		`CREATE TABLE IF NOT EXISTS odometer_readings (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			mileage   REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_odometer_timestamp ON odometer_readings(timestamp)`,

		`CREATE TABLE IF NOT EXISTS daily_idle (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			date       TEXT NOT NULL UNIQUE,
			energy_kwh REAL NOT NULL,
			cost       REAL NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_daily_idle_date ON daily_idle(date)`,

		`CREATE TABLE IF NOT EXISTS webauthn_credentials (
			id               TEXT PRIMARY KEY,
			public_key       BLOB NOT NULL,
			attestation_type TEXT NOT NULL,
			transport        TEXT,
			flags            TEXT NOT NULL,
			authenticator    TEXT NOT NULL,
			counter          INTEGER NOT NULL DEFAULT 0,
			name             TEXT NOT NULL,
			created_at       TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE TABLE IF NOT EXISTS connector_state (
			charge_box               TEXT NOT NULL,
			connector_id             INTEGER NOT NULL,
			status                   TEXT NOT NULL DEFAULT '',
			status_at                TEXT NOT NULL DEFAULT '',
			error_code               TEXT NOT NULL DEFAULT '',
			transaction_id           INTEGER NOT NULL DEFAULT 0,
			id_tag                   TEXT NOT NULL DEFAULT '',
			last_status_notification TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (charge_box, connector_id)
		)`,

		`CREATE TABLE IF NOT EXISTS schedule_overrides (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			kind       TEXT NOT NULL CHECK(kind IN ('force','block')),
			start_time TEXT NOT NULL,
			end_time   TEXT NOT NULL,
			power_w    REAL NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			CHECK(end_time > start_time)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_overrides_window ON schedule_overrides(start_time, end_time)`,
	}

	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(m)), "ALTER TABLE") &&
				strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("exec migration: %w\nSQL: %s", err, m)
		}
	}

	return nil
}
