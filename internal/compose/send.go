// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/blob"
	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/xoauth2"
)

// This file is the send half of the delayed-submission engine (spec §5/§16):
// the durable-job handler that actually hands a scheduled submission to SMTP.
//
// It is a JOB, not a request path, because a send must survive a restart: the
// submission row and its job are written in the same transaction as the
// EmailSubmission change_log row (Composer.Submit), so a crash anywhere after
// the client got its response still leaves a timer that fires on boot
// (jobs.Dispatcher runs a catch-up pass immediately). Nothing is sent twice: the
// scheduled -> sending transition is a guarded UPDATE, so a re-run of the same
// job after a partial failure finds the submission already out of 'scheduled'
// and does nothing.
//
// The Sent copy is queued as a writeback APPEND rather than written here, so a
// provider that is up for SMTP but down for IMAP still sends (and files the copy
// when IMAP comes back), and so the SELF-APPEND DEDUP (ADR-001, spec §4) happens
// at the one place that can actually see the target mailbox: the writeback engine.

// errIngestOnlyCannotSend is the permanent failure recorded on a submission
// dispatched for an account in ingest-only mode (ADR-028 §6).
//
// The wording is the user's, not the schema's: it ends up in the submission's
// stored error and therefore in front of whoever tries to send.
var errIngestOnlyCannotSend = errors.New(
	"this account is in ingest-only mode: Mailroom is observing the mailbox and will not send from it")

// AwaitingReconnectReason is what a held submission's stored response says
// while its account needs the user to sign in again (#52, ADR-046 §11): its
// grant is needs_reauth, or it was disconnected. The submission is still
// 'scheduled', not failed. store.HeldProviderReason is the other hold: the
// provider refusing this server's client (operator_error).
const AwaitingReconnectReason = store.HeldReconnectReason

// sendBlockedCause is the permanent failure recorded on a submission for a
// RECEIVE-ONLY account (store.SendBlocked*, ADR-046 §12).
//
// FAILED, NOT HELD — a deliberate difference from the reconnect and
// operator holds above. A hold is right when the fix is near and certain: the
// user clicks Reconnect, or the operator rotates a secret, and the message
// goes out minutes later as if nothing happened. 5.7.139 is an organisation's
// security policy (Security Defaults turns it on): the fix is an
// administrator's decision that may never come, and nothing Mailroom can do
// brings it closer. Holding would show "Sending…" for days over mail the
// person needed to send today, and each recheck would be another refused AUTH
// logged against the mailbox. A failure with this text is the honest answer:
// it says why, and the person can send from Outlook itself. Nothing is lost —
// the message stays in Mailroom as the failed submission's Email.
func sendBlockedCause(code string) error {
	return errors.New(store.SendBlockedReason(code))
}

// reconnectHoldInterval is how often a held submission looks again. After the
// user reconnects (or the operator fixes the server's sign-in), the send goes
// out within one interval.
const reconnectHoldInterval = 5 * time.Minute

// tokenProber is an optional Keys capability: obtain a current access token
// for an OAuth account without sending anything. The hold for operator_error
// uses it so a held submission is also what notices the operator's fix — an
// account with sync switched off (a held Gmail account) has no sync pass to
// refresh its grant for it. cmd/mailroom's keyResolver implements it.
type tokenProber interface {
	ProbeToken(ctx context.Context, userID ulid.ULID, acct store.Account) error
}

// holdTestHook, when set (tests only), runs between the hold's unlocked read
// and its guarded write — the window a concurrent Undo Send can land in.
var holdTestHook func()

// SendWorker executes `submit` jobs.
type SendWorker struct {
	store  *store.Store
	blobs  blob.BlobStore
	keys   Keys
	sender provider.Sender
	log    *slog.Logger
	onSent func(userID ulid.ULID)
}

// SendWorkerOption configures a SendWorker.
type SendWorkerOption func(*SendWorker)

// WithOnSent registers a callback run once for every submission this worker
// actually sends, AFTER the transaction that records the send and queues the
// Sent APPEND and the Drafts EXPUNGE has committed. It is never called for a
// cancelled, refused, requeued or exhausted send — none of those queues
// anything.
//
// ⚠ IT EXISTS BECAUSE NOTHING ELSE CARRIES THOSE INTENTS UP (#691). The only
// writeback nudge was Email/set's (#581), and a send completes in a background
// job seconds after the last Email/set, so the post-send intents waited for
// whatever next entered the account's sync gate — 2.8 minutes in one production
// send — with the sent message's Drafts copy still on the server all along.
// cmd/mailroom wires this to syncrunner.Runner.NudgeWriteback. A nudge that
// arrives during a running pass still waits for it (syncrunner/gate.go); this
// removes the wait for a trigger, not the wait behind a long pass.
//
// f must return promptly (NudgeWriteback does — it drains on its own
// goroutines): it runs on the dispatcher's goroutine, which sends one job at a
// time.
func WithOnSent(f func(userID ulid.ULID)) SendWorkerOption {
	return func(w *SendWorker) { w.onSent = f }
}

// NewSendWorker builds the job handler.
func NewSendWorker(st *store.Store, blobs blob.BlobStore, keys Keys, sender provider.Sender, log *slog.Logger, opts ...SendWorkerOption) *SendWorker {
	if log == nil {
		log = slog.Default()
	}
	w := &SendWorker{store: st, blobs: blobs, keys: keys, sender: sender, log: log}
	for _, o := range opts {
		o(w)
	}
	return w
}

// submitPayload is the job payload Composer.Submit writes.
type submitPayload struct {
	SubmissionID string `json:"submission_id"`
}

// Handle runs one `submit` job. Register it with jobs.Dispatcher under
// SubmitJobKind.
//
// A returned error means "retry me" (the dispatcher backs off and eventually
// dead-letters, which is surfaced). A nil return means the job is finished —
// which includes the cases where the submission was canceled inside the undo
// window, where the provider PERMANENTLY refused it, and where THIS was the
// job's last allowed attempt (see Send's attempts/maxAttempts): all three are
// terminal outcomes recorded on the submission, and retrying any of them
// would be wrong. job.Attempts/job.MaxAttempts are threaded into Send so the
// last case can be recognized — see the doc there (#624 §4).
func (w *SendWorker) Handle(ctx context.Context, job store.Job) error {
	if job.UserID == nil {
		return fmt.Errorf("compose: submit job %s has no user", job.ID)
	}
	userID := *job.UserID

	var p submitPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("compose: submit job %s: bad payload: %w", job.ID, err)
	}
	subID, err := ulid.Parse(p.SubmissionID)
	if err != nil {
		return fmt.Errorf("compose: submit job %s: bad submission id: %w", job.ID, err)
	}
	return w.Send(ctx, userID, subID, job.Attempts, job.MaxAttempts)
}

// Send performs one submission end to end. attempts and maxAttempts are the
// job's own bookkeeping (store.Job.Attempts / .MaxAttempts) — Handle passes
// them through unchanged — and exist for exactly one decision: whether a
// transient provider failure here is the job's LAST allowed attempt. If it
// is, the dispatcher is about to dead-letter the job (jobs.Dispatcher.fail,
// mirroring db/schema.sql's app.fail_job: `attempts >= max_attempts`), and a
// dead job is never revisited — so requeuing the submission as usual would
// leave it 'scheduled' forever with nothing left to ever move it, which reads
// to a client as "Sending…" indefinitely (#624 §4). maxAttempts<=0 disables
// the check entirely (always requeue on a transient failure, the pre-#624
// behavior) — for a caller with no job attempt budget to compare against;
// there are none today, Handle is the only caller and always has one.
func (w *SendWorker) Send(ctx context.Context, userID, subID ulid.ULID, attempts, maxAttempts int) error {
	log := w.log.With("submission_id", subID.String(), "user_id", userID.String())

	// --- 0. Held? ------------------------------------------------------------
	// Before the claim, so a submission waiting on its account (a reconnect,
	// or the operator's fix) is never moved to 'sending' and back every few
	// minutes (each move is an EmailSubmission change row every client would
	// refetch).
	if held, err := w.holdIfAccountCannotSend(ctx, userID, subID, log); err != nil || held {
		return err
	}

	// --- 1. Claim ------------------------------------------------------------
	// One guarded UPDATE decides the Undo-Send race: if the user canceled, the
	// claim finds nothing to claim and the send simply does not happen.
	var (
		sub     store.Submission
		claimed bool
		acct    store.Account
		raw     []byte
		blocked string
	)
	err := w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		sub, claimed, err = tx.ClaimSubmissionForSend(ctx, subID)
		if err != nil || !claimed {
			return err
		}
		if acct, err = tx.GetAccount(ctx, sub.AccountID); err != nil {
			return err
		}
		if blocked, err = tx.SendBlocked(ctx, sub.AccountID); err != nil {
			return err
		}
		// The daily cap is drawn down INSIDE the claim transaction, so two
		// concurrent sends cannot both take the last unit of the budget
		// (spec §5: provider caps are counted per account, per day).
		if _, err := tx.ReserveOutbound(ctx, sub.AccountID); err != nil {
			return err
		}
		ref, err := tx.MessageRef(ctx, sub.MessageID)
		if err != nil {
			return err
		}
		raw, err = readRaw(ctx, w.blobs, w.keys, userID, ref.ContentHash)
		return err
	})
	if errors.Is(err, store.ErrOverQuota) {
		return w.deferForQuota(ctx, userID, subID, log)
	}
	if err != nil {
		return err // transient: the dispatcher retries
	}
	if !claimed {
		log.Info("submission is no longer scheduled; nothing to send (canceled, or already sent)")
		return nil
	}

	// INGEST-ONLY MODE: this account may not send (ADR-028 §6).
	//
	// ⚠ THIS IS THE STRUCTURAL GATE FOR SUBMISSION, and it is the SMTP analogue
	// of the IMAPWriter one. EmailSubmission/set already refuses a create on a
	// shadowed account, but that is the policy layer: it cannot speak for a
	// submission that was created BEFORE the mode was switched on and is only
	// now being dispatched. This is the one place in the tree where bytes are
	// handed to a provider.Sender, so it is the only place the guarantee can
	// actually be made.
	//
	// It fails the submission PERMANENTLY rather than requeueing it. A shadowed
	// account does not become sendable by waiting, so a retry loop would just
	// re-attempt forever and bury the reason; failSubmission gives back the
	// reserved quota and records a cause the user can read.
	if !acct.WritebackEnabled || w.store.WritebackDisabled() {
		return w.failSubmission(ctx, userID, sub, errIngestOnlyCannotSend, log)
	}

	// RECEIVE-ONLY (ADR-046 §12): the provider has already refused this
	// account as a policy (5.7.139). A submission queued before that was known
	// FAILS with the readable reason, without dialling — it is not held like a
	// reconnect (see sendBlockedCause for why).
	if blocked != "" {
		return w.failSubmission(ctx, userID, sub, sendBlockedCause(blocked), log)
	}

	// --- 2. Submit -----------------------------------------------------------
	login, err := w.keys.SMTPAuth(ctx, userID, acct)
	if err != nil {
		return w.failSubmission(ctx, userID, sub, fmt.Errorf("smtp credentials: %w", err), log)
	}
	res, err := w.sender.Send(ctx, provider.SendRequest{
		Host: login.Host, Port: login.Port,
		Auth:        login.Auth,
		ImplicitTLS: login.ImplicitTLS,
		MailFrom:    sub.FromAddress,
		RcptTo:      sub.EnvelopeTo,
		// Bcc never goes on the wire (build.go).
		Data: SendPayload(raw),
	})
	if err != nil {
		if errors.Is(err, xoauth2.ErrReauthRequired) {
			// The grant died between the check above and this send (the
			// token refresh just heard invalid_grant, or the account has no
			// grant). Nothing was sent: held, not failed (decided
			// 2026-10-27), and checked before ErrPermanent, which a token
			// failure also carries.
			return w.holdClaimed(ctx, userID, sub, AwaitingReconnectReason, log)
		}
		if errors.Is(err, xoauth2.ErrProviderMisconfigured) {
			// The provider refused THIS SERVER's client (an expired or rotated
			// secret): the operator's to fix, never the user's mail to lose.
			// Held with the provider reason, exactly like a reconnect.
			return w.holdClaimed(ctx, userID, sub, store.HeldProviderReason(acct.ProviderProfile), log)
		}
		if errors.Is(err, provider.ErrSMTPAuthDisabled) {
			// 5.7.139: the organisation turned SMTP AUTH off. Mark the account
			// receive-only — later sends are refused up front, queued ones fail
			// without dialling — and fail THIS submission with the readable
			// reason. Checked before ErrPermanent, which it also carries.
			if merr := w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
				return tx.MarkSendBlocked(ctx, sub.AccountID, store.SendBlockedSMTPAuthDisabled, time.Now())
			}); merr != nil {
				log.Error("could not mark the account receive-only", "err", merr)
			}
			log.Warn("provider refuses SMTP AUTH for this account (5.7.139); it is receive-only now",
				"account_id", sub.AccountID.String())
			return w.failSubmission(ctx, userID, sub, sendBlockedCause(store.SendBlockedSMTPAuthDisabled), log)
		}
		if errors.Is(err, provider.ErrPermanent) || errors.Is(err, provider.ErrEAIUnsupported) ||
			errors.Is(err, provider.ErrHostNotPermitted) {
			// The provider will never accept this. Record it and stop; a retry
			// would just repeat the rejection five more times and then surface
			// it an hour later, when the user has moved on.
			//
			// ErrHostNotPermitted belongs here too (#625 §3): the SMTP host now
			// resolves to an address the egress guard refuses (a private or
			// loopback address — DNS rebinding, or a host that was never
			// public). Nothing was dialled, so nothing was sent; waiting does not
			// make the address permitted, and the cause names the host and the
			// switch that would, so the user sees why rather than "Sending…".
			return w.failSubmission(ctx, userID, sub, err, log)
		}
		// ⚠ #624 §4: if this is the job's LAST allowed attempt, requeuing would
		// set the submission 'scheduled' one final time — and then the dispatcher
		// dead-letters the job (jobs.Dispatcher.fail) and NOTHING ever revisits
		// it again. Without this branch that is a permanently stuck "Sending…"
		// in the client's Outbox, indistinguishable from a send in progress.
		if maxAttempts > 0 && attempts >= maxAttempts {
			return w.failExhausted(ctx, userID, sub, attempts, err, log)
		}
		// Transient, with retries remaining: put the submission back in the
		// queue and let the job retry.
		if rerr := w.requeue(ctx, userID, sub, err); rerr != nil {
			return rerr
		}
		return err
	}

	// --- 3. Record the send + file the Sent copy -----------------------------
	if err := w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if err := tx.FinishSubmission(ctx, sub.ID, store.SubmissionSent, res.Response); err != nil {
			return err
		}
		if err := w.fileSentCopy(ctx, tx, sub, raw, log); err != nil {
			return err
		}
		// SEND & ARCHIVE (#648): HERE, and only here — the one place a
		// submission becomes 'sent'. A failed, cancelled or exhausted send
		// never reaches this line, so it archives nothing. Same transaction as
		// the 'sent' record (they commit together; no crash window leaves a
		// sent reply whose conversation never archives), but under its own
		// savepoints inside ArchiveAfterSend, so a failed archive is logged
		// and can never roll back the record of a send that already happened.
		w.archiveAfterSend(ctx, tx, sub, log)
		return nil
	}); err != nil {
		// The mail IS sent. Failing the job now would re-send it on retry, so
		// this failure is logged and swallowed: the submission's own state is
		// the record of the send, and the Sent copy will be reconciled by the
		// next sync pass (which will ingest the provider's copy if it made one).
		log.Error("send succeeded but recording it failed", "err", err)
	} else if w.onSent != nil {
		// Committed: the Sent APPEND and the Drafts EXPUNGE are visible to a
		// drain now, so carry them up now rather than at the next scheduled
		// pass (#691). Not on the error branch: that transaction rolled back
		// and queued nothing.
		w.onSent(userID)
	}
	log.Info("submission sent", "recipients", len(sub.EnvelopeTo))
	return nil
}

// fileSentCopy queues the Sent APPEND and flips the draft into the sent state.
//
// SELF-APPEND DEDUP (ADR-001 / spec §4: "most providers do not save a copy on
// SMTP submission, so Mailroom APPENDs to Sent per account — but the appender
// first checks (by content hash) whether the provider self-appended (Gmail
// does), so relayed sends never appear twice"). The check runs in two places, on
// purpose:
//
//	here      — a byte-identical copy the provider filed and sync has ALREADY
//	            ingested is visible in the canonical store, so we skip the
//	            append outright and never touch IMAP.
//	the engine — a copy the provider filed but sync has NOT seen yet is invisible
//	            here (a send is usually only seconds old), so writeback searches
//	            the Sent mailbox for the message's Message-ID immediately before
//	            appending, and skips if it is already there. The Message-ID is
//	            ours, generated under the sending identity's domain, so it is a
//	            reliable handle on OUR copy — and it catches the case a content
//	            hash cannot: a provider whose filed copy differs by a header it
//	            added on the way out.
func (w *SendWorker) fileSentCopy(ctx context.Context, tx *store.UserTx, sub store.Submission, raw []byte, log *slog.Logger) error {
	sent, found, err := tx.MailboxForRole(ctx, sub.AccountID, "sent")
	if err != nil {
		return err
	}
	if !found {
		// Surfaced, not silently swallowed: the mail went out, but we have
		// nowhere to file it, and the operator needs to know their account has
		// no \Sent mailbox mapped.
		log.Warn("account has no Sent mailbox; the sent copy was not filed",
			"account_id", sub.AccountID.String())
		return nil
	}

	ref, err := tx.MessageRef(ctx, sub.MessageID)
	if err != nil {
		return err
	}
	already, err := tx.ContentHashInMailbox(ctx, ref.ContentHash, sent.ID)
	if err != nil {
		return err
	}
	if err := tx.MarkMessageSent(ctx, sub.MessageID, sent.ID); err != nil {
		return err
	}

	// The draft copy on the server is now stale — the draft has been consumed by
	// sending — so it is expunged from Drafts (mailroom#146). Without this the
	// message stays in the Drafts folder AND is re-tagged \Draft by the next
	// sync's flag reconcile, undoing the MarkMessageSent above.
	//
	// BEFORE the Sent APPEND below, deliberately: this cleanup cancels a Drafts
	// APPEND that has not run yet, and doing it first means there is not yet a
	// Sent APPEND in the queue for that cancellation to go anywhere near. (It is
	// also scoped to the Drafts mailbox, so the ordering is belt and braces.)
	//
	// The ACCOUNT is passed because the Drafts mailbox is resolved by role, not
	// from provenance: at this instant the draft's own APPEND has usually not run
	// yet, so there is no copy to have observed (mailroom#150).
	// The dropped-provenance count is deliberately ignored HERE and nowhere else:
	// MarkMessageSent above already appended this Email's change_log row in this
	// same transaction, so the membership change that drop represents is covered.
	// See RetireSentDraftCopy's doc and store.RepairSentDraft (#311).
	if n, _, err := tx.RetireSentDraftCopy(ctx, sub.MessageID, sub.AccountID); err != nil {
		return err
	} else if n > 0 {
		log.Info("expunging the draft copy now that the message is sent", "copies", n)
	}

	if already {
		log.Info("provider already filed this message in Sent; skipping the append",
			"mailbox", sent.IMAPName)
		return nil
	}
	// The APPENDed copy is the STORED one (Bcc header intact), not the
	// transmitted one — see build.go.
	return tx.EnqueueAppendIntent(ctx, sub.MessageID, sub.AccountID, sent.IMAPName,
		[]string{`\Seen`}, raw, ref.ContentHash, ref.MessageIDHint)
}

// archiveAfterSend performs a sent submission's Send & archive request (#648)
// and logs each message's outcome. It never returns an error: the send has
// happened, and the archive is a courtesy that must not undo the record of it
// (store.ArchiveAfterSend rolls a failure back to its own savepoint).
//
// sub.ArchiveOnSuccess was read at claim time and only short-circuits the
// common no-archive send; store.ArchiveAfterSend re-reads the request and the
// state from the row itself.
func (w *SendWorker) archiveAfterSend(ctx context.Context, tx *store.UserTx, sub store.Submission, log *slog.Logger) {
	if len(sub.ArchiveOnSuccess) == 0 {
		return
	}
	outcomes, err := tx.ArchiveAfterSend(ctx, sub.ID)
	if err != nil {
		log.Error("send & archive: the message was sent but its conversation was not archived", "err", err)
		return
	}
	for _, o := range outcomes {
		switch o.Result {
		case store.SendArchived:
			log.Info("send & archive: archived", "message_id", o.MessageID.String())
		case store.SendArchiveFailed:
			log.Error("send & archive: archiving failed; the send itself is recorded",
				"message_id", o.MessageID.String(), "err", o.Err)
		default:
			// Skipped, not an error: gone, no longer in the Inbox, or no Archive
			// folder. Logged so "why is it still in my Inbox" has an answer.
			log.Info("send & archive: skipped", "message_id", o.MessageID.String(),
				"reason", string(o.Result))
		}
	}
}

// holdIfAccountCannotSend holds a still-scheduled submission whose OAuth
// account cannot send right now (#52, ADR-046 §11):
//
//   - the user must sign in again (needs_reauth, or disconnected: no grant) —
//     held with AwaitingReconnectReason;
//   - the provider refuses this server's client (operator_error) and a fresh
//     token still cannot be had — held with store.HeldProviderReason. The probe
//     is what notices the operator's fix even for an account nothing syncs.
//
// Held means: it stays 'scheduled', a fresh job looks again in
// reconnectHoldInterval (none of this job's retry budget is spent), and its
// stored response says why — written once, when the hold starts, so a long
// wait does not append a change row every interval.
//
// ⚠ THE WRITE IS GUARDED (store.MarkSubmissionHeld: FOR UPDATE, state =
// 'scheduled'). The read above it is not locked, and a user's Undo Send can
// commit in between; an unguarded write would put the cancelled submission
// back to 'scheduled' and send it after the reconnect. held is false — and
// nothing is requeued — when the submission is no longer scheduled.
func (w *SendWorker) holdIfAccountCannotSend(ctx context.Context, userID, subID ulid.ULID, log *slog.Logger) (held bool, err error) {
	var (
		sub    store.Submission
		acct   store.Account
		reason string
	)
	if err := w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		reason = ""
		subs, err := tx.GetSubmissions(ctx, []ulid.ULID{subID})
		if err != nil || len(subs) == 0 || subs[0].State != store.SubmissionScheduled {
			return err
		}
		sub = subs[0]
		acct, err = tx.GetAccount(ctx, sub.AccountID)
		if err != nil {
			if errors.Is(err, store.ErrAccountNotFound) {
				return nil
			}
			return err
		}
		switch {
		case acct.AwaitingReconnect():
			reason = AwaitingReconnectReason
		case acct.ProviderUnavailable():
			reason = store.HeldProviderReason(acct.ProviderProfile)
		}
		return nil
	}); err != nil || reason == "" {
		return false, err
	}
	if acct.ProviderUnavailable() {
		if p, ok := w.keys.(tokenProber); ok {
			if perr := p.ProbeToken(ctx, userID, acct); perr == nil {
				// The operator fixed it: a token was issued (and the grant is
				// active again). Send now.
				return false, nil
			} else if errors.Is(perr, xoauth2.ErrReauthRequired) {
				reason = AwaitingReconnectReason
			}
		}
	}
	if holdTestHook != nil {
		holdTestHook()
	}
	err = w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if held, e = tx.MarkSubmissionHeld(ctx, sub.ID, reason); e != nil || !held {
			return e
		}
		return w.requeueLater(ctx, tx, sub.ID)
	})
	if err != nil {
		return false, err
	}
	if held {
		log.Info("submission held: its account cannot send right now",
			"account_id", acct.ID.String(), "reason", reason, "retry_in", reconnectHoldInterval.String())
	}
	return held, nil
}

// holdClaimed holds a submission this worker already claimed when the send
// found the account unable to send (the token refresh heard invalid_grant,
// the account has no grant, or the provider refused this server's client):
// the reserved quota is given back, it returns to 'scheduled' with reason —
// guarded on the 'sending' state it claimed — and a fresh job looks again.
func (w *SendWorker) holdClaimed(ctx context.Context, userID ulid.ULID, sub store.Submission, reason string, log *slog.Logger) error {
	log.Info("submission held: its account cannot send right now",
		"account_id", sub.AccountID.String(), "reason", reason, "retry_in", reconnectHoldInterval.String())
	return w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if err := tx.ReleaseOutbound(ctx, sub.AccountID); err != nil {
			return err
		}
		held, err := tx.HoldClaimedSubmission(ctx, sub.ID, reason)
		if err != nil || !held {
			return err
		}
		return w.requeueLater(ctx, tx, sub.ID)
	})
}

// requeueLater queues a fresh submit job for subID reconnectHoldInterval from
// now and links it, so Undo Send still cancels the job that will actually run.
func (w *SendWorker) requeueLater(ctx context.Context, tx *store.UserTx, subID ulid.ULID) error {
	at := time.Now().Add(reconnectHoldInterval)
	key := fmt.Sprintf("submit:%s:reconnect:%d", subID, at.UnixNano())
	jobID, err := tx.EnqueueJob(ctx, SubmitJobKind, at,
		[]byte(`{"submission_id":"`+subID.String()+`"}`), key)
	if err != nil {
		return err
	}
	return tx.SetSubmissionJob(ctx, subID, jobID)
}

// failSubmission records a permanent failure, returns the reserved quota, and
// completes the job (a permanent failure is not retried).
func (w *SendWorker) failSubmission(ctx context.Context, userID ulid.ULID, sub store.Submission, cause error, log *slog.Logger) error {
	log.Error("submission failed permanently", "err", cause)
	return w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if err := tx.ReleaseOutbound(ctx, sub.AccountID); err != nil {
			return err
		}
		return tx.FinishSubmission(ctx, sub.ID, store.SubmissionFailed, cause.Error())
	})
}

// failExhausted marks a submission failed because its job's retries ran out
// (#624 §4) — send.go's Send calls this instead of requeue when attempts has
// reached maxAttempts, because a requeue on the LAST attempt would leave the
// submission 'scheduled' with a job about to be dead-lettered and nothing
// left to ever revisit it. Reuses FinishSubmission, exactly like failSubmission
// and requeue, so the load-bearing change_log invariant (CLAUDE.md) is the
// store layer's job here too, not hand-rolled.
//
// It shares failSubmission's shape (ReleaseOutbound then FinishSubmission)
// but NOT its return: on success it returns cause, the original send error —
// not nil — so Handle still reports failure up to jobs.Dispatcher, which is
// what makes the dispatcher's own dead-letter bookkeeping (the DeadLettered
// metric, its log line) fire exactly as it would have without this fix. Only
// a failure to even RECORD the exhaustion returns something else, so that
// failure is what gets logged instead.
//
// ⚠ NOT the same database transaction as the dispatcher's job -> 'dead'
// transition: that runs as a SECURITY DEFINER with no user identity
// (SystemTx, app.fail_job) while this is a normal per-user UserTx, and the
// two roles cannot share one transaction. This runs first, deterministically,
// which is the closest the two can get without merging identities.
func (w *SendWorker) failExhausted(ctx context.Context, userID ulid.ULID, sub store.Submission, attempts int, cause error, log *slog.Logger) error {
	log.Error("submission failed: job exhausted its retries", "attempts", attempts, "err", cause)
	msg := fmt.Sprintf("retries exhausted after %d attempt(s); last error: %s", attempts, cause.Error())
	if err := w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if err := tx.ReleaseOutbound(ctx, sub.AccountID); err != nil {
			return err
		}
		return tx.FinishSubmission(ctx, sub.ID, store.SubmissionFailed, msg)
	}); err != nil {
		return err
	}
	return cause
}

// requeue returns a transiently-failed submission to 'scheduled' (so the job's
// next attempt can claim it again) and gives back the quota it reserved.
func (w *SendWorker) requeue(ctx context.Context, userID ulid.ULID, sub store.Submission, cause error) error {
	return w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if err := tx.ReleaseOutbound(ctx, sub.AccountID); err != nil {
			return err
		}
		return tx.FinishSubmission(ctx, sub.ID, store.SubmissionScheduled, cause.Error())
	})
}

// deferForQuota handles an over-quota send: the submission stays SCHEDULED and a
// fresh job is queued for the next daily reset. Spec §5 is explicit that sends
// "queue rather than silently failing late" when a provider's daily cap is
// reached — so the send is neither dropped nor burned through the job's retry
// budget in the next hour, it simply waits for the budget to come back.
func (w *SendWorker) deferForQuota(ctx context.Context, userID, subID ulid.ULID, log *slog.Logger) error {
	next := nextQuotaReset(time.Now())
	log.Warn("account outbound daily quota is exhausted; the send is queued until the cap resets",
		"retry_at", next)
	return w.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// A distinct idempotency key per reset day, so re-deferring on
		// successive days is not a duplicate-key failure.
		key := fmt.Sprintf("submit:%s:%s", subID, next.Format("2006-01-02"))
		jobID, err := tx.EnqueueJob(ctx, SubmitJobKind, next,
			[]byte(`{"submission_id":"`+subID.String()+`"}`), key)
		if err != nil {
			return err
		}
		return tx.SetSubmissionJob(ctx, subID, jobID)
	})
}

// nextQuotaReset is the next UTC midnight — when app.outbound_ledger rolls to a
// new `day` and the account's budget is fresh.
func nextQuotaReset(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).
		Add(24 * time.Hour)
}
