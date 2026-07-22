package ocpi

import "regexp"

var tokenRe = regexp.MustCompile(`"token"\s*:\s*"[^"]*"`)

// RedactTokens strips credential token values from a JSON excerpt before it is
// persisted to the request log — tokens must never be stored or shown there.
func RedactTokens(s string) string {
	return tokenRe.ReplaceAllString(s, `"token":"[redacted]"`)
}
