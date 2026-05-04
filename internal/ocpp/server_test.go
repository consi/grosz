package ocpp

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
)

func newServerWithTempStore(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	st, err := store.New(dbPath, log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	s := NewServer(st, log)
	return s, st
}

// hydrateChargePointFromStore must populate a fresh CP from connector_state
// (so a charger seen for the first time after a hot restore picks up its
// transaction id immediately).
func TestHydrateChargePointFromStore_LoadsTxn(t *testing.T) {
	s, st := newServerWithTempStore(t)

	at := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	require.NoError(t, st.UpsertConnectorState(store.ConnectorState{
		ChargeBox: "CP777", ConnectorID: 1,
		Status: "Charging", StatusAt: at,
		TransactionID: 50, IdTag: "garaz",
		LastStatusNotification: at,
	}))

	cp := NewChargePoint("CP777")
	s.hydrateChargePointFromStore(cp)

	conn := cp.GetConnector(1)
	snap := conn.Snapshot()
	assert.Equal(t, 50, snap.TransactionID)
	assert.Equal(t, "garaz", snap.IdTag)
	assert.Equal(t, "Charging", snap.Status)
}

// Calling hydrate when no row exists is a no-op (no panic, fresh CP).
func TestHydrateChargePointFromStore_NoRows(t *testing.T) {
	s, _ := newServerWithTempStore(t)

	cp := NewChargePoint("CPnew")
	s.hydrateChargePointFromStore(cp)
	cp.mu.RLock()
	assert.Empty(t, cp.Connectors)
	cp.mu.RUnlock()
}
