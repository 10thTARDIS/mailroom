// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"
)

// Runner drains one account's writeback queue over a real IMAP session. It is
// the piece that turns "Email/set wrote a row" into "the message is actually
// archived on the server".
//
// It dials its OWN session rather than borrowing the sync engine's, so the two
// stay independently failable — but the caller runs it in the same per-account
// sequence as the sync pass (cmd/mailroom wires it as syncrunner's pre-sync
// hook), so an account never has two Mailroom IMAP connections open at once.
// Provider concurrent-IMAP caps are a real constraint (spec §4: "stay
// conservative").
//
// Order matters: writeback runs BEFORE the account's sync pass. Local deltas go
// up first, then we reconcile what the server has — so a flag we just set is not
// immediately "corrected" back by a reconcile that has not seen it yet.
type Runner struct {
	store  *store.Store
	dial   Dialer
	blobs  BlobReader
	engine *Engine
	log    *slog.Logger
	limits DrainLimits
}

// Dialer opens an IMAP session for an account. In production it is
// imapsync.Dial; tests inject a fake session with no network.
type Dialer func(ctx context.Context, cfg imapsync.Config) (imapsync.SyncSession, error)

// NewRunner builds a writeback runner. blobs resolves an APPEND intent's bytes
// by content hash (mailroom#89); cmd/mailroom wires it to the SAME instance it
// hands the search indexer, so there is one mail-blob DEK derivation path, not
// two. blobs may be nil — an APPEND whose payload has no inline body then
// dead-letters with a diagnosable reason instead of panicking (see
// Engine.appendBody), which matters only for a legacy in-flight intent or a
// caller that mis-wires this; production always supplies one.
func NewRunner(st *store.Store, dial Dialer, blobs BlobReader, log *slog.Logger, opts ...Option) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{store: st, dial: dial, blobs: blobs, engine: NewEngine(nil, opts...), log: log,
		limits: DefaultDrainLimits}
}

// DrainAccount applies every pending writeback intent for one account.
//
// It first asks whether there is anything to do, in a cheap query, and returns
// without dialing when there is not — an idle account must not cost an IMAP
// connection every pass.
func (r *Runner) DrainAccount(ctx context.Context, userID, accountID ulid.ULID, cfg imapsync.Config) (Result, error) {
	var (
		pending   int
		writeback bool
		awaiting  bool
		gmail     bool
		folders   store.GmailFolders
	)
	if err := r.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		acct, err := tx.GetAccount(ctx, accountID)
		if errors.Is(err, store.ErrAccountNotFound) {
			// Deleted between the sync runner listing it and this drain. Nothing
			// to write to, and not an error worth failing the pass over — the
			// same outcome the old count-based early return produced.
			return nil
		}
		if err != nil {
			return err
		}
		// THE GMAIL WRITEBACK HOLD (ADR-047 §3, ADR-028), the drain-side
		// half of store.gmailWritebackHeldSQL: while
		// store.GmailWritebackReady is false a Gmail OAuth account is
		// ingest-only HERE TOO, whatever its column says, so a row that is
		// somehow pending (enqueued before the account was a Gmail one, or by
		// any path that bypassed EnqueueIntent) still never dials. It joins
		// the ingest-only branch below and gets that branch's self-heal.
		gmailHeld := r.engine.gmailHold
		if gmailHeld == nil {
			gmailHeld = store.GmailWritebackHeld
		}
		writeback = acct.WritebackEnabled && !r.store.WritebackDisabled() &&
			!gmailHeld(acct.AuthKind, acct.ProviderProfile)
		awaiting = acct.AwaitingReconnect()
		// GMAIL MODE is decided from the account row in this same
		// transaction: a Gmail OAuth account's intents are applied with
		// Gmail's primitives (writeback/gmail.go), never the generic folder
		// ops, which would EXPUNGE in label folders.
		if gmail = store.IsGmailOAuth(acct.AuthKind, acct.ProviderProfile); gmail {
			if folders, err = tx.GmailWritebackFolders(ctx, accountID); err != nil {
				return err
			}
		}
		// ⚠ DO NOT SHORT-CIRCUIT HERE ON !writeback, however tempting it is to
		// save the count for an account that cannot write. Returning early would
		// leave `pending` at 0, so the `pending == 0` return below would do the
		// work and the named guard would become dead code — the guarantee would
		// rest on a coincidence instead of on the check that documents it.
		//
		// That is not hypothetical: it is what this function did on first
		// writing, and the probe caught it. Deleting the guard left
		// TestIngestOnlyNeverDials passing, because the mode was being enforced
		// by an accident of variable initialisation.
		pending, err = tx.PendingIntentCount(ctx, accountID)
		return err
	}); err != nil {
		return Result{}, fmt.Errorf("writeback: count pending: %w", err)
	}

	// INGEST-ONLY MODE — THIS IS THE CONTROL (ADR-028 §3).
	//
	// Returning here is not "we checked a flag before writing". This function
	// holds the ONLY production construction of an IMAPWriter in the tree
	// (imapsync.NewWriter, below), so returning before it means no connection is
	// opened and the object capable of mutating the mailbox is never created.
	// That is what makes the mode structurally impossible to violate rather than
	// merely unused.
	//
	// ⚠ The flag is read inside the transaction above rather than taken from an
	// account struct the caller passed in, so a caller holding a stale copy from
	// earlier in the pass cannot widen the gate.
	//
	// ⚠ IT MUST COME BEFORE THE pending==0 RETURN AND MUST NOT BE FOLDED INTO
	// IT. Intents enqueued while the mode is on are 'suppressed' and so are not
	// 'pending' either, which makes the two look interchangeable — they are
	// not. Anything queued BEFORE the mode was switched on is still pending,
	// and this is the only thing that stops it draining. The probe that proves
	// this line is load-bearing is in the test file: delete it and
	// TestIngestOnlyNeverDials goes red.
	if !writeback {
		// SELF-HEAL: a row still 'pending' here becomes a ledger row NOW.
		//
		// ⚠ THIS IS WHAT MAKES §4's PROMISE A REPEATED INVARIANT RATHER THAN A
		// PROPERTY OF ONE TRANSACTIONAL EDGE. Account/set suppresses the backlog
		// in the same transaction as the flag change, which is correct but is a
		// single instant — and a row can reach 'pending' on an account that is
		// ALREADY shadowed without ever passing through it:
		//
		//   - THE SWITCH-ON RACE, MEASURED. EnqueueIntent computes its state from
		//     a subselect on app.accounts at its own statement snapshot (still
		//     true -> 'pending'), and only then does its FK check take KEY SHARE
		//     on the account row — which conflicts with UpdateAccount's FOR
		//     UPDATE, so the INSERT blocks. SuppressAccountIntents, running in
		//     that same transaction, cannot see the uncommitted row and reports 0.
		//     The switch commits, the INSERT unblocks, and the row lands 'pending'
		//     on a shadowed account. A probe with a pg_stat_activity witness
		//     reproduced the interleaving and then the row APPLIED on the first
		//     drain after the mode was lifted — with no --apply, and invisible to
		//     writeback-ledger, which counts only 'suppressed'.
		//   - BOOTING WITH THE INSTANCE FLAG while rows are already pending.
		//     Nothing calls Account/set at all, so nothing suppresses them and
		//     they fire the moment the flag comes off.
		//
		// One guard closes both, with no second predicate to disagree with the
		// first: this function already holds the effective flag and the count,
		// and it runs every pass. The mode's own control is untouched — we still
		// return before r.dial, so this adds a store write and never an IMAP
		// capability.
		//
		// ⚠ IT IS NOT A CLAIM THAT NO PENDING ROW EVER EXISTS ON A SHADOWED
		// ACCOUNT. Between the race and the next pass the row sits pending, so
		// the residue is "the mode was switched on and off again inside one sync
		// interval" — a much smaller window than the whole shadow run, but not
		// zero. Closing it at source needs a locking read in EnqueueIntent's
		// subselect; see #554.
		//
		// A failure is logged, not returned: nothing reached the mailbox either
		// way, so the safety property does not depend on this write, and the next
		// pass retries. Failing the drain would turn a bookkeeping hiccup into a
		// reported sync error for an account that is behaving exactly as asked.
		if pending > 0 {
			var moved int64
			if err := r.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				var e error
				moved, e = tx.SuppressAccountIntents(ctx, accountID)
				return e
			}); err != nil {
				r.log.Warn("writeback: could not suppress intents pending on an ingest-only account; "+
					"they stay pending and the next pass retries",
					"user_id", userID.String(), "account_id", accountID.String(),
					"pending", pending, "err", err)
			} else if moved > 0 {
				r.log.Info("writeback: moved intents pending on an ingest-only account into the ledger; "+
					"they now require an explicit writeback-ledger --apply (ADR-028 §4)",
					"user_id", userID.String(), "account_id", accountID.String(), "suppressed", moved)
			}
		}
		return Result{}, nil
	}
	// RECONNECT HOLD (#52, ADR-046 §5/§11; decided 2026-10-27). The
	// OAuth account needs its user to sign in again — its grant is
	// needs_reauth, or it was disconnected — so a dial could only fail. The
	// intents are HELD — left exactly as they are, 'pending' with their
	// attempt counts untouched, never dead-lettered and never charged against
	// a retry budget — and the first drain after the reconnect applies them.
	// Read inside the transaction above, like the writeback flag, so a stale
	// account struct cannot widen it. The sync lists already leave such an
	// account out (store.ListSyncableAccounts), so the runner never calls this
	// for it; this is the same rule where the connection would be opened.
	if awaiting {
		if pending > 0 {
			r.log.Info("writeback: intents held until the account is reconnected",
				"user_id", userID.String(), "account_id", accountID.String(), "pending", pending)
		}
		return Result{}, nil
	}
	if pending == 0 {
		return Result{}, nil
	}

	session, err := r.dial(ctx, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("writeback: dial: %w", err)
	}
	defer func() { _ = session.Close() }()

	engineOpts := []Option{WithBlobReader(r.blobs, userID)}
	if gmail {
		engineOpts = append(engineOpts, WithGmail(folders))
	}
	engine := NewEngine(NewStoreIntents(r.store, userID), engineOpts...)
	engine.batchSize = r.engine.batchSize
	engine.maxAttempts = r.engine.maxAttempts

	// THE BACKSTOP (ADR-028 §3). Today this is unreachable: the guard above
	// returned for every account with writeback off, so `writeback` is true
	// here by construction.
	//
	// ⚠ DO NOT DELETE IT AS DEAD CODE — being unreachable is the entire point.
	// This line is the reason that REMOVING the guard above degrades the mode
	// to "dials but cannot write" instead of to "writes to the user's real
	// mailbox". A backstop that only runs once the thing in front of it is
	// broken looks redundant right up until it is the only thing left.
	//
	// Honest limit on that claim: the probe run for this change deleted the
	// guard and confirmed the never-dials test goes red AT THE DIAL — so what
	// is demonstrated is that the guard is load-bearing, not that this branch
	// then refuses every write. The refusal itself rests on RefusingWriter's
	// own unit coverage plus the compile-time IMAPWriter assertion in
	// refusing_writer.go. Do not upgrade this comment to "proven" without a
	// test that removes the guard and asserts on the writer.
	var w IMAPWriter = imapsync.NewWriter(session)
	if !writeback {
		w = RefusingWriter{Log: r.log, Account: accountID.String()}
	}

	// ONE CONNECTION, SEVERAL PASSES (#706): Drain keeps going while passes come
	// back full, within r.limits, instead of leaving the next 64 for the next
	// sync pass and a fresh login. One log line per pass, now with how long the
	// pass took, so the split between IMAP work and waiting is measurable.
	res, err := engine.Drain(ctx, accountID, w, r.limits, func(round int, res Result, took time.Duration) {
		r.logPass(userID, accountID, pending, round, took, res)
	})
	r.recordKeywordProbes(ctx, userID, accountID, session)
	return res, err
}

// keywordProber is the session capability recordKeywordProbes reads:
// imapsync's real session has it (session.WritableKeywordProbes). A session
// without it (a test fake) simply records nothing, and the live harness's
// assertion that the column BECOMES true is what would notice the wiring
// breaking (#740).
type keywordProber interface {
	WritableKeywordProbes() map[string]bool
}

// recordKeywordProbes stores what this drain's read-write SELECTs said about
// new keywords (#740). It costs no IMAP: the SELECTs already happened, for the
// writes. Recorded even when the drain failed part-way — what a SELECT
// observed is true regardless of what came after it. A store failure is logged
// rather than returned: the drain's own outcome is the thing a caller acts on,
// and an unrecorded probe is re-observed on the next write to that mailbox.
func (r *Runner) recordKeywordProbes(ctx context.Context, userID, accountID ulid.ULID, session imapsync.SyncSession) {
	p, ok := session.(keywordProber)
	if !ok {
		return
	}
	probes := p.WritableKeywordProbes()
	if len(probes) == 0 {
		return
	}
	var changed int
	if err := r.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		changed, e = tx.RecordKeywordProbes(ctx, accountID, probes)
		return e
	}); err != nil {
		r.log.Warn("writeback: could not record keyword probes; the next write re-observes them",
			"user_id", userID.String(), "account_id", accountID.String(), "err", err)
		return
	}
	if changed > 0 {
		r.log.Info("writeback: recorded keyword probes",
			"user_id", userID.String(), "account_id", accountID.String(),
			"observed", len(probes), "changed", changed)
	}
}

// logPass writes one "writeback pass" line. pending is the count read before
// the drain began, so on round 2 and later it is the drain's starting backlog,
// not what is left.
func (r *Runner) logPass(userID, accountID ulid.ULID, pending, round int, took time.Duration, res Result) {
	r.log.Info("writeback pass",
		"user_id", userID.String(), "account_id", accountID.String(),
		"round", round, "took_ms", took.Milliseconds(), "claimed", res.Claimed,
		"pending", pending, "applied", res.Applied, "retried", res.Retried,
		"dead_lettered", res.DeadLettered, "conflicts", res.Conflicts,
		"deduped", res.Deduped,
		// Gmail accounts only (#52): copies found at a new UID by
		// X-GM-MSGID, and labels CREATEd because an add did not stick.
		"reresolved", res.Reresolved, "labels_created", res.LabelsCreated,
		// Logged separately from applied on purpose (#268): a pass that applied
		// an expunge and destroyed nothing is the failure this counter exists to
		// make visible in the operator's log.
		"expunged", res.Expunged,
		// Non-zero means the apply-time guard fired: a queued purge EXPUNGE was
		// dropped because the message had left Trash since it was enqueued, so
		// mail the user asked to keep was NOT destroyed (#555). It is the only
		// place that event is observable — a withdrawn intent leaves no row —
		// and an operator asking "does this actually happen on real mail?" has
		// nothing else to read.
		"withdrawn", res.Withdrawn,
		// Intents whose row was gone by the time the pass recorded them —
		// counted here, never as applied or dead-lettered (#691).
		"vanished", res.Vanished)
}
