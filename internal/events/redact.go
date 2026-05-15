package events

import "regexp"

var sensitiveKeyPattern = regexp.MustCompile(`(?i)secret|token|password|key|api_key`)

// RedactSettings returns a copy of the input map with values for any key
// matching /secret|token|password|key|api_key/i replaced by "****"
// (empty values pass through unchanged so the absence of a configured
// secret stays visible). Use this before recording settings updates so
// secret material never lands in system_events.
func RedactSettings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if sensitiveKeyPattern.MatchString(k) && v != "" {
			out[k] = "****"
		} else {
			out[k] = v
		}
	}
	return out
}
