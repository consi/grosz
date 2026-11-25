// Package camera restreams an RTSP camera to the dashboard as HLS.
//
// The camera is pulled on demand: the first playlist request opens the RTSP
// session and the session is closed again once nobody has asked for the
// stream for a while. Video is repackaged (H264/H265 into fMP4 segments)
// without transcoding, and audio is dropped, so the cost is close to a plain
// copy of the camera's bitrate. RTSP credentials stay on the server; the
// browser only ever talks to grosz, behind the normal web session.
package camera

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/rtp"

	"github.com/consi/grosz/internal/events"
)

const (
	// idleTimeout closes the camera session after the last HLS request.
	// Players poll continuously, so a quiet minute means nobody is watching.
	idleTimeout = 60 * time.Second
	// readyTimeout bounds how long a request waits for the RTSP handshake.
	readyTimeout = 20 * time.Second
	// retryBackoff keeps a failing camera from being redialled on every
	// player retry.
	retryBackoff = 5 * time.Second
	// failureLogInterval rate-limits identical connection-failure events.
	failureLogInterval = 10 * time.Minute
	// stopWait bounds how long Stop waits for the session to wind down.
	stopWait = 3 * time.Second

	videoClockRate = 90000
)

// Stop reasons recorded with the streamStopped event.
const (
	reasonIdle     = "idle"
	reasonConfig   = "configChanged"
	reasonShutdown = "shutdown"
)

var (
	errNotConfigured = errors.New("camera is not configured")
	errNoVideo       = errors.New("camera stream has no H264 or H265 video track")

	// hlsFile matches the playlists, init files, segments and parts that the
	// muxer serves; everything else is rejected before it reaches it.
	hlsFile = regexp.MustCompile(`^[A-Za-z0-9_]+\.(m3u8|mp4)$`)
)

// Restreamer owns the (at most one) live camera session.
type Restreamer struct {
	settings Settings
	events   *events.Bound
	log      *slog.Logger

	// Overridable in tests.
	idleTimeout time.Duration

	mu           sync.Mutex
	sess         *session
	lastErr      error
	lastFailure  time.Time
	lastLogged   string
	lastLoggedAt time.Time
	closed       bool
}

// New creates a restreamer. Nothing connects until the stream is requested.
func New(st Settings, rec events.Recorder, log *slog.Logger) *Restreamer {
	return &Restreamer{
		settings:    st,
		events:      events.For(events.SourceCamera, rec),
		log:         log.With("component", "camera"),
		idleTimeout: idleTimeout,
	}
}

// Enabled reports whether a camera URL is configured.
func (r *Restreamer) Enabled() bool {
	return r != nil && LoadConfig(r.settings).Enabled()
}

// Reload drops the running session so the next request picks up changed
// settings.
func (r *Restreamer) Reload() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sess != nil {
		r.sess.requestStop(reasonConfig)
		r.sess = nil
	}
	r.lastErr = nil
	r.lastLogged = ""
}

// Stop closes the running session and refuses new ones.
func (r *Restreamer) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	sess := r.sess
	r.sess = nil
	r.mu.Unlock()
	if sess == nil {
		return
	}
	sess.requestStop(reasonShutdown)
	select {
	case <-sess.done:
	case <-time.After(stopWait):
	}
}

// ServeHLS serves the HLS playlist and media files of the camera, starting
// the RTSP session when needed. The caller is responsible for authentication.
func (r *Restreamer) ServeHLS(w http.ResponseWriter, req *http.Request) {
	if r == nil || !hlsFile.MatchString(path.Base(req.URL.Path)) {
		http.NotFound(w, req)
		return
	}

	sess, err := r.acquire()
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNotConfigured) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}

	timeout := time.NewTimer(readyTimeout)
	defer timeout.Stop()
	select {
	case <-sess.ready:
	case <-sess.done:
		err := sess.err
		if err == nil {
			err = errors.New("camera stream stopped")
		}
		writeError(w, http.StatusBadGateway, err)
		return
	case <-req.Context().Done():
		return
	case <-timeout.C:
		writeError(w, http.StatusGatewayTimeout, errors.New("camera did not answer in time"))
		return
	}

	// Playlist requests block until the next part exists; a viewer waiting
	// on one is not idle.
	sess.active.Add(1)
	tw := &trackingWriter{ResponseWriter: w}
	sess.muxer.Handle(tw, req)
	sess.touch()
	sess.active.Add(-1)
	if !tw.wrote {
		// The muxer silently ignores files it does not know (for example a
		// segment of a previous session).
		http.NotFound(w, req)
	}
}

// acquire returns the running session, starting one if there is none.
func (r *Restreamer) acquire() (*session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, errors.New("camera restreamer is shut down")
	}
	cfg := LoadConfig(r.settings)
	if !cfg.Enabled() {
		return nil, errNotConfigured
	}
	if r.sess != nil {
		r.sess.touch()
		return r.sess, nil
	}
	if r.lastErr != nil && time.Since(r.lastFailure) < retryBackoff {
		return nil, r.lastErr
	}

	sess := &session{
		cfg:   cfg,
		ready: make(chan struct{}),
		done:  make(chan struct{}),
		stop:  make(chan struct{}),
	}
	sess.touch()
	r.sess = sess
	go r.run(sess)
	return sess, nil
}

// run drives one session from dial to teardown and records its outcome.
func (r *Restreamer) run(sess *session) {
	started := time.Now()
	input := map[string]any{"url": sess.cfg.URL, "auth": sess.cfg.Username != ""}

	err := r.stream(sess, input, started)
	if err != nil {
		err = errors.New(redact(err.Error(), sess.cfg))
	}
	sess.err = err
	wasLive := sess.isReady()
	close(sess.done)

	r.mu.Lock()
	if r.sess == sess {
		r.sess = nil
	}
	if err != nil {
		r.lastErr = err
		r.lastFailure = time.Now()
	} else if wasLive {
		r.lastErr = nil
		r.lastLogged = ""
	}
	// A camera that stays down is retried by every open dashboard; record the
	// same failure only once per interval.
	logFailure := false
	if err != nil && (err.Error() != r.lastLogged || time.Since(r.lastLoggedAt) > failureLogInterval) {
		r.lastLogged = err.Error()
		r.lastLoggedAt = time.Now()
		logFailure = true
	}
	r.mu.Unlock()

	duration := int(time.Since(started).Seconds())
	switch {
	case err == nil && wasLive:
		r.log.Info("camera stream stopped", "reason", sess.stopReason(), "duration_s", duration)
		r.events.Info(events.ActionCameraStreamStopped, input,
			map[string]any{"reason": sess.stopReason(), "durationSec": duration})
	case err != nil && wasLive:
		r.log.Warn("camera stream lost", "err", err, "duration_s", duration)
		if logFailure {
			r.events.Warn(events.ActionCameraStreamStopped, input,
				map[string]any{"error": err.Error(), "durationSec": duration})
		}
	case err != nil:
		r.log.Warn("camera stream failed to start", "err", err)
		if logFailure {
			r.events.Error(events.ActionCameraStreamStarted, input, err)
		}
	}

	// A live session buffers several seconds of video (tens of megabytes for
	// a 4K camera). Hand that back to the system now rather than whenever the
	// runtime gets around to it: grosz usually runs on a small box.
	if wasLive {
		debug.FreeOSMemory()
	}
}

// stream connects to the camera and feeds the HLS muxer until the session is
// stopped (nil) or breaks (error).
func (r *Restreamer) stream(sess *session, input map[string]any, started time.Time) error {
	u, err := base.ParseURL(sess.cfg.URL)
	if err != nil {
		return ErrInvalidURL
	}
	if sess.cfg.Username != "" {
		u.User = url.UserPassword(sess.cfg.Username, sess.cfg.Password)
	}

	// Interleaved TCP: no lost packets (which smear a 4K picture until the
	// next keyframe) and no UDP port juggling through firewalls.
	transport := gortsplib.ProtocolTCP
	client := &gortsplib.Client{
		Scheme:    u.Scheme,
		Host:      u.Host,
		Protocol:  &transport,
		UserAgent: "grosz",
	}
	if u.Scheme == "rtsps" {
		// Cameras ship self-signed certificates; there is no CA to verify
		// against. TLS still keeps credentials and video off the wire in clear.
		client.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // self-signed camera certificates
	}
	if err := client.Start(); err != nil {
		return err
	}
	defer client.Close()

	// A stop request must also interrupt the blocking handshake below.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-sess.stop:
			client.Close()
		case <-finished:
		}
	}()

	fatal := make(chan error, 1)
	muxer, codec, err := r.setup(client, u, fatal)
	if err != nil {
		if sess.stopped() {
			return nil
		}
		return err
	}
	// The RTSP reader writes into the muxer: stop it before closing the muxer.
	defer func() {
		client.Close()
		muxer.Close()
	}()

	if _, err := client.Play(nil); err != nil {
		if sess.stopped() {
			return nil
		}
		return err
	}

	sess.muxer = muxer
	close(sess.ready)
	connectMs := time.Since(started).Milliseconds()
	r.log.Info("camera stream started", "url", sess.cfg.URL, "codec", codec, "connect_ms", connectMs)
	r.events.Info(events.ActionCameraStreamStarted, input,
		map[string]any{"codec": codec, "connectMs": connectMs})

	lost := make(chan error, 1)
	go func() { lost <- client.Wait() }()
	tick := time.NewTicker(min(5*time.Second, r.idleTimeout/2))
	defer tick.Stop()
	for {
		select {
		case <-sess.stop:
			return nil
		case err := <-lost:
			if sess.stopped() {
				return nil
			}
			return fmt.Errorf("camera connection lost: %w", err)
		case err := <-fatal:
			return err
		case <-tick.C:
			if sess.active.Load() == 0 && sess.idleFor() > r.idleTimeout {
				sess.requestStop(reasonIdle)
				return nil
			}
		}
	}
}

// setup describes the stream, subscribes to its video track and wires the
// RTP packets into a fresh HLS muxer.
func (r *Restreamer) setup(client *gortsplib.Client, u *base.URL, fatal chan<- error) (*gohlslib.Muxer, string, error) {
	desc, _, err := client.Describe(u)
	if err != nil {
		return nil, "", err
	}

	var (
		media  *description.Media
		forma  format.Format
		track  *gohlslib.Track
		codec  string
		decode func(*rtp.Packet) ([][]byte, error)
		write  func(*gohlslib.Muxer, int64, [][]byte) error
	)
	var f264 *format.H264
	var f265 *format.H265
	if m := desc.FindFormat(&f264); m != nil {
		dec, err := f264.CreateDecoder()
		if err != nil {
			return nil, "", err
		}
		media, forma, codec, decode = m, f264, describeH264(f264.SPS), dec.Decode
		track = &gohlslib.Track{Codec: &codecs.H264{SPS: f264.SPS, PPS: f264.PPS}, ClockRate: videoClockRate}
		write = func(m *gohlslib.Muxer, pts int64, au [][]byte) error {
			return m.WriteH264(track, time.Now(), pts, au)
		}
	} else if m := desc.FindFormat(&f265); m != nil {
		dec, err := f265.CreateDecoder()
		if err != nil {
			return nil, "", err
		}
		media, forma, codec, decode = m, f265, describeH265(f265.SPS), dec.Decode
		track = &gohlslib.Track{Codec: &codecs.H265{VPS: f265.VPS, SPS: f265.SPS, PPS: f265.PPS}, ClockRate: videoClockRate}
		write = func(m *gohlslib.Muxer, pts int64, au [][]byte) error {
			return m.WriteH265(track, time.Now(), pts, au)
		}
	} else {
		return nil, "", errNoVideo
	}

	if _, err := client.Setup(desc.BaseURL, media, 0, 0); err != nil {
		return nil, "", err
	}

	muxer := &gohlslib.Muxer{
		Tracks: []*gohlslib.Track{track},
		// Low-latency HLS publishes ~200 ms parts, so playback is not held
		// back by the camera's keyframe interval (often several seconds).
		Variant:      gohlslib.MuxerVariantLowLatency,
		SegmentCount: 7,
		OnEncodeError: func(err error) {
			r.log.Debug("camera encode error", "err", err)
		},
	}
	if err := muxer.Start(); err != nil {
		return nil, "", err
	}

	client.OnPacketRTP(media, forma, func(pkt *rtp.Packet) {
		pts, ok := client.PacketPTS(media, pkt)
		if !ok {
			return
		}
		// Decode errors here are "need more packets" or a fragment whose
		// start was missed; the access unit simply is not complete yet.
		au, err := decode(pkt)
		if err != nil || len(au) == 0 {
			return
		}
		if err := write(muxer, pts, au); err != nil {
			select {
			case fatal <- fmt.Errorf("camera stream cannot be repackaged: %w", err):
			default:
			}
		}
	})
	return muxer, codec, nil
}

func describeH264(sps []byte) string {
	var s h264.SPS
	if len(sps) == 0 || s.Unmarshal(sps) != nil {
		return "H264"
	}
	return fmt.Sprintf("H264 %dx%d", s.Width(), s.Height())
}

func describeH265(sps []byte) string {
	var s h265.SPS
	if len(sps) == 0 || s.Unmarshal(sps) != nil {
		return "H265"
	}
	return fmt.Sprintf("H265 %dx%d", s.Width(), s.Height())
}

// session is one RTSP connection and the HLS muxer it feeds.
type session struct {
	cfg Config

	ready chan struct{} // closed once muxer is serving
	done  chan struct{} // closed when the session has ended; err is then set
	stop  chan struct{} // closed to request teardown
	muxer *gohlslib.Muxer
	err   error

	stopOnce   sync.Once
	reason     atomic.Value // string
	lastAccess atomic.Int64 // unix nanoseconds
	active     atomic.Int32 // requests currently inside the muxer
}

func (s *session) touch() { s.lastAccess.Store(time.Now().UnixNano()) }

func (s *session) idleFor() time.Duration {
	return time.Since(time.Unix(0, s.lastAccess.Load()))
}

func (s *session) requestStop(reason string) {
	s.stopOnce.Do(func() {
		s.reason.Store(reason)
		close(s.stop)
	})
}

func (s *session) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *session) isReady() bool {
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

func (s *session) stopReason() string {
	reason, _ := s.reason.Load().(string)
	return reason
}

// redact removes the RTSP password from an error text before it is shown,
// logged or recorded.
func redact(msg string, cfg Config) string {
	if cfg.Password == "" {
		return msg
	}
	userinfo := strings.TrimPrefix(url.UserPassword("", cfg.Password).String(), ":")
	for _, secret := range []string{cfg.Password, userinfo, url.QueryEscape(cfg.Password), url.PathEscape(cfg.Password)} {
		msg = strings.ReplaceAll(msg, secret, "****")
	}
	return msg
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

// trackingWriter records whether the wrapped handler produced a response.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(status int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}
