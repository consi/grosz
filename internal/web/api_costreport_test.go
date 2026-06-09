package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/store"
)

// TestAPI_SessionReportHTML_EscapesDescriptions ensures user-supplied cost
// descriptions cannot inject HTML into the report.
func TestAPI_SessionReportHTML_EscapesDescriptions(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	today := time.Now().UTC().Format("2006-01-02")
	_, err := web.store.AddExternalCost(store.ExternalCost{
		Date:        today,
		Description: `<img src=x onerror=alert(1)>`,
		Amount:      12.34,
	})
	require.NoError(t, err)

	from := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
	to := time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02")
	req := httptest.NewRequest(http.MethodGet, "/api/report/html?from="+from+"&to="+to, nil)
	w := httptest.NewRecorder()
	web.handleSessionReportHTML(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.NotContains(t, body, "<img src=x onerror=alert(1)>")
	assert.Contains(t, body, "&lt;img src=x onerror=alert(1)&gt;")
}

// TestAPI_InternalError_GenericBody ensures internalError never leaks the
// underlying error text to the client.
func TestAPI_InternalError_GenericBody(t *testing.T) {
	web, _, _ := newAPITestServer(t)

	w := httptest.NewRecorder()
	web.internalError(w, "context msg", assert.AnError)

	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, "internal server error", strings.TrimSpace(w.Body.String()))
	assert.NotContains(t, w.Body.String(), assert.AnError.Error())
}
