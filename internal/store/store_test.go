package store

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := New(dbPath, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNewAndMigrate(t *testing.T) {
	s := testStore(t)

	// Verify tables exist by querying them
	var count int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM settings").Scan(&count))
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM ocpp_events").Scan(&count))
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM charging_sessions").Scan(&count))
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM tariff_rates").Scan(&count))
}

func TestMigrateIdempotent(t *testing.T) {
	s := testStore(t)

	// Running migrate again should not fail
	require.NoError(t, s.migrate())
}
