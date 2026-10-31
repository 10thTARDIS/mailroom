// SPDX-License-Identifier: AGPL-3.0-only

package thread

import "strings"

// subjectEntry caches a root container's effective subject analysis so it
// is computed once and reused across the table-building and merging
// passes below.
type subjectEntry struct {
	c       *container
	base    string // normalized base subject; "" if none / gave up
	isReply bool   // stripped prefix chain contained a "Re:"
	isFwd   bool   // stripped prefix chain contained a "Fwd:"/"Fw:"
}

// restrictedSubjectFallback implements JWZ step 5 with the restrictions
// documented in doc.go. See that comment for the full rationale; in short:
// only reply-form, non-forward subjects are ever pulled into another
// container, and never when both sides already have an established
// References-rooted identity (children) from before this step ran.
func restrictedSubjectFallback(roots []*container) []*container {
	entries := make([]subjectEntry, len(roots))
	hadIdentity := make(map[*container]bool, len(roots))
	for i, c := range roots {
		hadIdentity[c] = len(c.children) > 0
		subj, ok := findSubject(c)
		if !ok {
			entries[i] = subjectEntry{c: c}
			continue
		}
		base, isReply, isFwd := normalizeSubject(subj)
		entries[i] = subjectEntry{c: c, base: base, isReply: isReply, isFwd: isFwd}
	}

	table := buildSubjectTable(entries)

	out := make([]*container, 0, len(roots))
	for _, e := range entries {
		if e.base == "" || e.isFwd || !e.isReply {
			out = append(out, e.c)
			continue
		}
		target, ok := table[e.base]
		if !ok || target == e.c {
			out = append(out, e.c)
			continue
		}
		if hadIdentity[e.c] && hadIdentity[target] {
			// Restriction: never join two containers that each already
			// have a distinct References-rooted identity.
			out = append(out, e.c)
			continue
		}
		linkParentChild(target, e.c)
		if e.c.parent != target {
			// linkParentChild refused (e.g. would create a cycle, or e.c
			// already had a parent from an earlier merge in this pass);
			// keep it as its own root rather than silently drop it.
			out = append(out, e.c)
		}
	}
	return out
}

// buildSubjectTable populates one canonical container per base subject,
// per JWZ's tie-break rules: a Fwd-flagged subject never participates
// (neither as anchor nor source); an empty (dummy) container is preferred
// as the anchor over a real one; a non-reply subject is preferred over a
// reply subject as the anchor.
func buildSubjectTable(entries []subjectEntry) map[string]*container {
	table := make(map[string]*container, len(entries))
	replyOf := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.base == "" || e.isFwd {
			continue
		}
		cur, ok := table[e.base]
		if !ok {
			table[e.base] = e.c
			replyOf[e.base] = e.isReply
			continue
		}
		if cur.message == nil {
			continue // an empty container already anchors this subject
		}
		if e.c.message == nil {
			table[e.base] = e.c
			replyOf[e.base] = e.isReply
			continue
		}
		if replyOf[e.base] && !e.isReply {
			table[e.base] = e.c
			replyOf[e.base] = e.isReply
		}
	}
	return table
}

// findSubject returns the effective subject for a container: its own
// message's subject, or (for a dummy container, which after pruning can
// only occur at the root with real, non-dummy children) the first child's
// subject.
func findSubject(c *container) (string, bool) {
	if c.message != nil {
		return c.message.Subject, true
	}
	for _, child := range c.children {
		if s, ok := findSubject(child); ok {
			return s, true
		}
	}
	return "", false
}

// normalizeSubject strips leading "Re:"/"Fwd:"/"Fw:" markers and bracketed
// mailing-list tags ("[list-name] ...") repeatedly and in any order,
// case- and space-insensitively, reporting whether a reply and/or forward
// marker was seen along the way. The returned base is lower-cased with
// runs of whitespace collapsed to a single space, for equality comparison.
func normalizeSubject(subject string) (base string, isReply, isFwd bool) {
	s := subject
	for {
		s = strings.TrimSpace(s)

		if strings.HasPrefix(s, "[") {
			if idx := strings.Index(s, "]"); idx > 0 {
				s = s[idx+1:]
				continue
			}
		}

		lower := strings.ToLower(s)
		switch {
		case strings.HasPrefix(lower, "re:"):
			isReply = true
			s = s[len("re:"):]
			continue
		case strings.HasPrefix(lower, "fwd:"):
			isFwd = true
			s = s[len("fwd:"):]
			continue
		case strings.HasPrefix(lower, "fw:"):
			isFwd = true
			s = s[len("fw:"):]
			continue
		}
		break
	}

	fields := strings.Fields(strings.ToLower(s))
	base = strings.Join(fields, " ")
	return base, isReply, isFwd
}
