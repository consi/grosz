package camera

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitURL(t *testing.T) {
	tests := []struct {
		name              string
		in                string
		clean, user, pass string
		wantErr           bool
	}{
		{name: "plain", in: "rtsp://cam.local/stream1", clean: "rtsp://cam.local/stream1"},
		{name: "port and query", in: " rtsp://10.0.0.5:554/h264?channel=1 ", clean: "rtsp://10.0.0.5:554/h264?channel=1"},
		{name: "credentials", in: "rtsp://admin:s3cr3t@cam.local:554/live", clean: "rtsp://cam.local:554/live", user: "admin", pass: "s3cr3t"},
		{name: "escaped password", in: "rtsp://admin:p%40ss%3Aword@cam.local/live", clean: "rtsp://cam.local/live", user: "admin", pass: "p@ss:word"},
		{name: "user only", in: "rtsp://viewer@cam.local/live", clean: "rtsp://cam.local/live", user: "viewer"},
		{name: "tls", in: "rtsps://cam.local:322/live", clean: "rtsps://cam.local:322/live"},
		{name: "http", in: "http://cam.local/stream", wantErr: true},
		{name: "no scheme", in: "cam.local/stream", wantErr: true},
		{name: "no host", in: "rtsp:///stream", wantErr: true},
		{name: "garbage", in: "rtsp://[::1", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean, user, pass, err := SplitURL(tt.in)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrInvalidURL)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.clean, clean)
			assert.Equal(t, tt.user, user)
			assert.Equal(t, tt.pass, pass)
		})
	}
}

func TestNormalizeUpdates(t *testing.T) {
	tests := []struct {
		name    string
		in      map[string]string
		want    map[string]string
		changed bool
		wantErr bool
	}{
		{
			name: "unrelated settings are left alone",
			in:   map[string]string{"log.level": "debug"},
			want: map[string]string{"log.level": "debug"},
		},
		{
			name:    "credentials move out of the URL",
			in:      map[string]string{KeyURL: "rtsp://admin:s3cr3t@cam.local/live"},
			want:    map[string]string{KeyURL: "rtsp://cam.local/live", KeyUsername: "admin", KeyPassword: "s3cr3t"},
			changed: true,
		},
		{
			name:    "empty password means unchanged",
			in:      map[string]string{KeyURL: "rtsp://cam.local/live", KeyUsername: "admin", KeyPassword: ""},
			want:    map[string]string{KeyURL: "rtsp://cam.local/live", KeyUsername: "admin"},
			changed: true,
		},
		{
			name:    "clearing the username clears the password",
			in:      map[string]string{KeyUsername: " "},
			want:    map[string]string{KeyUsername: "", KeyPassword: ""},
			changed: true,
		},
		{
			name:    "new password is kept verbatim",
			in:      map[string]string{KeyPassword: " spaced "},
			want:    map[string]string{KeyPassword: " spaced "},
			changed: true,
		},
		{
			name:    "empty URL turns the camera off and keeps credentials",
			in:      map[string]string{KeyURL: "  "},
			want:    map[string]string{KeyURL: ""},
			changed: true,
		},
		{
			name:    "invalid URL is rejected",
			in:      map[string]string{KeyURL: "http://cam.local/live"},
			changed: true,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed, err := NormalizeUpdates(tt.in)
			assert.Equal(t, tt.changed, changed)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrInvalidURL)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, tt.in)
		})
	}
}
