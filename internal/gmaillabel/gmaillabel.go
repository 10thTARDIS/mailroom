// SPDX-License-Identifier: AGPL-3.0-only

// Package gmaillabel maps Gmail's X-GM-LABELS onto Mailroom's model (#52,
// ADR-047 §2). It is pure: no IMAP, no database, so the sync path (store),
// the census (gmailsync) and their tests all classify a label set the same
// way.
//
// Gmail conflates three things in one label list, and the mapping splits them
// back apart, with the same priority the Takeout importer uses
// (internal/importer/gmail.go classifyGmail):
//
//	\Inbox \Sent \Draft(s) [\Spam \Trash]   -> the FOLDER (one; by priority)
//	\Starred \Important and every other     -> IGNORED here: \Starred arrives
//	  backslash label                          as the \Flagged FLAG already,
//	                                           and Mailroom has its own
//	                                           priority and tabs
//	everything else                         -> USER labels (Mailroom labels)
//
// A message with no folder-like label is archived: it lives in All Mail.
package gmaillabel

import (
	"strings"
	"unicode"
)

// Folder values: the role of the Mailroom mailbox a label set places a
// message in. Archive means Gmail's All Mail (no folder-like label).
const (
	FolderArchive = "archive"
	FolderInbox   = "inbox"
	FolderSent    = "sent"
	FolderDrafts  = "drafts"
	FolderSpam    = "junk"
	FolderTrash   = "trash"
)

// folderPriority is classifyGmail's order: Trash > Spam > Drafts > Sent >
// Inbox > Archive. Mail you sent yourself carries \Inbox and \Sent, and
// lands in Sent, as it does from a Takeout.
var folderPriority = map[string]struct {
	folder string
	prio   int
}{
	`\inbox`:  {FolderInbox, 1},
	`\sent`:   {FolderSent, 2},
	`\draft`:  {FolderDrafts, 3},
	`\drafts`: {FolderDrafts, 3},
	`\spam`:   {FolderSpam, 4},
	`\junk`:   {FolderSpam, 4},
	`\trash`:  {FolderTrash, 5},
}

// Classified is one label set, split.
type Classified struct {
	// Folder is one of the Folder* constants; FolderArchive when no
	// folder-like label is present.
	Folder string
	// User are the user labels, in the order given, without duplicates.
	User []string
	// System are every backslash label, in the order given (folder-like ones
	// included), for counting.
	System []string
}

// IsSystem reports whether a decoded label is a Gmail system label
// (`\Inbox`, `\Important`, ...).
func IsSystem(label string) bool { return strings.HasPrefix(label, `\`) }

// Classify splits a decoded X-GM-LABELS list.
func Classify(labels []string) Classified {
	out := Classified{Folder: FolderArchive}
	best := 0
	seen := map[string]bool{}
	for _, l := range labels {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		if IsSystem(l) {
			out.System = append(out.System, l)
			if f, ok := folderPriority[strings.ToLower(l)]; ok && f.prio > best {
				best, out.Folder = f.prio, f.folder
			}
			continue
		}
		out.User = append(out.User, l)
	}
	return out
}

// Sanitize makes a decoded label set safe to store (#52 review H2): invalid
// UTF-8 is replaced, and NUL plus every other C0/C1 control character is
// removed — PostgreSQL's text type rejects NUL outright, and modified UTF-7
// can encode one ("&AAA-" decodes to "\x00"), so one hostile label would
// otherwise fail the message's ingest on every pass. A label that sanitises
// to nothing is dropped. NIL STAYS NIL: nil means "no reading", which is not
// the same as an empty label set, and nothing here may turn one into the other.
func Sanitize(labels []string) []string {
	if labels == nil {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		l = strings.ToValidUTF8(l, "\uFFFD")
		l = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, l)
		if l == "" || l == `\` {
			continue
		}
		out = append(out, l)
	}
	return out
}

// UserLabels returns just the user labels of a label set.
func UserLabels(labels []string) []string { return Classify(labels).User }

// Diff returns the user labels added and removed going from prior to next.
// A nil prior means "never observed": everything in next is added and
// nothing is removed.
func Diff(prior, next []string) (added, removed []string) {
	was := map[string]bool{}
	for _, l := range UserLabels(prior) {
		was[l] = true
	}
	now := map[string]bool{}
	for _, l := range UserLabels(next) {
		now[l] = true
		if !was[l] {
			added = append(added, l)
		}
	}
	for _, l := range UserLabels(prior) {
		if !now[l] {
			removed = append(removed, l)
		}
	}
	return added, removed
}

// Equal reports whether two label sets are the same set.
func Equal(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	set := map[string]int{}
	for _, l := range a {
		set[l] |= 1
	}
	for _, l := range b {
		set[l] |= 2
	}
	for _, v := range set {
		if v != 3 {
			return false
		}
	}
	return true
}
