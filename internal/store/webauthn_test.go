package store

import (
	"database/sql"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCredential(id string) *webauthn.Credential {
	return &webauthn.Credential{
		ID:              []byte(id),
		PublicKey:       []byte("test-public-key-" + id),
		AttestationType: "none",
		Transport:       []protocol.AuthenticatorTransport{"internal"},
		Flags: webauthn.CredentialFlags{
			UserPresent:    true,
			UserVerified:   true,
			BackupEligible: false,
			BackupState:    false,
		},
		Authenticator: webauthn.Authenticator{
			SignCount: 0,
		},
	}
}

func TestSaveAndListCredentials(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.SaveCredential("My Phone", testCredential("cred-1")))
	require.NoError(t, s.SaveCredential("My Laptop", testCredential("cred-2")))

	creds, err := s.ListCredentials()
	require.NoError(t, err)
	require.Len(t, creds, 2)

	assert.Equal(t, "My Phone", creds[0].Name)
	assert.Equal(t, "My Laptop", creds[1].Name)

	// Verify library credentials are populated
	lib := creds[0].ToLibrary()
	assert.Equal(t, []byte("cred-1"), lib.ID)
	assert.Equal(t, []byte("test-public-key-cred-1"), lib.PublicKey)
	assert.Equal(t, "none", lib.AttestationType)
	assert.True(t, lib.Flags.UserPresent)
	assert.True(t, lib.Flags.UserVerified)
}

func TestDeleteCredential(t *testing.T) {
	s := testStore(t)

	cred := testCredential("cred-del")
	require.NoError(t, s.SaveCredential("To Delete", cred))

	creds, _ := s.ListCredentials()
	require.Len(t, creds, 1)

	require.NoError(t, s.DeleteCredential(creds[0].ID))

	creds, _ = s.ListCredentials()
	assert.Len(t, creds, 0)
}

func TestDeleteNonexistent(t *testing.T) {
	s := testStore(t)
	err := s.DeleteCredential("nonexistent-id")
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestUpdateCredentialCounter(t *testing.T) {
	s := testStore(t)

	cred := testCredential("cred-counter")
	require.NoError(t, s.SaveCredential("Counter Test", cred))

	creds, _ := s.ListCredentials()
	require.Len(t, creds, 1)

	require.NoError(t, s.UpdateCredentialCounter(creds[0].ID, 42))

	creds, _ = s.ListCredentials()
	lib := creds[0].ToLibrary()
	assert.Equal(t, uint32(42), lib.Authenticator.SignCount)
}

func TestHasCredentials(t *testing.T) {
	s := testStore(t)

	has, err := s.HasCredentials()
	require.NoError(t, err)
	assert.False(t, has)

	require.NoError(t, s.SaveCredential("Test", testCredential("cred-has")))

	has, err = s.HasCredentials()
	require.NoError(t, err)
	assert.True(t, has)

	creds, _ := s.ListCredentials()
	require.NoError(t, s.DeleteCredential(creds[0].ID))

	has, err = s.HasCredentials()
	require.NoError(t, err)
	assert.False(t, has)
}
