package web

import (
	"time"

	"github.com/consi/grosz/internal/events"
)

const defaultSessionLifetimeDays = 30

// sessionLifetime returns the configured session lifetime, falling back to 30 days.
func (s *Server) sessionLifetime() time.Duration {
	days := s.store.GetInt("auth.session_lifetime_days", defaultSessionLifetimeDays)
	if days <= 0 {
		days = defaultSessionLifetimeDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// createSession inserts a new session row and returns its token and expiry.
func (s *Server) createSession(username, userAgent string) (string, time.Time, error) {
	token := generateToken()
	lifetime := s.sessionLifetime()
	expiresAt := time.Now().UTC().Add(lifetime)

	_, err := s.store.DB().Exec(
		`INSERT INTO web_sessions (token, username, expires_at, user_agent) VALUES (?, ?, ?, ?)`,
		token, username, expiresAt.Format(time.RFC3339), userAgent,
	)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// lookupSession returns (username, true) if the token is valid and unexpired.
// On hit, opportunistically slides the expiry forward — but only if last_seen is older
// than 5 minutes, to avoid hammering the DB on every authenticated request.
func (s *Server) lookupSession(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	var username, lastSeen string
	err := s.store.DB().QueryRow(
		`SELECT username, last_seen FROM web_sessions
		 WHERE token = ? AND expires_at > datetime('now')`,
		token,
	).Scan(&username, &lastSeen)
	if err != nil {
		return "", false
	}

	// Sliding renewal, debounced to ≥5min between writes.
	t, perr := time.Parse(time.RFC3339, lastSeen)
	if perr != nil {
		// Fallback if last_seen is in datetime('now') format ("2006-01-02 15:04:05").
		t, perr = time.Parse("2006-01-02 15:04:05", lastSeen)
	}
	if perr != nil || time.Since(t) > 5*time.Minute {
		newExpiry := time.Now().UTC().Add(s.sessionLifetime()).Format(time.RFC3339)
		_, _ = s.store.DB().Exec(
			`UPDATE web_sessions SET last_seen = datetime('now'), expires_at = ? WHERE token = ?`,
			newExpiry, token,
		)
	}
	return username, true
}

// deleteSession removes a session by token.
func (s *Server) deleteSession(token string) error {
	if token == "" {
		return nil
	}
	_, err := s.store.DB().Exec(`DELETE FROM web_sessions WHERE token = ?`, token)
	return err
}

// purgeExpiredSessions drops rows whose expires_at is in the past. Called once at startup.
func (s *Server) purgeExpiredSessions() {
	res, err := s.store.DB().Exec(`DELETE FROM web_sessions WHERE expires_at <= datetime('now')`)
	if err != nil {
		s.log.Warn("failed to purge expired sessions", "err", err)
		return
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.log.Info("purged expired sessions", "count", n)
		s.auth.Info(events.ActionPurgeWebSessions, nil,
			map[string]any{"rowsDeleted": n},
		)
	}
}
