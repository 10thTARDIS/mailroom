// SPDX-License-Identifier: AGPL-3.0-only

// Package dsn recognizes and parses RFC 3464 Delivery Status Notifications
// (bounces) among ingested messages, so a bounce can be linked back to the
// message it reports on instead of sitting in the inbox as an opaque robot
// email (spec §4, Forgejo #30).
//
// Everything here is best-effort and panic-safe by contract. Real DSNs are
// malformed constantly, and a DSN we cannot fully parse must degrade to "record
// what we got" and NEVER crash or abort the ingest of the message carrying it.
// Parse therefore never panics and never returns an error: on hostile input it
// returns whatever could be recovered (possibly zero recipients).
package dsn

import (
	"strings"

	"mailroom/internal/mime"
)

// validActions is the closed set of RFC 3464 per-recipient actions, which is
// exactly the app.dsn_action enum. Anything outside it is normalized away before
// it can reach the column (the store guards this too — defense in depth).
var validActions = map[string]bool{
	"failed": true, "delayed": true, "delivered": true,
	"relayed": true, "expanded": true,
}

// Recipient is one per-recipient delivery-status record (RFC 3464 §2.3).
type Recipient struct {
	// Address is the reported recipient (Final-Recipient, falling back to
	// Original-Recipient), with the RFC 3464 address-type prefix stripped.
	Address string
	// Action is normalized to a valid app.dsn_action value (never "").
	Action string
	// Status is the RFC 3463 status code ("5.1.1"), or "" if absent/unusable.
	Status string
	// Diagnostic is the Diagnostic-Code text with its type prefix stripped and
	// whitespace collapsed, or "" if absent.
	Diagnostic string
}

// Report is a parsed DSN: the recovered original Message-ID (the link key back
// to the message we sent) plus one record per reported recipient.
type Report struct {
	// OriginalMessageID is the Message-ID of the message this DSN reports on,
	// recovered from the returned message/headers part. "" when unrecoverable.
	OriginalMessageID string
	// OriginalEnvelopeID is the Original-Envelope-Id from the per-message fields,
	// captured for completeness; linking is by Message-ID.
	OriginalEnvelopeID string
	// Recipients is one record per reported recipient (may be empty for a DSN
	// too malformed to yield any).
	Recipients []Recipient
}

// Parse detects whether m is a DSN and, if so, parses it best-effort. ok is
// false when m is not a DSN at all. A DSN that is malformed still returns
// ok=true with whatever could be recovered. Parse never panics and never
// returns an error.
func Parse(m *mime.Message) (report Report, ok bool) {
	// A hostile DSN must never crash ingest. If parsing panics after we have
	// already recognized the message as a DSN, ok stays true and the caller
	// records the partial report; a panic before recognition returns false.
	defer func() { _ = recover() }()

	if m == nil || m.Structure == nil {
		return Report{}, false
	}
	nodes := flatten(m.Structure)
	dsNode := findByType(nodes,
		"message/delivery-status", "message/global-delivery-status")
	if !isDSN(m.Structure, dsNode) {
		return Report{}, false
	}
	ok = true // recognized: everything from here is best-effort.

	if dsNode != nil {
		report.Recipients, report.OriginalEnvelopeID = parseDeliveryStatus(string(dsNode.Data))
	}
	report.OriginalMessageID = recoverOriginalMessageID(nodes)
	return report, ok
}

// isDSN recognizes a delivery status notification. The primary signal is a
// multipart/report container whose report-type is delivery-status (RFC 3462);
// the definitive fallback is the mere PRESENCE of a message/delivery-status
// part, which only a DSN ever carries, so a report wrapper with a missing or
// malformed report-type parameter is still recognized. (A MAILER-DAEMON From is
// a weak corroborating signal only — not required, and not sufficient, so it is
// deliberately not used: the structure is authoritative.)
func isDSN(root *mime.Structure, dsNode *mime.Structure) bool {
	if strings.EqualFold(root.ContentType, "multipart/report") {
		if strings.EqualFold(strings.TrimSpace(root.Params["report-type"]), "delivery-status") {
			return true
		}
	}
	return dsNode != nil
}

// recoverOriginalMessageID pulls the Message-ID of the bounced message out of
// the DSN's returned-content part — a full message/rfc822 copy, or a
// text/rfc822-headers headers-only copy (and their EAI message/global variants).
// It reuses the panic-safe MIME header parser rather than hand-rolling header
// extraction. "" when there is no returned part or it carries no Message-ID.
func recoverOriginalMessageID(nodes []*mime.Structure) string {
	returned := findByType(nodes,
		"message/rfc822", "message/global",
		"text/rfc822-headers", "message/global-headers")
	if returned == nil || len(returned.Data) == 0 {
		return ""
	}
	parsed, _ := mime.ParseBytes(returned.Data)
	if parsed == nil {
		return ""
	}
	return parsed.Header.MessageID()
}

// parseDeliveryStatus parses the body of a message/delivery-status part
// (RFC 3464 §2.1): RFC 822-style field groups separated by blank lines — one
// per-message group, then one group per recipient. It is intentionally lenient:
// it does not assume the first group is the per-message one, but treats any
// group carrying a recipient/action/status field as a recipient group, so a DSN
// that omits or reorders the per-message group still yields its recipients.
func parseDeliveryStatus(body string) ([]Recipient, string) {
	var recips []Recipient
	var envelopeID string
	for _, group := range splitGroups(body) {
		f := parseFields(group)
		if envelopeID == "" {
			envelopeID = f["original-envelope-id"]
		}
		addr := recipientAddress(f)
		action := f["action"]
		status := f["status"]
		if addr == "" && action == "" && status == "" {
			continue // a per-message group (Reporting-MTA, Arrival-Date, …).
		}
		recips = append(recips, Recipient{
			Address:    addr,
			Action:     normalizeAction(action, status),
			Status:     cleanStatus(status),
			Diagnostic: cleanDiagnostic(f["diagnostic-code"]),
		})
	}
	return recips, envelopeID
}

// recipientAddress returns the reported recipient, preferring Final-Recipient
// over Original-Recipient, with the "addr-type;" prefix and angle brackets
// stripped.
func recipientAddress(f map[string]string) string {
	for _, k := range []string{"final-recipient", "original-recipient"} {
		if v := f[k]; v != "" {
			return stripAddrType(v)
		}
	}
	return ""
}

// stripAddrType turns "rfc822; user@example.com" into "user@example.com".
func stripAddrType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[i+1:]
	}
	return strings.Trim(strings.TrimSpace(v), "<>")
}

// normalizeAction maps the Action field onto a valid app.dsn_action. A
// missing/garbage action is inferred from the RFC 3463 status class (2=delivered,
// 4=delayed, 5=failed); with nothing usable it defaults to "failed", since a DSN
// with no determinable action is, in practice, a bounce.
func normalizeAction(action, status string) string {
	if a := strings.ToLower(strings.TrimSpace(action)); validActions[a] {
		return a
	}
	switch statusClass(status) {
	case '2':
		return "delivered"
	case '4':
		return "delayed"
	case '5':
		return "failed"
	}
	return "failed"
}

// statusClass returns the leading digit of a status code, or 0.
func statusClass(status string) byte {
	if s := strings.TrimSpace(status); s != "" {
		return s[0]
	}
	return 0
}

// cleanStatus keeps a status code only when it looks like one (leads with a
// digit); anything else becomes "" (a NULL beats junk in a queryable column).
func cleanStatus(status string) string {
	s := strings.TrimSpace(status)
	if s == "" || s[0] < '0' || s[0] > '9' {
		return ""
	}
	return truncate(s, 32)
}

// cleanDiagnostic strips the "diag-type;" prefix from a Diagnostic-Code, folds
// its (often multi-line) text to a single space-separated line, and bounds its
// length. This is provider status text — never our message body.
func cleanDiagnostic(diag string) string {
	d := strings.TrimSpace(diag)
	if i := strings.IndexByte(d, ';'); i >= 0 {
		d = strings.TrimSpace(d[i+1:])
	}
	d = strings.Join(strings.Fields(d), " ")
	return truncate(d, 1024)
}

// flatten returns every node of a MIME tree in document order (a DSN's
// delivery-status and returned-content parts are leaves, so a flat scan finds
// them regardless of how the report is wrapped).
func flatten(root *mime.Structure) []*mime.Structure {
	var out []*mime.Structure
	var walk func(n *mime.Structure)
	walk = func(n *mime.Structure) {
		if n == nil {
			return
		}
		out = append(out, n)
		for _, c := range n.SubParts {
			walk(c)
		}
	}
	walk(root)
	return out
}

// findByType returns the first node whose (lowercased) content type matches any
// of types, or nil.
func findByType(nodes []*mime.Structure, types ...string) *mime.Structure {
	for _, n := range nodes {
		for _, t := range types {
			if strings.EqualFold(n.ContentType, t) {
				return n
			}
		}
	}
	return nil
}

// splitGroups splits an RFC 822-style body into field groups on blank lines,
// tolerant of CRLF or LF line endings.
func splitGroups(body string) []string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var groups []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return groups
}

// parseFields parses one field group into a lowercased-key map, unfolding
// continuation lines and keeping the FIRST value seen for a repeated field.
func parseFields(group string) map[string]string {
	f := make(map[string]string)
	for _, line := range unfold(group) {
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		val := strings.TrimSpace(line[i+1:])
		if _, exists := f[key]; !exists {
			f[key] = val
		}
	}
	return f
}

// unfold joins RFC 822 folded continuation lines (those beginning with
// whitespace) onto the preceding field line.
func unfold(group string) []string {
	var out []string
	for _, l := range strings.Split(group, "\n") {
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += " " + strings.TrimSpace(l)
			continue
		}
		out = append(out, l)
	}
	return out
}

// truncate bounds s to n runes without splitting a multi-byte rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
