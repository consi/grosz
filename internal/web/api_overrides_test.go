package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
)

func TestAPI_CreateOverride_Roundtrip(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	now := time.Now().UTC().Truncate(time.Second)
	body := map[string]any{
		"kind":   "force",
		"start":  now.Add(time.Hour).Format(time.RFC3339),
		"end":    now.Add(2 * time.Hour).Format(time.RFC3339),
		"powerW": 11000,
	}
	buf, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/schedule/overrides", bytes.NewReader(buf))
	w := httptest.NewRecorder()
	web.handleCreateOverride(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		OK       bool                   `json:"ok"`
		Override store.ScheduleOverride `json:"override"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.True(t, resp.OK)
	assert.Greater(t, resp.Override.ID, int64(0))

	// LIST
	getReq := httptest.NewRequest(http.MethodGet, "/api/schedule/overrides", nil)
	getW := httptest.NewRecorder()
	web.handleListOverrides(getW, getReq)
	require.Equal(t, http.StatusOK, getW.Code)

	var listResp struct {
		Overrides []store.ScheduleOverride `json:"overrides"`
	}
	require.NoError(t, json.NewDecoder(getW.Body).Decode(&listResp))
	require.Len(t, listResp.Overrides, 1)
	assert.Equal(t, store.OverrideKindForce, listResp.Overrides[0].Kind)

	// DELETE
	delReq := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/schedule/overrides/%d", resp.Override.ID), nil)
	delReq.SetPathValue("id", strconv.FormatInt(resp.Override.ID, 10))
	delW := httptest.NewRecorder()
	web.handleDeleteOverride(delW, delReq)
	require.Equal(t, http.StatusOK, delW.Code, delW.Body.String())
}

func TestAPI_CreateOverride_RejectsOverlap(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	now := time.Now().UTC()
	mk := func(start, end time.Duration) []byte {
		body := map[string]any{
			"kind":   "force",
			"start":  now.Add(start).Format(time.RFC3339),
			"end":    now.Add(end).Format(time.RFC3339),
			"powerW": 11000,
		}
		b, _ := json.Marshal(body)
		return b
	}

	w := httptest.NewRecorder()
	web.handleCreateOverride(w, httptest.NewRequest(http.MethodPost, "/api/schedule/overrides", bytes.NewReader(mk(time.Hour, 2*time.Hour))))
	require.Equal(t, http.StatusOK, w.Code)

	w2 := httptest.NewRecorder()
	web.handleCreateOverride(w2, httptest.NewRequest(http.MethodPost, "/api/schedule/overrides", bytes.NewReader(mk(90*time.Minute, 3*time.Hour))))
	assert.Equal(t, http.StatusConflict, w2.Code)
}

func TestAPI_CreateOverride_ValidatesInputs(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	now := time.Now().UTC()
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing kind", map[string]any{"start": now.Add(time.Hour).Format(time.RFC3339), "end": now.Add(2 * time.Hour).Format(time.RFC3339)}},
		{"end before start", map[string]any{"kind": "force", "start": now.Add(2 * time.Hour).Format(time.RFC3339), "end": now.Add(time.Hour).Format(time.RFC3339), "powerW": 11000}},
		{"end in past", map[string]any{"kind": "block", "start": now.Add(-2 * time.Hour).Format(time.RFC3339), "end": now.Add(-time.Hour).Format(time.RFC3339)}},
		{"force without power", map[string]any{"kind": "force", "start": now.Add(time.Hour).Format(time.RFC3339), "end": now.Add(2 * time.Hour).Format(time.RFC3339)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.body)
			w := httptest.NewRecorder()
			web.handleCreateOverride(w, httptest.NewRequest(http.MethodPost, "/api/schedule/overrides", bytes.NewReader(b)))
			assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
		})
	}
}

func TestAPI_DeleteOverride_NotFound(t *testing.T) {
	web, _, _ := newAPITestServer(t)
	delReq := httptest.NewRequest(http.MethodDelete, "/api/schedule/overrides/9999", nil)
	delReq.SetPathValue("id", "9999")
	delW := httptest.NewRecorder()
	web.handleDeleteOverride(delW, delReq)
	assert.Equal(t, http.StatusNotFound, delW.Code)
}
