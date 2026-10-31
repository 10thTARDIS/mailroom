// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// TestJMAPDraftPathNeverKeepsTheMessageID is Forgejo #311 item 4.
//
// compose.Draft.KeepMessageID re-uses an existing Message-ID instead of minting
// one. Wiring it into the JMAP draft path is a natural-looking change — JMAP
// clients like a stable messageId across an edit — and it would DESTROY THE
// DRAFT THE USER JUST SAVED.
//
// The mechanism, because "don't do that" without it is an instruction nobody can
// evaluate: internal/writeback's applyHeaderExpunge expunges every UID its
// Message-ID SEARCH returns. A create/destroy pair in one Email/set enqueues an
// APPEND of revision N+1 and a header-expunge of revision N; if both revisions
// carried the same Message-ID they would also share a created_at, making
// ClaimPendingIntents' ORDER BY a tie, and an APPEND that applied first would
// leave the SEARCH matching BOTH revisions. store.RetireDraft's contract
// promises the opposite ("it survives as a second draft"), and the only thing
// keeping that promise is that Build mints a fresh Message-ID per revision.
//
// ⚠ NOTHING PINNED THAT. build_test.go proves two Builds produce different
// Message-IDs, which is the generator's property; it says nothing about whether
// this package still lets the generator run. This is the missing half.
//
// Source-level for the same reason TestEveryIframeCarriesTheMessageSandbox is:
// the property is "which field the draft path sets", and exercising it for real
// would need a database, a writeback engine and an IMAP server to observe a
// failure that only appears when two intents tie on created_at.
func TestJMAPDraftPathNeverKeepsTheMessageID(t *testing.T) {
	// draft.go is where the JMAP draft is assembled; email.go holds the
	// Email/set create/destroy pairing that supplies the expunge half. Both must
	// stay clear of it.
	for _, file := range []string{"draft.go", "email.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, f); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		body := buf.String()

		// CONTROL ON THE SCAN. Comments are stripped above so prose about
		// KeepMessageID cannot trip the assertion — which means an empty or
		// unparsed result would pass silently. Each file must still contain
		// something only its real body has.
		anchor := map[string]string{"draft.go": "compose.DraftRequest{", "email.go": "func (a *API) emailSet"}[file]
		if !strings.Contains(body, anchor) {
			t.Fatalf("the parsed re-print of %s does not contain %q (%d bytes) — "+
				"the assertion below is not looking at the source it thinks it is",
				file, anchor, len(body))
		}

		if strings.Contains(body, "KeepMessageID") {
			t.Errorf("internal/api/%s sets compose.Draft.KeepMessageID. A draft revision MUST get "+
				"a fresh Message-ID: writeback's header-expunge deletes every UID matching the id, "+
				"so two revisions sharing one means the expunge of revision N can destroy revision "+
				"N+1 — the draft the user just saved (#311 item 4, see the comment on "+
				"compose.Draft.KeepMessageID).", file)
		}
	}
}
