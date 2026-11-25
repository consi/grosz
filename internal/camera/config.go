package camera

import (
	"errors"
	"net/url"
	"strings"
)

// Settings keys of the camera preview.
const (
	KeyURL      = "camera.rtsp_url"
	KeyUsername = "camera.rtsp_username"
	KeyPassword = "camera.rtsp_password"
)

// ErrInvalidURL is returned for a camera URL that is not a usable RTSP address.
var ErrInvalidURL = errors.New("camera URL must look like rtsp://host[:port]/path (or rtsps://)")

// Settings is the read access to the settings store the restreamer needs.
type Settings interface {
	GetDefault(key, defaultVal string) string
}

// Config is the camera source as configured in settings. The URL never
// carries credentials; they live in Username and Password.
type Config struct {
	URL      string
	Username string
	Password string
}

// Enabled reports whether a camera is configured.
func (c Config) Enabled() bool { return c.URL != "" }

// LoadConfig reads the camera configuration from settings.
func LoadConfig(st Settings) Config {
	return Config{
		URL:      strings.TrimSpace(st.GetDefault(KeyURL, "")),
		Username: st.GetDefault(KeyUsername, ""),
		Password: st.GetDefault(KeyPassword, ""),
	}
}

// SplitURL validates an RTSP URL and separates the credentials embedded in
// it (rtsp://user:pass@host/path). The returned URL is safe to show and log.
func SplitURL(raw string) (clean, username, password string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", "", ErrInvalidURL
	}
	if (u.Scheme != "rtsp" && u.Scheme != "rtsps") || u.Hostname() == "" || u.Opaque != "" || u.Fragment != "" {
		return "", "", "", ErrInvalidURL
	}
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
		u.User = nil
	}
	return u.String(), username, password, nil
}

// NormalizeUpdates validates and rewrites the camera keys of a settings
// update in place, before it is stored or logged:
//   - credentials typed into the URL move to the username/password settings,
//     so the stored URL is free of secrets;
//   - an empty password means "unchanged", because the API never returns the
//     stored one; clearing the username removes the password with it.
//
// It reports whether the update touches the camera at all.
func NormalizeUpdates(updates map[string]string) (changed bool, err error) {
	rawURL, hasURL := updates[KeyURL]
	_, hasUser := updates[KeyUsername]
	_, hasPass := updates[KeyPassword]
	if !hasURL && !hasUser && !hasPass {
		return false, nil
	}

	if hasUser {
		updates[KeyUsername] = strings.TrimSpace(updates[KeyUsername])
	}
	if hasURL {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL != "" {
			clean, user, pass, err := SplitURL(rawURL)
			if err != nil {
				return true, err
			}
			rawURL = clean
			if user != "" {
				updates[KeyUsername] = user
				updates[KeyPassword] = pass
				hasUser = true
			}
		}
		updates[KeyURL] = rawURL
	}

	switch {
	case hasUser && updates[KeyUsername] == "":
		updates[KeyPassword] = ""
	case updates[KeyPassword] == "":
		delete(updates, KeyPassword)
	}
	return true, nil
}
