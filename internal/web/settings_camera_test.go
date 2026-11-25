package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/camera"
)

func putSettings(t *testing.T, web *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	web.handlePutSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader([]byte(body))))
	return w
}

func cameraEnabledInStatus(t *testing.T, web *Server) bool {
	t.Helper()
	data, err := json.Marshal(web.buildStatus())
	require.NoError(t, err)
	var status struct {
		CameraEnabled bool `json:"cameraEnabled"`
	}
	require.NoError(t, json.Unmarshal(data, &status))
	return status.CameraEnabled
}

func TestAPI_CameraSettings(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	// No camera configured: the dashboard keeps its plain layout.
	assert.False(t, cameraEnabledInStatus(t, web))

	// A URL that is not RTSP is refused with a translatable error.
	w := putSettings(t, web, `{"camera.rtsp_url":"http://cam.local/stream"}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), `"errorKey":"settings.cameraUrlInvalid"`)
	assert.False(t, cameraEnabledInStatus(t, web))

	// Credentials typed into the URL are stored apart from it.
	w = putSettings(t, web, `{"camera.rtsp_url":"rtsp://admin:s3cr3t@cam.local:554/live"}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "rtsp://cam.local:554/live", web.store.GetDefault(camera.KeyURL, ""))
	assert.Equal(t, "admin", web.store.GetDefault(camera.KeyUsername, ""))
	assert.Equal(t, "s3cr3t", web.store.GetDefault(camera.KeyPassword, ""))
	assert.True(t, cameraEnabledInStatus(t, web))

	// The password is never sent back to the browser...
	w = httptest.NewRecorder()
	web.handleGetSettings(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "s3cr3t")
	var got map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "", got[camera.KeyPassword])
	assert.Equal(t, "rtsp://cam.local:554/live", got[camera.KeyURL])

	// ...so saving the form with the password field untouched keeps it.
	w = putSettings(t, web, `{"camera.rtsp_username":"viewer","camera.rtsp_password":""}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "viewer", web.store.GetDefault(camera.KeyUsername, ""))
	assert.Equal(t, "s3cr3t", web.store.GetDefault(camera.KeyPassword, ""))

	// Nor does it reach the system log.
	evts, err := web.store.RecentSystemEvents(50, 0)
	require.NoError(t, err)
	require.NotEmpty(t, evts)
	logged, err := json.Marshal(evts)
	require.NoError(t, err)
	assert.NotContains(t, string(logged), "s3cr3t")

	// Clearing the URL turns the camera off again.
	w = putSettings(t, web, `{"camera.rtsp_url":""}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, cameraEnabledInStatus(t, web))
}

func TestAPI_CameraStreamRequiresLogin(t *testing.T) {
	web, _, _ := newAPITestServer(t)
	require.Equal(t, http.StatusOK, putSettings(t, web, `{"camera.rtsp_url":"rtsp://cam.local/live"}`).Code)

	w := httptest.NewRecorder()
	web.srv.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/camera/hls/index.m3u8", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
