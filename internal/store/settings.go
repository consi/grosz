package store

import (
	"database/sql"
	"fmt"
	"strconv"
)

// Get returns the value for a settings key, or an error if not found.
func (s *Store) Get(key string) (string, error) {
	var val string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&val)
	if err != nil {
		return "", fmt.Errorf("get setting %q: %w", key, err)
	}
	return val, nil
}

// GetDefault returns the value for a key, or defaultVal if not found.
func (s *Store) GetDefault(key, defaultVal string) string {
	val, err := s.Get(key)
	if err != nil {
		return defaultVal
	}
	return val
}

// GetInt returns the int value for a key, or defaultVal on error.
func (s *Store) GetInt(key string, defaultVal int) int {
	val, err := s.Get(key)
	if err != nil {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return n
}

// GetFloat returns the float64 value for a key, or defaultVal on error.
func (s *Store) GetFloat(key string, defaultVal float64) float64 {
	val, err := s.Get(key)
	if err != nil {
		return defaultVal
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return f
}

// GetBool returns the bool value for a key, or defaultVal on error.
func (s *Store) GetBool(key string, defaultVal bool) bool {
	val, err := s.Get(key)
	if err != nil {
		return defaultVal
	}
	b, err := strconv.ParseBool(val)
	if err != nil {
		return defaultVal
	}
	return b
}

// Set upserts a single setting.
func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

// All returns all settings as a map.
func (s *Store) All() (map[string]string, error) {
	rows, err := s.db.Query("SELECT key, value FROM settings ORDER BY key")
	if err != nil {
		return nil, fmt.Errorf("query settings: %w", err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		result[k] = v
	}
	return result, rows.Err()
}

// SetMany upserts multiple settings in a single transaction.
func (s *Store) SetMany(kv map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for k, v := range kv {
		if _, err := stmt.Exec(k, v); err != nil {
			return fmt.Errorf("set %q: %w", k, err)
		}
	}

	return tx.Commit()
}

// SeedDefaults inserts default settings only if the settings table is empty.
func (s *Store) SeedDefaults(defaults map[string]string) error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM settings").Scan(&count); err != nil {
		return fmt.Errorf("count settings: %w", err)
	}
	if count > 0 {
		return nil
	}

	s.log.Info("seeding default settings", "count", len(defaults))
	return s.SetMany(defaults)
}

// HasSettings returns true if the settings table has any entries.
func (s *Store) HasSettings() bool {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM settings").Scan(&count)
	return err == nil && count > 0
}

// Delete removes a setting by key. Returns sql.ErrNoRows if not found.
func (s *Store) Delete(key string) error {
	result, err := s.db.Exec("DELETE FROM settings WHERE key = ?", key)
	if err != nil {
		return fmt.Errorf("delete setting %q: %w", key, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
