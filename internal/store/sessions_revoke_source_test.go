// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every statement that ends a session (sets app.sessions.revoked_at) must live in
// sessions.go, beside the push-subscription delete that goes with it
// (revokeSessionRow and revokeSessionsWhere, both calling deleteSessionPush). A
// new revoking UPDATE anywhere else, such as the one RenewSessionLocked used to
// carry, would end a session and leave its subscriptions receiving pushes, and no
// behavioural test would notice until someone wrote one for that path. This reads
// the source so the next such path fails here.
func TestEveryRevokedAtWriterDeletesPush(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		n := strings.Count(src, "UPDATE app.sessions SET revoked_at")
		if n > 0 && f != "sessions.go" {
			t.Errorf("%s sets app.sessions.revoked_at itself (%d time(s)); route it through revokeSessionRow so the session's push subscriptions are deleted in the same transaction", f, n)
		}
		if f == "sessions.go" {
			total = n
			// Each writer is paired with the shared delete.
			if c := strings.Count(src, "deleteSessionPush(ctx"); c < n {
				t.Errorf("sessions.go has %d revoking UPDATEs but only %d deleteSessionPush calls", n, c)
			}
		}
	}
	if total != 2 {
		t.Errorf("sessions.go has %d revoking UPDATEs, want 2 (revokeSessionRow, revokeSessionsWhere): update this guard deliberately if that changed", total)
	}
}
