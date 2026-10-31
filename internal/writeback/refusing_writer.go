// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	imapsync "mailroom/internal/sync"
)

// ErrWritebackDisabled is returned by every mutating method of RefusingWriter.
// It is deliberately a sentinel: a test asserting the ingest-only guarantee
// should be able to say "this exact refusal", not pattern-match a string.
var ErrWritebackDisabled = errors.New("writeback: refused — this account is in ingest-only mode (ADR-028)")

// RefusingWriter is an IMAPWriter that cannot write. Every mutating method
// fails with ErrWritebackDisabled and logs at WARN; the two read-ish methods
// answer in the way that makes a caller do the least damage.
//
// # Why this exists when DrainAccount already returns early
//
// It is the BACKSTOP, not the control (ADR-028 §3). The control is that
// DrainAccount returns before imapsync.NewWriter, so on an ingest-only account
// no writer is constructed at all and this type is never reached.
//
// ⚠ REACHING THIS TYPE IS A BUG BY CONSTRUCTION. It means some path obtained a
// writer for an account that must not be written to — a hole in the layer
// above. That is why every method logs at WARN rather than failing quietly: a
// silent backstop would convert a structural defect into a mystery, and the
// whole point of the mode is that nothing about it is silent.
//
// It is wired wherever a writer must be produced unconditionally by a signature
// that cannot decline, and it is what an ingest-only e2e test asserts against
// when it wants to prove the refusal is observable rather than merely that
// nothing happened to be written.
type RefusingWriter struct {
	// Log receives one WARN per refused operation. A nil Log is allowed and
	// means "refuse silently", which only tests should choose.
	Log *slog.Logger
	// Account names the account in the log line, so an operator reading a WARN
	// can tell which mailbox was nearly mutated.
	Account string
}

func (w RefusingWriter) refuse(op, mailbox string) error {
	if w.Log != nil {
		w.Log.Warn("writeback refused: account is ingest-only",
			"op", op, "mailbox", mailbox, "account_id", w.Account,
			// Named so the log line says what to go and look at, rather than
			// only that something was stopped.
			"cause", "a writer was constructed for an account with writeback disabled; "+
				"DrainAccount should have returned first (ADR-028 §3)")
	}
	return fmt.Errorf("%w: %s on %q", ErrWritebackDisabled, op, mailbox)
}

// StoreFlags refuses.
func (w RefusingWriter) StoreFlags(_ context.Context, mailbox string, _ imapsync.UID, _, _ []imapsync.Flag) error {
	return w.refuse("STORE", mailbox)
}

// Copy refuses.
func (w RefusingWriter) Copy(_ context.Context, mailbox string, _ imapsync.UID, _ string) error {
	return w.refuse("COPY", mailbox)
}

// Move refuses.
func (w RefusingWriter) Move(_ context.Context, mailbox string, _ imapsync.UID, _ string) error {
	return w.refuse("MOVE", mailbox)
}

// Expunge refuses.
//
// ⚠ It returns 0 AND an error, never 0 with a nil error. Expunge's contract
// says a zero count with no error is a legitimate "there was nothing there"
// (#268), so refusing that way would tell the engine the message is already
// gone — turning a refusal into a false confirmation of destruction.
func (w RefusingWriter) Expunge(_ context.Context, mailbox string, _ imapsync.UID) (int, error) {
	return 0, w.refuse("EXPUNGE", mailbox)
}

// Append refuses.
func (w RefusingWriter) Append(_ context.Context, mailbox string, _ []imapsync.Flag, _ []byte) (imapsync.UID, error) {
	return 0, w.refuse("APPEND", mailbox)
}

// SearchMessageID refuses rather than answering "no matches".
//
// ⚠ AN EMPTY RESULT IS NOT A SAFE ANSWER HERE. The engine uses this probe to
// decide whether an APPEND would duplicate and which draft copy to expunge; an
// empty SEARCH treated as success is exactly what created a stale draft and
// then a loop in #268. A refusal is a fact; a fabricated empty answer is a lie
// the engine would act on.
func (w RefusingWriter) SearchMessageID(_ context.Context, mailbox string, _ string) ([]imapsync.UID, error) {
	return nil, w.refuse("SEARCH", mailbox)
}

// UIDPresent refuses rather than answering false (absent).
//
// ⚠ SAME REASONING AS SearchMessageID. UIDPresent exists to tell the engine's
// #611 re-observation apart "the server truly removed it" from "the server
// kept it anyway" — a fabricated false here would read as the reassuring
// half of that question and let a caller that reached this backstop by
// mistake believe a message is gone when nobody ever asked the server.
func (w RefusingWriter) UIDPresent(_ context.Context, mailbox string, _ imapsync.UID) (bool, error) {
	return false, w.refuse("SEARCH", mailbox)
}

// SupportsMove reports false.
//
// This is the one method that cannot fail, so it answers with the value that
// leads to the least destructive path if anything ignores the refusals above:
// false sends a move down the COPY+EXPUNGE fallback, whose every step is itself
// refused, rather than down the single-shot MOVE.
func (w RefusingWriter) SupportsMove() bool { return false }

// Compile-time proof that the refusal satisfies the same interface the real
// writer does — without it, a method added to IMAPWriter would leave this
// backstop silently unwired.
var _ IMAPWriter = RefusingWriter{}
