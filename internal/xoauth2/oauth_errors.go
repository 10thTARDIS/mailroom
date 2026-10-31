// SPDX-License-Identifier: AGPL-3.0-only

package xoauth2

import "strings"

// MaxDescription caps an OAuth error_description in our errors and logs.
const MaxDescription = 200

// OAuthErrorCode keeps an OAuth error code only if it looks like one: a short
// token of lowercase letters, digits and underscores. Anything else is replaced
// by "unrecognised_error", so a hostile body cannot put arbitrary text into a
// log line, a state reason or the audit log. An empty code stays empty.
//
// Shared by internal/oauthmail (mail-provider grants) and internal/auth
// (sign-in session renewal, ADR-048).
func OAuthErrorCode(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 48 {
		return "unrecognised_error"
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "unrecognised_error"
		}
	}
	return s
}

// CleanDescription makes a provider's error_description safe to log: every
// secret the request carried is scrubbed out, control characters are replaced,
// and the text is capped at MaxDescription bytes.
func CleanDescription(s string, secrets ...Secret) string {
	s = Scrub(s, secrets...)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > MaxDescription {
		s = s[:MaxDescription] + "…"
	}
	return s
}
