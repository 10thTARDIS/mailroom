// SPDX-License-Identifier: AGPL-3.0-only

package importer

import (
	"bytes"
	"strings"
)

// This file turns what an archive says about a message into what Mailroom stores:
// a FOLDER (one mailbox — local_mailbox_id is single-valued), a set of FLAGS, and
// a set of LABELS (spec §6: labels live in Postgres, folders are mailboxes).
//
// # Google Takeout
//
// Takeout is one flat mbox of everything, and each message carries
// `X-Gmail-Labels: Inbox,Starred,Work,Category Personal`. Gmail conflates three
// different things in that one header, so the mapping splits them back apart:
//
//	Unread / Starred / Draft / Spam   -> FLAGS   (Gmail's per-message state)
//	Inbox / Sent / Trash / Spam /
//	  Draft / Archived                -> FOLDER  (Gmail's implicit "where is it")
//	everything else                   -> LABELS  (what a Gmail user calls a label)
//
// A message can carry several folder-ish labels at once (Inbox + Sent for mail you
// sent to yourself). local_mailbox_id holds exactly one mailbox, so they are
// resolved by priority — Trash, Spam, Drafts, Sent, Inbox, then Archive, which is
// where Gmail's "archived" mail (no folder-ish label at all) belongs.
//
// "Unread" is the one to be careful with: it is asserted, not implied, so its
// ABSENCE means the message was read. Getting this backwards would present the
// operator with an archive of twenty thousand unread messages.

// gmailLabelsHeader is Takeout's label header.
const gmailLabelsHeader = "X-Gmail-Labels"

// gmailFolder is a Gmail label that means a folder, with its priority (higher
// wins) and the mailbox it maps to.
type gmailFolder struct {
	mailbox  string
	priority int
}

var gmailFolders = map[string]gmailFolder{
	"inbox":    {"Inbox", 1},
	"archived": {"Archive", 0},
	"sent":     {"Sent", 2},
	"draft":    {"Drafts", 3},
	"spam":     {"Spam", 4},
	"trash":    {"Trash", 5},
	"bin":      {"Trash", 5}, // some locales export Trash as Bin
}

// classifyGmail maps an X-Gmail-Labels value onto (folder, flags, labels).
// It returns ok=false when the header is absent, so a non-Gmail mbox keeps the
// folder its file gave it.
func classifyGmail(value string) (folder string, flags []string, labels []string, ok bool) {
	if strings.TrimSpace(value) == "" {
		return "", nil, nil, false
	}
	var (
		unread   bool
		bestPrio = -1
	)
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, ",") {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		switch strings.ToLower(name) {
		case "unread":
			unread = true
			continue
		case "starred":
			flags = append(flags, `\Flagged`)
			continue
		case "opened":
			// Takeout's counterpart to Unread on some exports; it says nothing we
			// do not already infer from Unread's absence.
			continue
		}
		if f, isFolder := gmailFolders[strings.ToLower(name)]; isFolder {
			if f.priority > bestPrio {
				bestPrio, folder = f.priority, f.mailbox
			}
			switch strings.ToLower(name) {
			case "draft":
				flags = append(flags, `\Draft`)
			case "spam":
				flags = append(flags, "junk") // ingest.mapFlags -> is_junk
			}
			continue
		}
		labels = append(labels, name)
	}
	if !unread {
		flags = append(flags, `\Seen`)
	}
	if folder == "" {
		// Every folder-ish label absent is exactly what "archived" looks like in a
		// Takeout, which is also the largest single bucket in most Gmail accounts.
		folder = "Archive"
	}
	return folder, flags, labels, true
}

// statusFlags maps the Status:/X-Status: headers mutt, pine and friends write into
// an mbox onto IMAP flag tokens. They are the only flags a plain mbox carries.
//
//	Status:   R = read, O = old (delivered before this session; not a flag)
//	X-Status: A = answered, F = flagged, D = deleted, T = draft
func statusFlags(status, xstatus string) []string {
	var flags []string
	if strings.ContainsRune(status, 'R') {
		flags = append(flags, `\Seen`)
	}
	for _, c := range xstatus {
		switch c {
		case 'A':
			flags = append(flags, `\Answered`)
		case 'F':
			flags = append(flags, `\Flagged`)
		case 'T':
			flags = append(flags, `\Draft`)
		case 'D':
			flags = append(flags, `\Deleted`) // ingest drops it
		}
	}
	return flags
}

// headerValue pulls one header field out of a raw message, unfolding continuation
// lines. It is a deliberate hand-scan rather than a MIME parse: the importer needs
// three headers off the front of every message in a 25 GB archive, and it must not
// pay for a full parse to get them (ingest parses the message properly, once).
// It stops at the header/body separator and never allocates more than the header.
func headerValue(raw []byte, name string) string {
	end := headerEnd(raw)
	head := raw[:end]
	prefix := append([]byte(strings.ToLower(name)), ':')

	for i := 0; i < len(head); {
		lineEnd := bytes.IndexByte(head[i:], '\n')
		if lineEnd < 0 {
			lineEnd = len(head) - i
		} else {
			lineEnd++
		}
		line := head[i : i+lineEnd]
		if len(line) > len(prefix) && bytes.EqualFold(line[:len(prefix)], prefix) {
			var b strings.Builder
			b.Write(bytes.TrimSpace(line[len(prefix):]))
			// Unfold: a continuation line begins with a space or a tab.
			j := i + lineEnd
			for j < len(head) && (head[j] == ' ' || head[j] == '\t') {
				contEnd := bytes.IndexByte(head[j:], '\n')
				if contEnd < 0 {
					contEnd = len(head) - j
				} else {
					contEnd++
				}
				b.WriteByte(' ')
				b.Write(bytes.TrimSpace(head[j : j+contEnd]))
				j += contEnd
			}
			return b.String()
		}
		i += lineEnd
	}
	return ""
}

// headerEnd finds the end of the header block (the first empty line). A message
// with no body separator at all is all header — which is exactly what a truncated
// message looks like, and it must not send the scan through megabytes of body.
func headerEnd(raw []byte) int {
	if i := bytes.Index(raw, []byte("\n\r\n")); i >= 0 {
		return i + 1
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return i + 1
	}
	return len(raw)
}
