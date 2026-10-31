// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// roleForMailbox must recognize the reserved IMAP name INBOX as the "inbox" role
// even though it carries no SPECIAL-USE attribute (RFC 6154 defines no `\Inbox`);
// otherwise the inbox is role-less and drops out of inbox-role views such as the
// client's unified inbox. Every other role still comes from SPECIAL-USE.
func TestRoleForMailbox(t *testing.T) {
	cases := []struct {
		name       string
		imapName   string
		specialUse string
		want       string
	}{
		{"inbox, no special-use (the real-world case)", "INBOX", "", "inbox"},
		{"inbox, case-insensitive reserved name", "inbox", "", "inbox"},
		{"inbox even if a server also tagged it", "INBOX", `\Inbox`, "inbox"},
		{"a child of inbox is NOT the inbox", "INBOX/Receipts", "", ""},
		{"sent from special-use", "Sent", `\Sent`, "sent"},
		{"archive from special-use", "Archive", `\Archive`, "archive"},
		{"trash from special-use", "Trash", `\Trash`, "trash"},
		{"junk from special-use", "Junk", `\Junk`, "junk"},
		{"drafts from special-use", "Drafts", `\Drafts`, "drafts"},
		{"a plain custom folder has no role", "Receipts", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := roleForMailbox(c.imapName, c.specialUse); got != c.want {
				t.Errorf("roleForMailbox(%q, %q) = %q, want %q",
					c.imapName, c.specialUse, got, c.want)
			}
		})
	}
}
