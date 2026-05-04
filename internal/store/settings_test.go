package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetAndGet(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.Set("test.key", "hello"))
	val, err := s.Get("test.key")
	require.NoError(t, err)
	assert.Equal(t, "hello", val)
}

func TestGetNotFound(t *testing.T) {
	s := testStore(t)

	_, err := s.Get("nonexistent")
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestGetDefault(t *testing.T) {
	s := testStore(t)

	assert.Equal(t, "fallback", s.GetDefault("missing", "fallback"))

	require.NoError(t, s.Set("present", "value"))
	assert.Equal(t, "value", s.GetDefault("present", "fallback"))
}

func TestGetInt(t *testing.T) {
	s := testStore(t)

	assert.Equal(t, 42, s.GetInt("missing", 42))

	require.NoError(t, s.Set("port", "8887"))
	assert.Equal(t, 8887, s.GetInt("port", 0))

	require.NoError(t, s.Set("bad", "notanumber"))
	assert.Equal(t, 99, s.GetInt("bad", 99))
}

func TestGetFloat(t *testing.T) {
	s := testStore(t)

	assert.Equal(t, 0.5, s.GetFloat("missing", 0.5))

	require.NoError(t, s.Set("price", "0.1234"))
	assert.InDelta(t, 0.1234, s.GetFloat("price", 0), 0.0001)
}

func TestGetBool(t *testing.T) {
	s := testStore(t)

	assert.True(t, s.GetBool("missing", true))

	require.NoError(t, s.Set("enabled", "true"))
	assert.True(t, s.GetBool("enabled", false))

	require.NoError(t, s.Set("disabled", "false"))
	assert.False(t, s.GetBool("disabled", true))
}

func TestSetUpserts(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.Set("key", "v1"))
	require.NoError(t, s.Set("key", "v2"))

	val, err := s.Get("key")
	require.NoError(t, err)
	assert.Equal(t, "v2", val)
}

func TestAll(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.Set("a", "1"))
	require.NoError(t, s.Set("b", "2"))

	all, err := s.All()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, all)
}

func TestSetMany(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.SetMany(map[string]string{
		"x": "10",
		"y": "20",
		"z": "30",
	}))

	all, err := s.All()
	require.NoError(t, err)
	assert.Len(t, all, 3)
	assert.Equal(t, "10", all["x"])
}

func TestSeedDefaults(t *testing.T) {
	s := testStore(t)

	defaults := map[string]string{"ocpp.port": "8887", "web.port": "3000"}
	require.NoError(t, s.SeedDefaults(defaults))

	val, err := s.Get("ocpp.port")
	require.NoError(t, err)
	assert.Equal(t, "8887", val)

	// Should not overwrite existing
	require.NoError(t, s.Set("ocpp.port", "9999"))
	require.NoError(t, s.SeedDefaults(defaults))

	val, err = s.Get("ocpp.port")
	require.NoError(t, err)
	assert.Equal(t, "9999", val)
}

func TestDelete(t *testing.T) {
	s := testStore(t)

	require.NoError(t, s.Set("key", "val"))
	require.NoError(t, s.Delete("key"))
	_, err := s.Get("key")
	assert.ErrorIs(t, err, sql.ErrNoRows)

	assert.ErrorIs(t, s.Delete("nonexistent"), sql.ErrNoRows)
}

func TestHasSettings(t *testing.T) {
	s := testStore(t)

	assert.False(t, s.HasSettings())
	require.NoError(t, s.Set("key", "val"))
	assert.True(t, s.HasSettings())
}
