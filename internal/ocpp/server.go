package ocpp

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ws"

	"github.com/consi/grosz/internal/store"
)

// BootHook is called after a successful BootNotification.
type BootHook func(chargePointID string, req *core.BootNotificationRequest)

// StatusHook is called whenever chargepoint state changes (status, meters, transactions).
type StatusHook func()

// SoCUpdater is called with energy (kWh) after a charging session completes.
type SoCUpdater func(energyKWh float64)

// ConnectorStatusHook is called when a connector's status changes.
type ConnectorStatusHook func(cpID string, connectorID int, status string)

// Server wraps the OCPP 1.6 Central System.
type Server struct {
	cs        ocpp16.CentralSystem
	mu        sync.RWMutex
	points    map[string]*ChargePoint
	// realAddrs maps cpID → resolved client IP captured from X-Forwarded-For
	// during the WebSocket upgrade. The gorilla connection's RemoteAddr only
	// sees the reverse-proxy peer, so we stash the real IP here and read it
	// in the connect handler.
	realAddrs  map[string]string
	store     *store.Store
	log       *slog.Logger
	bootHooks  []BootHook
	statusHook      StatusHook
	socUpdater      SoCUpdater
	connStatusHook  ConnectorStatusHook
	nextTxnID  atomic.Int64
	lastPurge  time.Time
}

// NewServer creates a new OCPP Central System server.
// If ocpp.auth_key is set in the store, HTTP Basic Auth is enabled
// (myenergi Zappi portal's "Authorisation Key" field).
func NewServer(st *store.Store, log *slog.Logger) *Server {
	wsServer := ws.NewServer()

	// Enable HTTP Basic Auth if an auth key is configured.
	// myenergi sends username=ChargeboxID, password=AuthorisationKey.
	authKey := st.GetDefault("ocpp.auth_key", "")
	if authKey != "" {
		wsServer.SetBasicAuthHandler(func(username, password string) bool {
			return password == authKey
		})
		log.Info("OCPP Basic Auth enabled")
	}

	cs := ocpp16.NewCentralSystem(nil, wsServer)

	s := &Server{
		cs:        cs,
		points:    make(map[string]*ChargePoint),
		realAddrs: make(map[string]string),
		store:     st,
		log:       log.With("component", "ocpp"),
	}

	// Capture the real client IP from X-Forwarded-For during the upgrade.
	// The CentralSystem invokes this with the full *http.Request before it
	// hands us the WebSocket. We always allow the connection — this is purely
	// a header-stash, not an auth check (Basic Auth runs first).
	//
	// NOTE: must go through the CentralSystem (which routes to ocppj.Server),
	// not wsServer directly, because ocppj.Server.Start() overwrites the
	// wsServer's checkClientHandler with its own field at startup.
	cs.SetNewChargingStationValidationHandler(func(id string, r *http.Request) bool {
		if ip := clientIPFromRequest(r); ip != "" {
			s.mu.Lock()
			s.realAddrs[id] = ip
			s.mu.Unlock()
		}
		return true
	})
	// Initialize transaction ID counter from DB to avoid collisions after restart
	var maxTxnID int64
	_ = st.DB().QueryRow("SELECT COALESCE(MAX(transaction_id), 0) FROM charging_sessions").Scan(&maxTxnID)
	s.nextTxnID.Store(maxTxnID + 1)

	// Hydrate in-memory state from persisted connector_state so that the first
	// post-restart StatusNotification can be diffed against the last known
	// state (and not generate a phantom plug/unplug chart marker). The CP
	// stays Connected=false until an actual websocket connect.
	s.hydrateAllFromStore()

	cs.SetCoreHandler(s)
	cs.SetFirmwareManagementHandler(s)

	cs.SetNewChargePointHandler(func(cp ocpp16.ChargePointConnection) {
		id := cp.ID()
		s.mu.Lock()
		addr, ok := s.realAddrs[id]
		if !ok {
			addr = cp.RemoteAddr().String()
		}
		// Preserve in-memory state across websocket reconnects so the
		// transaction ID and live measurements survive. Wiping them would
		// strand active sessions: the suspend finalize, scheduler
		// ConnectorStatus, and report code all read from this snapshot.
		if existing, ok := s.points[id]; ok {
			existing.SetConnected(true)
		} else {
			fresh := NewChargePoint(id)
			s.points[id] = fresh
			s.hydrateChargePointFromStore(fresh)
		}
		s.mu.Unlock()
		s.log.Info("charge point connected", "id", id, "addr", addr)
		s.fireStatusHook()
		if s.store != nil {
			s.store.RecordSystemEvent(store.SystemEvent{
				Timestamp: time.Now(), Source: "ocpp", Action: "chargerConnected",
				Input: map[string]any{"cpID": id, "remoteAddr": addr},
			})
		}
	})

	cs.SetChargePointDisconnectedHandler(func(cp ocpp16.ChargePointConnection) {
		id := cp.ID()
		s.log.Info("charge point disconnected", "id", id)
		s.mu.Lock()
		if pt, ok := s.points[id]; ok {
			pt.SetConnected(false)
		}
		delete(s.realAddrs, id)
		s.mu.Unlock()
		s.fireStatusHook()
		if s.store != nil {
			s.store.RecordSystemEvent(store.SystemEvent{
				Timestamp: time.Now(), Source: "ocpp", Action: "chargerDisconnected",
				Input: map[string]any{"cpID": id},
			})
		}
	})

	return s
}

// Start begins listening for OCPP WebSocket connections.
func (s *Server) Start(port int, path string) {
	s.log.Info("starting OCPP server", "port", port, "path", path)
	s.cs.Start(port, path)
}

// Stop shuts down the OCPP server.
func (s *Server) Stop() {
	s.log.Info("stopping OCPP server")
	s.cs.Stop()
}

// RegisterBootHook adds a function to be called after BootNotification.
func (s *Server) RegisterBootHook(hook BootHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootHooks = append(s.bootHooks, hook)
}

// SetStatusHook registers a callback for chargepoint state changes.
func (s *Server) SetStatusHook(hook StatusHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusHook = hook
}

// SetSoCUpdater registers a callback for SoC updates after sessions.
func (s *Server) SetSoCUpdater(fn SoCUpdater) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.socUpdater = fn
}

// SetConnectorStatusHook registers a callback for connector status changes.
func (s *Server) SetConnectorStatusHook(hook ConnectorStatusHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connStatusHook = hook
}

func (s *Server) fireStatusHook() {
	s.mu.RLock()
	hook := s.statusHook
	s.mu.RUnlock()
	if hook != nil {
		go hook()
	}
}

func (s *Server) fireConnectorStatusHook(cpID string, connectorID int, status string) {
	s.mu.RLock()
	hook := s.connStatusHook
	s.mu.RUnlock()
	if hook != nil {
		go hook(cpID, connectorID, status)
	}
}

// hydrateAllFromStore loads every persisted connector_state into in-memory
// ChargePoints. Called once at startup. Charge points start disconnected
// until a real websocket connect.
func (s *Server) hydrateAllFromStore() {
	if s.store == nil {
		return
	}
	states, err := s.store.AllConnectorStates()
	if err != nil {
		s.log.Warn("hydrate connector_state failed", "err", err)
		return
	}
	for _, cstate := range states {
		cp, ok := s.points[cstate.ChargeBox]
		if !ok {
			cp = NewChargePoint(cstate.ChargeBox)
			cp.SetConnected(false)
			s.points[cstate.ChargeBox] = cp
		}
		applyConnectorState(cp, cstate)
	}
}

// hydrateChargePointFromStore loads persisted connector_state rows for a single
// charge point into the given in-memory CP. Used when a reconnecting charger
// shows up that isn't in s.points yet (e.g. process never saw it before).
func (s *Server) hydrateChargePointFromStore(cp *ChargePoint) {
	if s.store == nil {
		return
	}
	states, err := s.store.AllConnectorStates()
	if err != nil {
		s.log.Warn("hydrate connector_state failed", "err", err, "cpID", cp.ID)
		return
	}
	for _, cstate := range states {
		if cstate.ChargeBox != cp.ID {
			continue
		}
		applyConnectorState(cp, cstate)
	}
}

func applyConnectorState(cp *ChargePoint, cstate store.ConnectorState) {
	conn := cp.GetConnector(cstate.ConnectorID)
	conn.UpdateStatus(cstate.Status, cstate.ErrorCode, cstate.StatusAt)
	if cstate.TransactionID != 0 {
		conn.RestoreTransaction(cstate.TransactionID, cstate.IdTag)
	}
}

// ChargePoint returns the charge point by ID, or nil if not found.
func (s *Server) ChargePoint(id string) *ChargePoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.points[id]
}

// ChargePoints returns snapshots of all connected charge points.
func (s *Server) ChargePoints() []ChargePointSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var snaps []ChargePointSnapshot
	for _, cp := range s.points {
		snaps = append(snaps, cp.Snapshot())
	}
	return snaps
}

// CentralSystem returns the underlying ocpp16 CentralSystem for direct calls.
func (s *Server) CentralSystem() ocpp16.CentralSystem {
	return s.cs
}

// Store returns the store for use by handlers.
func (s *Server) Store() *store.Store {
	return s.store
}

func (s *Server) allocateTxnID() int {
	return int(s.nextTxnID.Add(1) - 1)
}

func (s *Server) recordEvent(direction, chargeBox, action string, payload any) {
	if s.store == nil {
		return
	}
	e := store.Event{
		Timestamp: timeNow(),
		Direction: direction,
		ChargeBox: chargeBox,
		Action:    action,
		Payload:   payload,
	}
	if err := s.store.RecordEvent(e); err != nil {
		s.log.Warn("failed to record event", "action", action, "err", err)
	}

	// Purge old events at most once per hour
	s.mu.Lock()
	if time.Since(s.lastPurge) > time.Hour {
		s.lastPurge = time.Now()
		s.mu.Unlock()
		_ = s.store.PurgeOldEvents(48 * time.Hour)
		_ = s.store.PurgeOldSystemEvents(24 * time.Hour)
		_ = s.store.PurgeChartMarkers(7 * 24 * time.Hour)
	} else {
		s.mu.Unlock()
	}
}

func (s *Server) fireBootHooks(cpID string, req *core.BootNotificationRequest) {
	s.mu.RLock()
	hooks := make([]BootHook, len(s.bootHooks))
	copy(hooks, s.bootHooks)
	s.mu.RUnlock()

	for _, hook := range hooks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("boot hook panicked", "err", fmt.Sprintf("%v", r))
				}
			}()
			hook(cpID, req)
		}()
	}
}
