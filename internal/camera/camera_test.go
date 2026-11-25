package camera

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/consi/grosz/internal/events"
)

const (
	camUser = "viewer"
	camPass = "s3cr3t-pass"
)

var (
	testSPS = []byte{
		0x67, 0x42, 0xc0, 0x28, 0xd9, 0x00, 0x78, 0x02,
		0x27, 0xe5, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04,
		0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc9,
		0x20,
	}
	testPPS = []byte{0x08, 0x06, 0x07, 0x08}
)

type memSettings map[string]string

func (m memSettings) GetDefault(key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

type memRecorder struct {
	mu     sync.Mutex
	events []events.SystemEvent
}

func (m *memRecorder) RecordSystemEvent(e events.SystemEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

func (m *memRecorder) byAction(a events.Action) []events.SystemEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []events.SystemEvent
	for _, e := range m.events {
		if e.Action == string(a) {
			out = append(out, e)
		}
	}
	return out
}

func (m *memRecorder) dump(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := json.Marshal(m.events)
	require.NoError(t, err)
	return string(data)
}

// fakeCamera is an RTSP server that requires credentials and plays a
// synthetic H264 stream made of keyframes only.
type fakeCamera struct {
	server *gortsplib.Server
	stream *gortsplib.ServerStream
	addr   string
	stop   chan struct{}
	done   chan struct{}

	mu       sync.Mutex
	sessions int
}

func (c *fakeCamera) authorized(conn *gortsplib.ServerConn, req *base.Request) bool {
	return conn.VerifyCredentials(req, camUser, camPass)
}

func (c *fakeCamera) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !c.authorized(ctx.Conn, ctx.Request) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}

func (c *fakeCamera) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !c.authorized(ctx.Conn, ctx.Request) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	return &base.Response{StatusCode: base.StatusOK}, c.stream, nil
}

func (c *fakeCamera) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (c *fakeCamera) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {
	c.mu.Lock()
	c.sessions++
	c.mu.Unlock()
}

func (c *fakeCamera) OnSessionClose(_ *gortsplib.ServerHandlerOnSessionCloseCtx) {
	c.mu.Lock()
	c.sessions--
	c.mu.Unlock()
}

func (c *fakeCamera) openSessions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions
}

func startFakeCamera(t *testing.T) *fakeCamera {
	t.Helper()
	cam := &fakeCamera{stop: make(chan struct{}), done: make(chan struct{})}
	cam.server = &gortsplib.Server{
		Handler:     cam,
		RTSPAddress: "127.0.0.1:0",
		Listen: func(network, _ string) (net.Listener, error) {
			l, err := net.Listen(network, "127.0.0.1:0")
			if err == nil {
				cam.addr = l.Addr().String()
			}
			return l, err
		},
	}
	require.NoError(t, cam.server.Start())

	forma := &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: testSPS, PPS: testPPS}
	media := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{forma}}
	cam.stream = &gortsplib.ServerStream{
		Server: cam.server,
		Desc:   &description.Session{Medias: []*description.Media{media}},
	}
	require.NoError(t, cam.stream.Initialize())

	enc, err := forma.CreateEncoder()
	require.NoError(t, err)

	// 20 keyframes a second: every access unit is a random access point, so
	// the muxer closes a segment as soon as its minimum duration has passed.
	go func() {
		defer close(cam.done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		var ts uint32
		for {
			select {
			case <-cam.stop:
				return
			case <-tick.C:
			}
			pkts, err := enc.Encode([][]byte{testSPS, testPPS, {0x65, 0x88, 0x84, 0x00, 0x10}})
			if err != nil {
				return
			}
			for _, pkt := range pkts {
				pkt.Timestamp = ts
				_ = cam.stream.WritePacketRTP(media, pkt)
			}
			ts += 90000 / 20
		}
	}()

	t.Cleanup(func() {
		close(cam.stop)
		<-cam.done
		cam.stream.Close()
		cam.server.Close()
	})
	return cam
}

func newTestRestreamer(t *testing.T, st memSettings) (*Restreamer, *memRecorder) {
	t.Helper()
	rec := &memRecorder{}
	r := New(st, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(r.Stop)
	return r, rec
}

func get(r *Restreamer, file string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHLS(w, httptest.NewRequest(http.MethodGet, "/api/camera/hls/"+file, nil))
	return w
}

func TestServeHLSNotConfigured(t *testing.T) {
	r, rec := newTestRestreamer(t, memSettings{})
	assert.False(t, r.Enabled())
	assert.Equal(t, http.StatusNotFound, get(r, "index.m3u8").Code)
	assert.Empty(t, rec.events)
}

func TestServeHLSRejectsForeignFiles(t *testing.T) {
	r, _ := newTestRestreamer(t, memSettings{KeyURL: "rtsp://127.0.0.1:1/x"})
	for _, name := range []string{"settings", "index.html", "..%2Fsettings", "a.m3u8.exe"} {
		assert.Equal(t, http.StatusNotFound, get(r, name).Code, name)
	}
}

func TestRestreamWithCredentials(t *testing.T) {
	cam := startFakeCamera(t)
	st := memSettings{KeyURL: "rtsp://" + cam.addr + "/live", KeyUsername: camUser, KeyPassword: camPass}
	r, rec := newTestRestreamer(t, st)
	r.idleTimeout = 600 * time.Millisecond
	require.True(t, r.Enabled())

	// The first request connects, authenticates and blocks until the muxer
	// has a segment to offer.
	w := get(r, "index.m3u8")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "video1_stream.m3u8")
	assert.Contains(t, w.Body.String(), `CODECS="avc1.`)

	w = get(r, "video1_stream.m3u8")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "#EXT-X-PART")

	// A file the muxer does not know is a 404, not an empty 200.
	assert.Equal(t, http.StatusNotFound, get(r, "nope_seg1.mp4").Code)

	started := rec.byAction(events.ActionCameraStreamStarted)
	require.Len(t, started, 1)
	assert.Equal(t, string(events.SourceCamera), started[0].Source)
	assert.Equal(t, string(events.LevelInfo), started[0].Level)
	assert.Equal(t, 1, cam.openSessions())

	// With nobody asking, the camera connection is closed again.
	require.Eventually(t, func() bool {
		return len(rec.byAction(events.ActionCameraStreamStopped)) == 1
	}, 5*time.Second, 50*time.Millisecond)
	stopped := rec.byAction(events.ActionCameraStreamStopped)[0]
	assert.Equal(t, reasonIdle, stopped.Result.(map[string]any)["reason"])
	require.Eventually(t, func() bool { return cam.openSessions() == 0 }, 5*time.Second, 50*time.Millisecond)

	// The password never reaches the event log.
	assert.NotContains(t, rec.dump(t), camPass)
}

func TestRestreamWrongPassword(t *testing.T) {
	cam := startFakeCamera(t)
	st := memSettings{KeyURL: "rtsp://" + cam.addr + "/live", KeyUsername: camUser, KeyPassword: "wrong-pass"}
	r, rec := newTestRestreamer(t, st)

	w := get(r, "index.m3u8")
	require.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), "wrong-pass")

	// Players retry: the failure is answered from the backoff and recorded once.
	w = get(r, "index.m3u8")
	require.Equal(t, http.StatusBadGateway, w.Code)
	failed := rec.byAction(events.ActionCameraStreamStarted)
	require.Len(t, failed, 1)
	assert.Equal(t, string(events.LevelError), failed[0].Level)
	assert.NotContains(t, rec.dump(t), "wrong-pass")
	assert.Equal(t, 0, cam.openSessions())
}

func TestReloadDropsSession(t *testing.T) {
	cam := startFakeCamera(t)
	st := memSettings{KeyURL: "rtsp://" + cam.addr + "/live", KeyUsername: camUser, KeyPassword: camPass}
	r, rec := newTestRestreamer(t, st)

	require.Equal(t, http.StatusOK, get(r, "index.m3u8").Code)
	r.Reload()
	require.Eventually(t, func() bool {
		return len(rec.byAction(events.ActionCameraStreamStopped)) == 1
	}, 5*time.Second, 50*time.Millisecond)
	stopped := rec.byAction(events.ActionCameraStreamStopped)[0]
	assert.Equal(t, reasonConfig, stopped.Result.(map[string]any)["reason"])

	// The next viewer gets a fresh session.
	require.Equal(t, http.StatusOK, get(r, "index.m3u8").Code)
	assert.Len(t, rec.byAction(events.ActionCameraStreamStarted), 2)
}

func TestRedact(t *testing.T) {
	cfg := Config{Password: "p@ss word"}
	msg := redact("dial rtsp://u:p%40ss%20word@cam failed (p@ss word)", cfg)
	assert.False(t, strings.Contains(msg, "p@ss word") || strings.Contains(msg, "p%40ss%20word"), msg)
	assert.Equal(t, "plain", redact("plain", Config{}))
}
