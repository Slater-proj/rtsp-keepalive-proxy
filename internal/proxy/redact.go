package proxy

import (
	"regexp"
	"strings"
)

// Camera URLs often fail net/url parsing (raw '#', '%' or '@' in passwords),
// so credentials are masked with regular expressions on the raw text.
var (
	// scheme://user:password@  ->  keeps user, masks password. The greedy
	// [^/\s]* swallows a raw '@' inside the password: it stops at the last
	// '@' before the path.
	userPass = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/:@\s]*):[^/\s]*@`)
	// scheme://token@  ->  user-only credentials, masked entirely.
	userOnly = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/:@\s]+@`)
)

// RedactURL hides the credentials of an RTSP URL for logging:
// rtsp://admin:secret@10.0.0.1/s -> rtsp://admin:***@10.0.0.1/s
func RedactURL(raw string) string {
	out := userPass.ReplaceAllString(raw, "${1}:***@")
	return userOnly.ReplaceAllString(out, "${1}***@")
}

// redactErr returns err's text with credentials masked, so that errors
// bubbling up from the RTSP library never leak the camera password.
func (sh *StreamHandler) redactErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if sh.cfg.Source != "" {
		msg = strings.ReplaceAll(msg, sh.cfg.Source, RedactURL(sh.cfg.Source))
	}
	return RedactURL(msg)
}
