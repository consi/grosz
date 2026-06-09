package web

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/consi/grosz/internal/events"
)

// webauthnUser implements webauthn.User for the single admin user.
type webauthnUser struct {
	name  string
	creds []webauthn.Credential
}

var adminUserID = []byte("grosz-admin-user")

func (u *webauthnUser) WebAuthnID() []byte            { return adminUserID }
func (u *webauthnUser) WebAuthnName() string           { return u.name }
func (u *webauthnUser) WebAuthnDisplayName() string    { return u.name }
func (u *webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// challengeStore holds pending WebAuthn challenges in memory.
type challengeStore struct {
	mu       sync.Mutex
	sessions map[string]*challengeEntry
}

type challengeEntry struct {
	data    *webauthn.SessionData
	expires time.Time
}

func newChallengeStore() *challengeStore {
	return &challengeStore{sessions: make(map[string]*challengeEntry)}
}

func (cs *challengeStore) store(data *webauthn.SessionData) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	key := hex.EncodeToString(b)

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Lazy cleanup of expired entries
	now := time.Now()
	for k, v := range cs.sessions {
		if now.After(v.expires) {
			delete(cs.sessions, k)
		}
	}

	cs.sessions[key] = &challengeEntry{
		data:    data,
		expires: now.Add(60 * time.Second),
	}
	return key
}

func (cs *challengeStore) get(key string) *webauthn.SessionData {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	entry, ok := cs.sessions[key]
	if !ok {
		return nil
	}
	delete(cs.sessions, key) // one-time use

	if time.Now().After(entry.expires) {
		return nil
	}
	return entry.data
}

func (s *Server) loadWebAuthnUser() *webauthnUser {
	stored, _ := s.store.ListCredentials()
	creds := make([]webauthn.Credential, len(stored))
	for i, c := range stored {
		creds[i] = c.ToLibrary()
	}
	return &webauthnUser{
		name:  s.store.GetDefault("auth.username", "admin"),
		creds: creds,
	}
}

func (s *Server) newWebAuthn(r *http.Request) (*webauthn.WebAuthn, error) {
	// Derive RP ID and origin from the browser's Origin header (most reliable
	// behind reverse proxies), falling back to Host / X-Forwarded-Host.
	origin := r.Header.Get("Origin")
	var host string
	if origin != "" {
		host = strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	} else {
		host = r.Host
		if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
			host = fwdHost
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		}
		origin = scheme + "://" + host
	}

	hostname := host
	if idx := strings.LastIndex(host, ":"); idx > 0 {
		if !strings.Contains(host[idx:], "]") {
			hostname = host[:idx]
		}
	}

	return webauthn.New(&webauthn.Config{
		RPDisplayName: "grosz",
		RPID:          hostname,
		RPOrigins:     []string{origin},
	})
}

// --- Registration (requires auth) ---

func (s *Server) handleWebAuthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	wan, err := s.newWebAuthn(r)
	if err != nil {
		s.internalError(w, "webauthn init failed", err)
		return
	}

	user := s.loadWebAuthnUser()

	// Exclude already-registered credentials
	var excludeList []protocol.CredentialDescriptor
	for _, c := range user.creds {
		excludeList = append(excludeList, c.Descriptor())
	}

	creation, session, err := wan.BeginRegistration(user,
		webauthn.WithExclusions(excludeList),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
	)
	if err != nil {
		s.internalError(w, "webauthn begin registration failed", err)
		return
	}

	key := s.challenges.store(session)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"options":      creation,
		"challengeKey": key,
	})
}

func (s *Server) handleWebAuthnRegisterComplete(w http.ResponseWriter, r *http.Request) {
	// Read the full body so we can extract challengeKey and re-feed the rest to the library.
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var envelope struct {
		ChallengeKey string `json:"challengeKey"`
		Name         string `json:"name"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	session := s.challenges.get(envelope.ChallengeKey)
	if session == nil {
		http.Error(w, "challenge expired or invalid", http.StatusBadRequest)
		return
	}

	wan, err := s.newWebAuthn(r)
	if err != nil {
		s.internalError(w, "webauthn init failed", err)
		return
	}

	user := s.loadWebAuthnUser()

	// The library reads the credential from r.Body, so re-attach the body.
	r.Body = io.NopCloser(bytes.NewReader(body))

	cred, err := wan.FinishRegistration(user, *session, r)
	if err != nil {
		s.log.Warn("webauthn registration failed", "err", err)
		http.Error(w, "registration failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	name := envelope.Name
	if name == "" {
		name = "Passkey"
	}

	if err := s.store.SaveCredential(name, cred); err != nil {
		http.Error(w, "failed to save credential", http.StatusInternalServerError)
		return
	}

	s.log.Info("webauthn credential registered", "name", name)
	s.auth.Info(events.ActionCredentialRegistered,
		map[string]any{"name": name, "userAgent": r.UserAgent()},
		nil,
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// --- Authentication (public) ---

func (s *Server) handleWebAuthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	has, _ := s.store.HasCredentials()
	if !has {
		http.Error(w, "no passkeys configured", http.StatusNotFound)
		return
	}

	wan, err := s.newWebAuthn(r)
	if err != nil {
		s.internalError(w, "webauthn init failed", err)
		return
	}

	user := s.loadWebAuthnUser()

	assertion, session, err := wan.BeginLogin(user)
	if err != nil {
		s.internalError(w, "webauthn begin login failed", err)
		return
	}

	key := s.challenges.store(session)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"options":      assertion,
		"challengeKey": key,
	})
}

func (s *Server) handleWebAuthnLoginComplete(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var envelope struct {
		ChallengeKey string `json:"challengeKey"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	session := s.challenges.get(envelope.ChallengeKey)
	if session == nil {
		http.Error(w, "challenge expired or invalid", http.StatusBadRequest)
		return
	}

	wan, err := s.newWebAuthn(r)
	if err != nil {
		s.internalError(w, "webauthn init failed", err)
		return
	}

	user := s.loadWebAuthnUser()
	r.Body = io.NopCloser(bytes.NewReader(body))

	cred, err := wan.FinishLogin(user, *session, r)
	if err != nil {
		s.log.Warn("webauthn login failed", "err", err)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}

	// Update counter
	credID := protocol.URLEncodedBase64(cred.ID).String()
	_ = s.store.UpdateCredentialCounter(credID, cred.Authenticator.SignCount)

	// Create session (same mechanism as password login)
	username := s.store.GetDefault("auth.username", "admin")
	token, expiresAt, err := s.createSession(username, r.UserAgent())
	if err != nil {
		s.log.Error("failed to create webauthn session", "err", err)
		s.auth.Error(events.ActionLogin,
			map[string]any{"username": username, "method": "webauthn"},
			err,
		)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, token)

	s.log.Info("webauthn login successful")
	s.auth.Info(events.ActionLogin,
		map[string]any{"username": username, "method": "webauthn", "userAgent": r.UserAgent()},
		map[string]any{"expiresAt": expiresAt.Format(time.RFC3339)},
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// --- Credential management (requires auth) ---

func (s *Server) handleWebAuthnCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.store.ListCredentials()
	if err != nil {
		s.internalError(w, "failed to list credentials", err)
		return
	}

	type credInfo struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		CreatedAt string `json:"createdAt"`
	}

	result := make([]credInfo, len(creds))
	for i, c := range creds {
		result[i] = credInfo{
			ID:        c.ID,
			Name:      c.Name,
			CreatedAt: c.CreatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"credentials": result})
}

func (s *Server) handleWebAuthnDeleteCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing credential id", http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteCredential(id); err != nil {
		http.Error(w, "credential not found", http.StatusNotFound)
		return
	}

	s.log.Info("webauthn credential deleted", "id", id)
	s.auth.Info(events.ActionCredentialDeleted,
		map[string]any{"id": id},
		nil,
	)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

