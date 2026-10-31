// SPDX-License-Identifier: AGPL-3.0-only

package writeback

// Unit coverage for the ingest-only backstop (ADR-028 §3).
//
// This is the half of the guarantee the integration probe cannot reach. That
// probe deletes DrainAccount's guard and shows the never-dials test goes red
// AT THE DIAL — which proves the guard is load-bearing, and then stops. What
// happens if something gets past the guard and obtains a writer anyway is what
// these tests pin.
//
// No database and no IMAP: RefusingWriter is a pure value.

import (
	"context"
	"errors"
	"strings"
	"testing"

	imapsync "mailroom/internal/sync"
)

// TestRefusingWriterRefusesEveryMutation walks every mutating method on the
// IMAPWriter interface and requires ErrWritebackDisabled from each.
//
// ⚠ IT IS DELIBERATELY EXHAUSTIVE RATHER THAN REPRESENTATIVE. A backstop that
// covers five of six operations is not a backstop; the one it misses is the one
// that reaches the user's mailbox. The compile-time `var _ IMAPWriter` in
// refusing_writer.go catches a NEW method being added to the interface, and
// this catches an existing one quietly starting to succeed.
func TestRefusingWriterRefusesEveryMutation(t *testing.T) {
	ctx := context.Background()
	w := RefusingWriter{Account: "acct-1"} // nil Log: refuse silently

	t.Run("StoreFlags", func(t *testing.T) {
		requireRefusal(t, w.StoreFlags(ctx, "INBOX", 1,
			[]imapsync.Flag{`\Seen`}, nil))
	})
	t.Run("Copy", func(t *testing.T) {
		requireRefusal(t, w.Copy(ctx, "INBOX", 1, "Archive"))
	})
	t.Run("Move", func(t *testing.T) {
		requireRefusal(t, w.Move(ctx, "INBOX", 1, "Archive"))
	})
	t.Run("Expunge", func(t *testing.T) {
		n, err := w.Expunge(ctx, "INBOX", 1)
		requireRefusal(t, err)
		// ⚠ 0 AND an error, never 0 with nil. Expunge's contract says a zero
		// count with no error is a legitimate "there was nothing there" (#268),
		// so refusing that way would report the message as already destroyed.
		if n != 0 {
			t.Errorf("Expunge count = %d, want 0", n)
		}
	})
	t.Run("Append", func(t *testing.T) {
		uid, err := w.Append(ctx, "Drafts", nil, []byte("raw"))
		requireRefusal(t, err)
		if uid != 0 {
			t.Errorf("Append uid = %d, want 0", uid)
		}
	})
	t.Run("SearchMessageID", func(t *testing.T) {
		uids, err := w.SearchMessageID(ctx, "Sent", "<m@example.com>")
		requireRefusal(t, err)
		// ⚠ NOT an empty slice with a nil error. The engine uses this probe to
		// decide whether an APPEND would duplicate and which draft copy to
		// expunge; an empty SEARCH treated as success is exactly what created a
		// stale draft and then a loop in #268.
		if uids != nil {
			t.Errorf("SearchMessageID = %v, want nil", uids)
		}
	})
	t.Run("UIDPresent", func(t *testing.T) {
		present, err := w.UIDPresent(ctx, "Drafts", 37)
		requireRefusal(t, err)
		// ⚠ NOT false with a nil error, for the same reason as SearchMessageID:
		// the engine's #611 re-observation reads false as "genuinely gone",
		// which would tell it a message nobody actually asked the server about
		// is safe to treat as destroyed.
		if present {
			t.Errorf("UIDPresent = %v, want false", present)
		}
	})
}

// TestRefusingWriterReportsNoMoveExtension pins the one method that cannot
// fail. false routes a move down the COPY+EXPUNGE fallback, every step of
// which is itself refused, rather than down the single-shot MOVE.
func TestRefusingWriterReportsNoMoveExtension(t *testing.T) {
	if (RefusingWriter{}).SupportsMove() {
		t.Error("SupportsMove() = true, want false")
	}
}

// TestRefusingWriterErrorNamesTheOperation checks the refusal is diagnosable.
// Reaching this type at all means a layer above has a hole, so the error has to
// say which operation was attempted and where — otherwise a structural defect
// surfaces as an unattributable failure.
func TestRefusingWriterErrorNamesTheOperation(t *testing.T) {
	err := RefusingWriter{}.Copy(context.Background(), "Archive", 7, "Trash")
	if err == nil {
		t.Fatal("Copy returned nil")
	}
	msg := err.Error()
	for _, want := range []string{"COPY", "Archive"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func requireRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("operation succeeded, want ErrWritebackDisabled")
	}
	if !errors.Is(err, ErrWritebackDisabled) {
		t.Fatalf("error = %v, want it to wrap ErrWritebackDisabled", err)
	}
}
