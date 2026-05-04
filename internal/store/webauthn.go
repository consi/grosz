package store

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthnCredential is a stored passkey credential.
type WebAuthnCredential struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`

	cred webauthn.Credential
}

// ToLibrary returns the go-webauthn Credential for use with the library.
func (c *WebAuthnCredential) ToLibrary() webauthn.Credential {
	return c.cred
}

// SaveCredential persists a new WebAuthn credential.
func (s *Store) SaveCredential(name string, cred *webauthn.Credential) error {
	transport, _ := json.Marshal(cred.Transport)
	flags, _ := json.Marshal(cred.Flags)
	auth, _ := json.Marshal(cred.Authenticator)

	_, err := s.db.Exec(`
		INSERT INTO webauthn_credentials (id, public_key, attestation_type, transport, flags, authenticator, counter, name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		protocol.URLEncodedBase64(cred.ID).String(),
		cred.PublicKey,
		cred.AttestationType,
		string(transport),
		string(flags),
		string(auth),
		cred.Authenticator.SignCount,
		name,
	)
	if err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	return nil
}

// ListCredentials returns all stored WebAuthn credentials.
func (s *Store) ListCredentials() ([]WebAuthnCredential, error) {
	rows, err := s.db.Query(`
		SELECT id, public_key, attestation_type, transport, flags, authenticator, counter, name, created_at
		FROM webauthn_credentials ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	var result []WebAuthnCredential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *c)
	}
	return result, rows.Err()
}

// DeleteCredential removes a credential by its base64url ID.
func (s *Store) DeleteCredential(id string) error {
	res, err := s.db.Exec(`DELETE FROM webauthn_credentials WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateCredentialCounter updates the sign counter after a successful login.
func (s *Store) UpdateCredentialCounter(id string, counter uint32) error {
	_, err := s.db.Exec(`UPDATE webauthn_credentials SET counter = ? WHERE id = ?`, counter, id)
	return err
}

// HasCredentials returns true if at least one passkey is registered.
func (s *Store) HasCredentials() (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM webauthn_credentials`).Scan(&count)
	return count > 0, err
}

type scannable interface {
	Scan(dest ...any) error
}

func scanCredential(row scannable) (*WebAuthnCredential, error) {
	var (
		id              string
		publicKey       []byte
		attestationType string
		transportJSON   sql.NullString
		flagsJSON       string
		authJSON        string
		counter         uint32
		name            string
		createdAtStr    string
	)

	if err := row.Scan(&id, &publicKey, &attestationType, &transportJSON, &flagsJSON, &authJSON, &counter, &name, &createdAtStr); err != nil {
		return nil, fmt.Errorf("scan credential: %w", err)
	}

	var transport []protocol.AuthenticatorTransport
	if transportJSON.Valid {
		_ = json.Unmarshal([]byte(transportJSON.String), &transport)
	}

	var flags webauthn.CredentialFlags
	_ = json.Unmarshal([]byte(flagsJSON), &flags)

	var auth webauthn.Authenticator
	_ = json.Unmarshal([]byte(authJSON), &auth)
	auth.SignCount = counter // DB column is authoritative

	credID, _ := base64.RawURLEncoding.DecodeString(id)

	createdAt, _ := time.Parse("2006-01-02 15:04:05", createdAtStr)

	return &WebAuthnCredential{
		ID:        id,
		Name:      name,
		CreatedAt: createdAt,
		cred: webauthn.Credential{
			ID:              credID,
			PublicKey:       publicKey,
			AttestationType: attestationType,
			Transport:       transport,
			Flags:           flags,
			Authenticator:   auth,
		},
	}, nil
}
