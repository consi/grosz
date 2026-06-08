package web

import (
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const bcryptCost = 12

// hashPassword returns a bcrypt hash of the given plaintext password.
func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// checkPassword compares a plaintext password against a bcrypt hash.
func checkPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// isBcryptHash returns true if the string looks like a bcrypt hash.
func isBcryptHash(s string) bool {
	return strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$")
}

// sensitiveKeys are settings that should be redacted in API responses and logs.
var sensitiveKeys = map[string]bool{
	"auth.password":           true,
	"vehicle.renault_session": true,
	"vehicle.renault_gmid":    true,
	"vehicle.renault_ucid":    true,
}

// migratePasswordHash ensures auth.password is stored as a bcrypt hash.
// If the password is plaintext (legacy), it hashes it in place.
// If no password is set, it seeds a hashed default ("admin").
func (s *Server) migratePasswordHash() {
	pass, err := s.store.Get("auth.password")
	if err != nil {
		// Not set; seed hashed default
		hashed, err := hashPassword("admin")
		if err != nil {
			s.log.Error("failed to hash default password", "err", err)
			return
		}
		if err := s.store.Set("auth.password", hashed); err != nil {
			s.log.Error("failed to seed default password hash", "err", err)
		} else {
			s.log.Info("seeded default auth.password as bcrypt hash")
		}
		return
	}

	if !isBcryptHash(pass) {
		hashed, err := hashPassword(pass)
		if err != nil {
			s.log.Error("failed to hash existing password", "err", err)
			return
		}
		if err := s.store.Set("auth.password", hashed); err != nil {
			s.log.Error("failed to migrate password hash", "err", err)
		} else {
			s.log.Info("migrated auth.password from plaintext to bcrypt hash")
		}
	}
}
