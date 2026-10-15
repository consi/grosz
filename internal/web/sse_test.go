package web

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSSEReconnectionReceivesSnapshotAndSubsequentUpdates(t *testing.T) {
	broker := NewSSEBroker(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer broker.Close()
	broker.SetSnapshot(func() string { return `{"overrides":[],"schedule":null}` })
	server := httptest.NewServer(broker)
	defer server.Close()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, "no", response.Header.Get("X-Accel-Buffering"))
		reader := bufio.NewReader(response.Body)
		readEvent := func() string {
			var lines strings.Builder
			for {
				line, err := reader.ReadString('\n')
				require.NoError(t, err)
				if line == "\n" {
					return lines.String()
				}
				lines.WriteString(line)
			}
		}
		require.Contains(t, readEvent(), "event: ping")
		require.Contains(t, readEvent(), `"schedule":null`)
		broker.Broadcast("status", `{"schedule":{"energy":22}}`)
		require.Contains(t, readEvent(), `"energy":22`)
		require.NoError(t, response.Body.Close())
		cancel()
	}
}
