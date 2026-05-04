package ocpp

import (
	"net/http"
	"testing"
)

func TestClientIPFromRequest(t *testing.T) {
	tests := []struct {
		name       string
		xff        string
		xRealIP    string
		remoteAddr string
		want       string
	}{
		{
			name:       "XFF single value wins over RemoteAddr",
			xff:        "203.0.113.5",
			remoteAddr: "198.51.100.190:37850",
			want:       "203.0.113.5",
		},
		{
			name:       "XFF leftmost when multi-hop",
			xff:        "203.0.113.5, 10.0.0.1, 198.51.100.190",
			remoteAddr: "198.51.100.190:37850",
			want:       "203.0.113.5",
		},
		{
			name:       "XFF tolerates whitespace",
			xff:        "  203.0.113.5  ,10.0.0.1",
			remoteAddr: "198.51.100.190:37850",
			want:       "203.0.113.5",
		},
		{
			name:       "X-Real-IP when no XFF",
			xRealIP:    "203.0.113.7",
			remoteAddr: "198.51.100.190:37850",
			want:       "203.0.113.7",
		},
		{
			name:       "RemoteAddr fallback strips port",
			remoteAddr: "198.51.100.50:54321",
			want:       "198.51.100.50",
		},
		{
			name:       "RemoteAddr without port",
			remoteAddr: "198.51.100.50",
			want:       "198.51.100.50",
		},
		{
			name:       "Malformed XFF falls through to X-Real-IP",
			xff:        "not-an-ip",
			xRealIP:    "203.0.113.7",
			remoteAddr: "198.51.100.190:37850",
			want:       "203.0.113.7",
		},
		{
			name:       "Malformed XFF and X-Real-IP fall through to RemoteAddr",
			xff:        "garbage",
			xRealIP:    "also-garbage",
			remoteAddr: "198.51.100.190:37850",
			want:       "198.51.100.190",
		},
		{
			name:       "IPv6 in XFF",
			xff:        "2001:db8::1",
			remoteAddr: "[::1]:37850",
			want:       "2001:db8::1",
		},
		{
			name:       "IPv6 RemoteAddr fallback",
			remoteAddr: "[2001:db8::1]:37850",
			want:       "2001:db8::1",
		},
		{
			name: "Empty everything returns empty",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}, RemoteAddr: tc.remoteAddr}
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xRealIP != "" {
				r.Header.Set("X-Real-IP", tc.xRealIP)
			}
			got := clientIPFromRequest(r)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClientIPFromRequest_NilRequest(t *testing.T) {
	if got := clientIPFromRequest(nil); got != "" {
		t.Errorf("nil request: got %q, want empty", got)
	}
}
