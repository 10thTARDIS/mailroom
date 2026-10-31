// SPDX-License-Identifier: AGPL-3.0-only

package writeback

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mailroom/internal/blob"
	"mailroom/internal/store"
	imapsync "mailroom/internal/sync"

	"github.com/oklog/ulid/v2"
)

// DefaultMaxAttempts bounds how many times a transiently-failing intent is
// retried before it is dead-lettered. attempts is incremented at claim time,
// so the first application runs with attempts == 1.
const DefaultMaxAttempts = 5

// IntentStore is the store surface the engine needs. Each method manages its
// own short transaction (see StoreIntents) so the engine never holds a DB
// transaction open across a network IMAP round-trip. It is an interface so the
// engine unit-tests against a fake with no database.
type IntentStore interface {
	ClaimPendingIntents(ctx context.Context, accountID ulid.ULID, limit int) ([]store.Intent, error)
	// MessageProvenanceTargetsForAccount returns EVERY live copy of the message
	// in the account, not one (#296) — see the store method's doc for what the
	// returned set guarantees.
	MessageProvenanceTargetsForAccount(ctx context.Context, messageID, accountID ulid.ULID) ([]store.ProvenanceTarget, error)
	// MarkIntentApplied and MarkIntentFailed report whether a row actually
	// moved; false means the intent was no longer inflight, usually because it
	// was cascaded away mid-pass (#691). The engine counts that as Vanished.
	MarkIntentApplied(ctx context.Context, id ulid.ULID) (bool, error)
	MarkIntentFailed(ctx context.Context, id ulid.ULID, errMsg, deadReason string) (bool, error)
	// PurgeExpungeStillEligible re-asks at APPLY time the question the purge
	// sweep answered at ENQUEUE time (#555). See the store method's doc for why
	// asking twice is the whole point.
	PurgeExpungeStillEligible(ctx context.Context, messageID, accountID ulid.ULID) (bool, store.ExpungeSkip, error)
	// WithdrawIntent drops an intent this pass has claimed and must not apply.
	WithdrawIntent(ctx context.Context, id ulid.ULID) error
	// RecordStoredFlags notes on one live provenance row the flags a STORE the
	// server just acknowledged added to or removed from that copy (#576). See
	// store.UserTx.ApplyStoredFlagDelta for why a sync alone cannot.
	RecordStoredFlags(ctx context.Context, provenanceID ulid.ULID, add, remove []string) error
	// RecordGmailLabels moves one live Gmail copy's gm_labels baseline by a
	// label delta the server just confirmed (#52, ADR-047 §3: echo
	// suppression). See store.UserTx.RecordGmailLabelDelta.
	RecordGmailLabels(ctx context.Context, provenanceID ulid.ULID, add, remove []string) error
}

// IMAPWriter is the set of IMAP write operations the engine applies. It is
// expressed in mailroom's own sync.UID/sync.Flag types (not go-imap types), so
// internal/sync.SyncSession can grow to satisfy it directly (see the package
// doc). mailbox is the raw IMAP mailbox name to operate in; the implementation
// SELECTs as needed. Every op targets a message by UID within that mailbox,
// except Append which writes new bytes.
type IMAPWriter interface {
	// StoreFlags issues a UID STORE that adds and/or removes flags/keywords on
	// the message identified by uid in mailbox. add and remove may each be
	// empty. It must be safe to call with a flag already in the desired state
	// (server-side no-op).
	StoreFlags(ctx context.Context, mailbox string, uid imapsync.UID, add, remove []imapsync.Flag) error
	// Copy copies the message to dest (UID COPY). Used for move on servers
	// without the MOVE extension.
	Copy(ctx context.Context, mailbox string, uid imapsync.UID, dest string) error
	// Move moves the message to dest in one step (UID MOVE, RFC 6851). Only
	// called when SupportsMove reports true.
	Move(ctx context.Context, mailbox string, uid imapsync.UID, dest string) error
	// Expunge permanently removes the message (UID EXPUNGE where available,
	// else EXPUNGE after \Deleted). It returns how many messages the SERVER
	// said it removed.
	//
	// That count is not decoration: a UID EXPUNGE naming a UID the server no
	// longer holds returns success, so without it "I destroyed the message"
	// and "there was nothing there" are the same green — the precise failure
	// Result.Expunged exists to make visible (#268, #296). 0 with a nil error
	// is a legitimate answer, not a failure; on a server without UIDPLUS the
	// fallback is a plain EXPUNGE, so the count can exceed the one UID asked
	// about and callers must treat it as a floor rather than an attribution.
	Expunge(ctx context.Context, mailbox string, uid imapsync.UID) (int, error)
	// Append appends raw RFC 5322 bytes to mailbox with the given flags and
	// returns the UID the server assigned it (RFC 4315 APPENDUID). The UID is 0
	// when the server does not advertise UIDPLUS and therefore did not say — a
	// missing answer, never an error. See Engine.appendedThisPass for the one
	// thing the engine does with it.
	Append(ctx context.Context, mailbox string, flags []imapsync.Flag, body []byte) (imapsync.UID, error)
	// SearchMessageID returns the UIDs in mailbox whose Message-ID header
	// matches. It backs two things: the Sent self-append dedup (is the
	// provider's own copy already there?) and the header-addressed expunge of
	// a superseded draft (which copy is ours?).
	SearchMessageID(ctx context.Context, mailbox string, messageID string) ([]imapsync.UID, error)
	// UIDPresent reports whether uid is still present in mailbox (a bare UID
	// SEARCH for that one UID). It exists ONLY to answer the question
	// Expunge's own return value cannot when it reports removing nothing
	// (#611): did the server truly have nothing there, or did it keep the
	// message anyway — not every provider is strictly read-your-writes
	// across a STORE \Deleted and the UID EXPUNGE that follows it. A
	// message flagged \Deleted but not yet expunged still matches a UID
	// SEARCH, which is exactly the "still present" this probe is meant to
	// catch. See expungeOne, the one place it is called from.
	UIDPresent(ctx context.Context, mailbox string, uid imapsync.UID) (bool, error)
	// SupportsMove reports whether the server advertised the MOVE extension.
	SupportsMove() bool
}

// BlobReader resolves a user's raw message bytes by content hash. The engine
// borrows it ONLY to apply an APPEND intent whose payload names its bytes by
// hash rather than carrying them inline (mailroom#89) — never stores what it
// reads.
//
// Its shape (ctx, userID, contentHash) -> (raw, err) is deliberately the same
// as search.BlobContent.Raw: cmd/mailroom hands the engine the EXACT SAME
// instance it already builds for the search indexer, so there is exactly one
// mail-blob DEK derivation path (syncrunner.MailBlobInfoV1) in the whole
// process, not a second one that could silently drift and decrypt garbage.
type BlobReader interface {
	Raw(ctx context.Context, userID ulid.ULID, contentHash []byte) ([]byte, error)
}

// Engine drains an account's writeback queue and applies each intent to the
// IMAP server. It is store- and session-agnostic (both are interfaces).
type Engine struct {
	store       IntentStore
	blobs       BlobReader
	userID      ulid.ULID
	batchSize   int
	maxAttempts int
	// gmail is Gmail mode (WithGmail, ADR-047 §3): every intent is applied
	// with Gmail's primitives, never the generic folder ops. folders names
	// the account's Gmail mailboxes.
	gmail   bool
	folders store.GmailFolders
	// gmailHold is the runner's Gmail writeback hold check
	// (WithGmailWritebackHold); nil means store.GmailWritebackHeld.
	gmailHold func(authKind, providerProfile string) bool
}

// Option configures an Engine.
type Option func(*Engine)

// WithBlobReader configures the engine to resolve an APPEND intent's bytes
// from the blob store by content hash when its payload does not carry them
// inline (mailroom#89). userID scopes every read the same way IntentStore is
// already scoped to one user (see NewStoreIntents) — the engine itself is
// reconstructed per user, per DrainAccount pass (runner.go), so this is set
// fresh alongside it rather than threaded through every call.
func WithBlobReader(r BlobReader, userID ulid.ULID) Option {
	return func(e *Engine) {
		e.blobs = r
		e.userID = userID
	}
}

// WithBatchSize sets how many intents ApplyPending claims per call (default 64).
func WithBatchSize(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.batchSize = n
		}
	}
}

// WithMaxAttempts sets the retry bound before dead-lettering (default
// DefaultMaxAttempts).
func WithMaxAttempts(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.maxAttempts = n
		}
	}
}

// NewEngine builds an Engine over the given intent store.
func NewEngine(s IntentStore, opts ...Option) *Engine {
	e := &Engine{store: s, batchSize: 64, maxAttempts: DefaultMaxAttempts}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Result summarizes one ApplyPending pass.
type Result struct {
	Applied      int // intents successfully mirrored to the server
	Retried      int // transient failures returned to pending
	DeadLettered int // intents moved to the surfaced dead state
	Conflicts    int // subset of DeadLettered caused by provenance divergence
	Deduped      int // appends skipped because the provider had already filed the message
	// Expunged counts the messages actually REMOVED from the server. It is
	// deliberately not derivable from Applied: an expunge that removed nothing
	// and an expunge that removed a message used to be the same green (#268),
	// which is exactly how a draft survived every send without anything
	// reporting a failure. A caller — or a test — that wants to know whether
	// mail was destroyed must be able to ask.
	Expunged int
	// Withdrawn counts intents that were claimed and then deliberately NOT
	// applied, because their precondition stopped holding between enqueue and
	// apply (#555: the user restored the message from Trash, or another client
	// moved it out, after the purge sweep had already queued its EXPUNGE).
	//
	// ⚠ IT IS DELIBERATELY NOT FOLDED INTO Applied OR DeadLettered, for the same
	// reason Expunged is not derivable from Applied. Counted as Applied it would
	// claim an IMAP operation that never happened; counted as DeadLettered it
	// would report the guard that just saved a message from destruction as a
	// writeback failure, and surface a dead_reason to the user for working
	// correctly. A non-zero value here is the guard firing, which is the number
	// worth being able to ask for — an operator wanting to know whether this
	// ever actually happens on real mail has nothing else to read.
	Withdrawn int
	// Vanished counts intents whose final mark moved no row: the intent was
	// no longer inflight when the pass came to record it, most often because
	// deleting its message cascaded it away mid-pass (#691). They are counted
	// here INSTEAD of Applied, Retried or DeadLettered, because those counts
	// are read as "there is a row in that state" — a pass once logged
	// dead_lettered=1 with no dead intent anywhere, which sent an
	// investigation looking for a failure that did not exist. The IMAP
	// operation itself may still have happened; Expunged and Deduped describe
	// that and are unaffected.
	Vanished int
	// Reresolved counts Gmail copies whose UID was found by X-GM-MSGID at a
	// different UID than Mailroom had stored (#52, ADR-047 §3): the message
	// left All Mail and came back, and the write went to where it is now.
	Reresolved int
	// LabelsCreated counts Gmail labels this pass CREATEd because a label
	// add did not stick without one (whether STORE creates a missing label
	// is UNVERIFIED, ADR-047).
	LabelsCreated int
	// Claimed is how many intents the pass claimed. A pass that claimed a full
	// batch may have left more behind, which is the only thing Drain needs to
	// know to decide whether to go round again.
	Claimed int
}

// add folds another pass's tally into r, for a drain of several passes.
func (r *Result) add(o Result) {
	r.Applied += o.Applied
	r.Retried += o.Retried
	r.DeadLettered += o.DeadLettered
	r.Conflicts += o.Conflicts
	r.Deduped += o.Deduped
	r.Expunged += o.Expunged
	r.Withdrawn += o.Withdrawn
	r.Vanished += o.Vanished
	r.Reresolved += o.Reresolved
	r.LabelsCreated += o.LabelsCreated
	r.Claimed += o.Claimed
}

// DrainLimits bounds one Drain: at most MaxRounds passes, and no new pass once
// Budget has elapsed since the first began.
type DrainLimits struct {
	MaxRounds int
	Budget    time.Duration
}

// DefaultDrainLimits is what the runner uses. 16 rounds of 64 is 1,024 intents
// per drain; 30 s keeps the account's sync pass, which runs straight after the
// drain on the same per-account sequence, from waiting more than about one
// poll interval behind a large clean-up.
var DefaultDrainLimits = DrainLimits{MaxRounds: 16, Budget: 30 * time.Second}

// Drain runs ApplyPending repeatedly on ONE writer while each pass comes back
// with a full batch, so a large backlog drains in one go instead of 64 intents
// per sync pass (#706).
//
// MEASURED BEFORE THIS EXISTED, on a real instance: a bulk move of
// many messages reached the provider over many minutes, at exactly 64 per pass
// with passes seconds apart. Each pass also dialled and logged in afresh,
// because the next 64 waited for the account's next sync pass.
//
// It stops at the first error (each pass already records per-intent outcomes,
// so stopping loses nothing), on a short batch (nothing more to claim), at
// MaxRounds or Budget, or when ctx is done. round is 1-based, for the caller's
// per-pass log line.
func (e *Engine) Drain(ctx context.Context, account ulid.ULID, w IMAPWriter, lim DrainLimits,
	onPass func(round int, res Result, took time.Duration)) (Result, error) {
	var total Result
	begin := time.Now()
	for round := 1; ; round++ {
		start := time.Now()
		res, err := e.ApplyPending(ctx, account, w)
		if onPass != nil {
			onPass(round, res, time.Since(start))
		}
		total.add(res)
		if err != nil {
			return total, err
		}
		if res.Claimed < e.batchSize || round >= lim.MaxRounds ||
			time.Since(begin) >= lim.Budget || ctx.Err() != nil {
			return total, nil
		}
	}
}

// pass is the mutable state of one ApplyPending call: the tally being built,
// plus what the pass has learned about the server as it went.
type pass struct {
	res Result
	// appendedUIDs is the UID the server assigned to each APPEND this pass
	// performed, keyed by appendKey. It exists for ONE reason (#268): a draft's
	// APPEND and the EXPUNGE that supersedes it routinely land in the same pass,
	// on the same IMAP session, milliseconds apart — and a SEARCH issued that
	// soon after our own APPEND has been observed to return nothing on a real
	// provider, which made the expunge a silent no-op. When we appended the
	// message ourselves moments ago we do not need to ask the server which UID
	// it is: it told us.
	appendedUIDs map[appendKey]imapsync.UID
	// The Gmail path's per-pass memory (gmail.go): each copy's UID as
	// re-resolved by X-GM-MSGID, the labels already CREATEd, and the drafts
	// (by Message-ID) already retired.
	gmailUIDs    map[gmailKey]imapsync.UID
	gmailCreated map[string]bool
	gmailRetired map[string]bool
}

// appendKey identifies a message the pass appended, by where it was put and
// which message it is.
type appendKey struct {
	mailbox   string
	messageID string
}

// recordAppend remembers the UID an APPEND was given, when the server said.
func (p *pass) recordAppend(mailbox, messageID string, uid imapsync.UID) {
	if messageID == "" || uid == 0 {
		// No UIDPLUS (uid == 0) or nothing to key on: we simply do not know, and
		// guessing a UID is how the wrong message gets destroyed.
		return
	}
	if p.appendedUIDs == nil {
		p.appendedUIDs = make(map[appendKey]imapsync.UID, 1)
	}
	p.appendedUIDs[appendKey{mailbox: mailbox, messageID: messageID}] = uid
}

// appendedThisPass reports the UID this pass appended for (mailbox, messageID).
func (p *pass) appendedThisPass(mailbox, messageID string) (imapsync.UID, bool) {
	uid, ok := p.appendedUIDs[appendKey{mailbox: mailbox, messageID: messageID}]
	return uid, ok
}

// intentPayload is the JSONB argument bag stored on an intent. Fields are
// per-op; only the ones an op needs are read (see apply).
type intentPayload struct {
	Flag        string   `json:"flag,omitempty"`        // +flag / -flag
	Keyword     string   `json:"keyword,omitempty"`     // +kw / -kw
	Mailbox     string   `json:"mailbox,omitempty"`     // move dest, append target
	TargetRole  string   `json:"target_role,omitempty"` // move dest as a JMAP role
	UIDValidity *int64   `json:"uidvalidity,omitempty"` // conflict baseline
	Flags       []string `json:"flags,omitempty"`       // append initial flags
	// Body carries the append's raw bytes inline (base64 in JSON). LEGACY ONLY
	// (mailroom#89): an intent enqueued before this change may still have this
	// set, and appendBody honors it so an in-flight intent from before the
	// upgrade is never stranded. Nothing enqueues it anymore — a current
	// APPEND intent carries ContentHash instead, and the engine reads the
	// bytes from the blob store.
	Body []byte `json:"body,omitempty"`
	// ContentHash (hex) names an APPEND's bytes by content hash instead of
	// carrying them (mailroom#89): appendBody resolves it via BlobReader when
	// Body is empty.
	ContentHash string `json:"content_hash,omitempty"`
	// MessageIDHeader names a message by its RFC 5322 Message-ID rather than by
	// a message row. On an APPEND it is the self-append dedup probe (is the
	// provider's copy already in the target mailbox? — spec §4). On an EXPUNGE
	// with no message_id it IS the target: the engine SEARCHes for it in
	// Mailbox and expunges what it finds. See applyHeaderExpunge.
	MessageIDHeader string `json:"message_id_header,omitempty"`
	// Label is the NAME of the Mailroom label a ±kw intent mirrors (its
	// keyword atom is Keyword). A Gmail account writes it as X-GM-LABELS
	// (gmail.go); every other account ignores it and writes the keyword.
	Label string `json:"label,omitempty"`
}

// ApplyPending claims a batch of pending intents for account and applies each
// via w. It returns a Result summarizing the pass. A per-intent op failure is
// recorded on the intent (retry or dead-letter) and does not abort the pass;
// only an infrastructure failure (claim, or a store mark call) is returned as
// an error, aggregated across the batch.
func (e *Engine) ApplyPending(ctx context.Context, account ulid.ULID, w IMAPWriter) (Result, error) {
	intents, err := e.store.ClaimPendingIntents(ctx, account, e.batchSize)
	if err != nil {
		return Result{}, fmt.Errorf("writeback: claim pending: %w", err)
	}
	var (
		p    pass
		errs []error
	)
	p.res.Claimed = len(intents)
	for _, in := range intents {
		if err := e.applyOne(ctx, in, w, &p); err != nil {
			errs = append(errs, err)
		}
	}
	return p.res, errors.Join(errs...)
}

// applyOne applies a single claimed intent. It returns a non-nil error only
// when a store mark call fails (an infrastructure problem); an IMAP-level or
// validation failure is handled by transitioning the intent and is not
// returned.
func (e *Engine) applyOne(ctx context.Context, in store.Intent, w IMAPWriter, st *pass) error {
	var p intentPayload
	if len(in.Payload) > 0 {
		if err := json.Unmarshal(in.Payload, &p); err != nil {
			// A malformed payload can never succeed on retry: dead-letter it.
			return e.deadLetter(ctx, in, st, "", fmt.Sprintf("invalid payload: %v", err))
		}
	}

	// GMAIL MODE (ADR-047 §3): no intent for a Gmail account may take a
	// generic folder op. A session that cannot write Gmail labels gets
	// NOTHING written, not the generic fallback.
	var g GmailWriter
	if e.gmail {
		var ok bool
		if g, ok = gmailCapable(w); !ok {
			return e.failTransient(ctx, in, st, errors.New(
				"gmail: the session does not offer X-GM-EXT-1 label writes; nothing was written"))
		}
	}

	// APPEND targets a mailbox directly and needs no existing message.
	if in.Op == store.IntentAppend {
		if p.Mailbox == "" {
			return e.deadLetter(ctx, in, st, "", "append: payload missing target mailbox")
		}
		body, deadReason, err := e.appendBody(ctx, p)
		if deadReason != "" {
			return e.deadLetter(ctx, in, st, "", deadReason)
		}
		if err != nil {
			return e.failTransient(ctx, in, st, fmt.Errorf("append: resolve body: %w", err))
		}
		// SELF-APPEND DEDUP (spec §4, ADR-001): most providers do not file a
		// copy of a message you submit over SMTP — but some do (Gmail), and for
		// those, appending our own copy would leave the user with two of every
		// sent message. So ask the server FIRST whether this Message-ID is
		// already in the target mailbox, and skip the append if it is.
		//
		// A SEARCH failure does NOT block the append: given the choice between
		// possibly duplicating a Sent copy and possibly LOSING one, duplication
		// is the recoverable outcome.
		if p.MessageIDHeader != "" {
			if uids, err := w.SearchMessageID(ctx, p.Mailbox, p.MessageIDHeader); err == nil && len(uids) > 0 {
				st.res.Deduped++
				return e.markApplied(ctx, in, st)
			}
		}
		uid, err := w.Append(ctx, p.Mailbox, toFlags(p.Flags), body)
		if err != nil {
			return e.failTransient(ctx, in, st, err)
		}
		// Remember where the server put it, for an expunge later in this same
		// pass that would otherwise have to find it by SEARCH (#268).
		st.recordAppend(p.Mailbox, p.MessageIDHeader, uid)
		return e.markApplied(ctx, in, st)
	}

	// A header-addressed EXPUNGE names its target by Message-ID instead of by a
	// message row, because the row is gone: it is how a superseded draft version
	// is removed from the IMAP Drafts folder (store.RetireDraft).
	if in.Op == store.IntentExpunge && in.MessageID == nil && p.MessageIDHeader != "" {
		if e.gmail {
			return e.applyGmailHeaderExpunge(ctx, in, p, w, g, st)
		}
		return e.applyHeaderExpunge(ctx, in, p, w, st)
	}

	// Every other op mutates an existing message; resolve its live location.
	if in.MessageID == nil {
		return e.deadLetter(ctx, in, st, "", fmt.Sprintf("op %q requires a message_id but none was set", in.Op))
	}
	targets, err := e.store.MessageProvenanceTargetsForAccount(ctx, *in.MessageID, in.AccountID)
	if err != nil {
		// A read failure is transient infrastructure, not a lost delta: retry.
		return e.failTransient(ctx, in, st, fmt.Errorf("read provenance: %w", err))
	}
	if reason, conflict := conflictReason(targets, p.UIDValidity); conflict {
		counted, err := e.deadLetterCounted(ctx, in, st, "", reason)
		if counted {
			st.res.Conflicts++
		}
		return err
	}

	if reason, ok := validateOp(in.Op, p); !ok {
		return e.deadLetter(ctx, in, st, "", reason)
	}
	if e.gmail {
		if reason, ok := validateGmailOp(in.Op, p); !ok {
			return e.deadLetter(ctx, in, st, "", reason)
		}
	}

	// ⚠ RE-ASK THE DESTRUCTIVE QUESTION BEFORE ANSWERING IT (#555). Phase 1 of
	// the purge decides "every live copy of this message is in this account's
	// Trash, and its retention has expired", enqueues an EXPUNGE, and until now
	// that decision was simply assumed to still hold whenever the drain got
	// round to it. It can stop holding in the minutes in between: the user
	// restores the message, or another client moves it out of Trash and
	// Mailroom learns at the next sync. The engine then destroyed the only
	// server copy of mail the user had just asked to keep — silently, because
	// from the engine's side everything succeeded.
	//
	// ⚠ GATED ON Source, NOT ON Op. A draft-retirement EXPUNGE is the same op
	// with the same shape and a precondition that has nothing to do with Trash;
	// running this check on one would refuse to retire drafts forever. And the
	// test must be `== IntentByPurge`, never `!= IntentByDraft`: an empty source
	// means "enqueued before migration 00057, actor unrecoverable", and sweeping
	// unknown rows into the branch that decides whether to destroy mail is
	// exactly the direction not to fail in. An unattributed row keeps the old
	// behaviour rather than gaining a check it was never enqueued under.
	//
	// This closes the general case, including the one no cancel-on-restore can
	// reach: when another client moves the copy out of Trash there is no local
	// transaction to hook a cancel onto.
	if in.Op == store.IntentExpunge && in.Source == store.IntentByPurge {
		eligible, skip, err := e.store.PurgeExpungeStillEligible(ctx, *in.MessageID, in.AccountID)
		if err != nil {
			// Never destroy mail because a safety READ failed. Retry the whole
			// intent instead: the copy is still on the server, still in Trash,
			// and the next sweep will ask again.
			return e.failTransient(ctx, in, st, fmt.Errorf("re-check purge eligibility: %w", err))
		}
		if !eligible {
			return e.withdraw(ctx, in, st, string(skip))
		}
	}
	if e.gmail {
		return e.applyGmailToEveryCopy(ctx, in, p, targets, w, g, st)
	}
	return e.applyToEveryCopy(ctx, in, p, targets, w, st)
}

// applyToEveryCopy issues the op against EVERY live copy of the message in this
// account and decides the intent's fate from the whole set (#296).
//
// WHY A LOOP AT ALL. One intent is enqueued per ACCOUNT (store.liveAccountIDs
// selects DISTINCT account_id) but a message can have several live copies inside
// one account — re-delivered mail recorded by #206, a copy filed in two folders.
// Until v1.34.4 the engine resolved that account to ONE copy and stopped, while
// ingest merged the flags of every copy with OR. Adds converged, so nothing
// looked wrong; removals reverted on the very next poll, because the untouched
// copies still carried the flag and the merge put it back.
//
// PARTIAL FAILURE: RETRY THE WHOLE INTENT, NEVER MARK IT APPLIED.
//
// Every target is attempted even after one fails, because the failures are
// usually independent — a mailbox that has gone away, a per-folder permission —
// and stopping at the first one would hand the remaining copies the exact silent
// non-application this issue is about. But if ANY target failed, the intent goes
// back to pending (or dead-letters once the attempt budget is spent) and the
// next pass re-applies it to ALL of them, including the ones that already
// succeeded.
//
// Re-applying to a copy that already took the write is safe and is the reason
// this design is affordable: the flag/keyword ops the multi-copy case actually
// affects are intrinsically idempotent (STORE +FLAGS \Seen on an already-seen
// message is a server-side no-op — see the package doc), and a UID EXPUNGE of a
// UID already gone is a no-op too. move-via-copy is the one op that is not
// idempotent, and it carries that caveat already for the single-copy case; the
// multi-copy case multiplies it but does not create it.
//
// The alternative — remembering which targets succeeded so a retry skips them —
// needs per-target durable state that app.writeback_intents does not have, i.e.
// a schema change. The trade is deliberate: an occasional redundant idempotent
// write, against never recording "applied" for an op that reached only some of
// the user's copies.
func (e *Engine) applyToEveryCopy(ctx context.Context, in store.Intent, p intentPayload, targets []store.ProvenanceTarget, w IMAPWriter, st *pass) error {
	var failures []error
	for _, t := range targets {
		destroyed, err := e.applyToTarget(ctx, in.Op, p, t, w)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s uid %d: %w", t.IMAPName, t.UID, err))
			continue
		}
		// THE SERVER NOW HOLDS WHAT WE STORED, SO OUR RECORD OF THE COPY MUST SAY
		// SO (#576). Otherwise a MOVE later in this same action takes the copy
		// away before any sync re-reads its flags, and the row keeps the flags
		// it had before this STORE — which the flag merge then ORs into the
		// message the moment its new copy is ingested. A failure here is
		// retried like a failed STORE: re-applying the STORE is idempotent, and
		// marking the intent applied over a stale record would leave exactly
		// that merge in place.
		if add, remove, ok := storedFlags(in.Op, p); ok {
			if err := e.store.RecordStoredFlags(ctx, t.ProvenanceID, add, remove); err != nil {
				failures = append(failures, fmt.Errorf("%s uid %d: record stored flags: %w", t.IMAPName, t.UID, err))
				continue
			}
		}
		if destroyed {
			// Counted here, outside the success branch below, because these
			// copies ARE gone regardless of what happens to the intent. Expunged
			// is a claim about mail, not about bookkeeping.
			st.res.Expunged++
		}
	}
	if len(failures) > 0 {
		// Name the bound, not just the failure: "3 of 4 copies" is what tells an
		// operator reading a dead_reason whether mail is half-mirrored, and a
		// single joined error string cannot say how many copies there were.
		return e.failTransient(ctx, in, st, fmt.Errorf(
			"applied to %d of %d live copies in this account, %d failed: %w",
			len(targets)-len(failures), len(targets), len(failures), errors.Join(failures...)))
	}
	return e.markApplied(ctx, in, st)
}

// appendBody resolves the bytes an APPEND intent writes to the server.
//
// A LEGACY intent — enqueued before mailroom#89 — carries the body inline; a
// current one carries only ContentHash, and the bytes are read back out of
// the blob store instead (store.EnqueueAppendIntent's doc: the caller writes
// the blob strictly before enqueuing, so a live intent can never outlive the
// bytes it names).
//
// deadReason non-empty means the caller must dead-letter: the intent
// structurally cannot ever resolve bytes (a malformed or absent content hash,
// or no BlobReader configured — a deployment/wiring problem, not a transient
// one), or the blob has genuinely, permanently ceased to exist
// (blob.ErrNotFound). Any other error is transient (a momentary I/O failure,
// or a key provider that could not open the master key this pass) and the
// caller must retry.
func (e *Engine) appendBody(ctx context.Context, p intentPayload) (body []byte, deadReason string, err error) {
	if len(p.Body) > 0 {
		return p.Body, "", nil
	}
	if p.ContentHash == "" {
		return nil, "append: payload has neither an inline body nor a content hash", nil
	}
	if e.blobs == nil {
		return nil, "append: engine has no blob reader configured to resolve content_hash", nil
	}
	hash, hexErr := hex.DecodeString(p.ContentHash)
	if hexErr != nil {
		return nil, fmt.Sprintf("append: malformed content_hash: %v", hexErr), nil
	}
	raw, readErr := e.blobs.Raw(ctx, e.userID, hash)
	if readErr != nil {
		if errors.Is(readErr, blob.ErrNotFound) {
			return nil, fmt.Sprintf("append: blob for content_hash %s no longer exists", p.ContentHash), nil
		}
		return nil, "", readErr
	}
	return raw, "", nil
}

// expungeOne issues the STORE \Deleted + UID EXPUNGE pair that removes one
// message, and re-observes the server when the expunge claims to have
// removed nothing (#611). This is deliberately the ONLY place that pair may
// appear in this package: the incident this fixes was measured at one of
// three call sites that all issue the identical pair (applyHeaderExpunge,
// applyToTarget's IntentExpunge case, and applyToTarget's no-MOVE fallback),
// and this project has repeatedly fixed one of two or three copies of a code
// path and left the others carrying the bug.
//
// removed reports whether the server said it destroyed the message —
// Result.Expunged's definition (see the package doc and #296): true only
// when Expunge itself reported a count > 0. A zero count is not
// automatically a failure — on a server without UIDPLUS the plain-EXPUNGE
// fallback can genuinely remove nothing because there was nothing \Deleted
// to sweep — but it is also not automatically success: on a real
// instance a UID EXPUNGE of exactly one UID returned zero with no error,
// and the message it named was still on the server, still flagged \Deleted,
// an hour later. So a zero count is RE-ASKED, not trusted:
//
//   - still present: the server did not do what it was told. That is a
//     transient failure — the caller retries it like any other, and
//     failTransient dead-letters it with this reason once attempts run
//     out — never a silent success.
//   - absent: genuinely gone (another client removed it first, or the
//     server simply did not report the count it should have). removed
//     stays false; this does NOT count toward Result.Expunged, which means
//     "the server told us it destroyed mail", not "we inferred it must be
//     gone".
//   - the presence probe itself fails: we cannot tell either way, so it is
//     treated the same as "still present" rather than as permission to
//     move on — the same fail-closed stance PurgeExpungeStillEligible's
//     read failure takes in applyOne.
func expungeOne(ctx context.Context, w IMAPWriter, mailbox string, uid imapsync.UID) (removed bool, err error) {
	if err := w.StoreFlags(ctx, mailbox, uid, []imapsync.Flag{imapsync.FlagDeleted}, nil); err != nil {
		return false, err
	}
	n, err := w.Expunge(ctx, mailbox, uid)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	present, err := w.UIDPresent(ctx, mailbox, uid)
	if err != nil {
		return false, fmt.Errorf("expunge: could not confirm whether uid %d in %q was actually removed: %w", uid, mailbox, err)
	}
	if present {
		return false, fmt.Errorf("expunge: server kept uid %d in %q after UID EXPUNGE (flagged \\Deleted, not removed)", uid, mailbox)
	}
	return false, nil
}

// applyHeaderExpunge removes the copy of a message identified by its Message-ID
// header from a mailbox.
//
// It resolves the UID by SEARCH rather than trusting a stored one, which is what
// makes it SAFE: a UID is only meaningful within a UIDVALIDITY generation, and a
// stale one could point at a message the server has since given that number to.
// Expunging the wrong message is unrecoverable, so the target is re-identified,
// on the server, immediately before it is destroyed.
//
// ⚠ AN EMPTY SEARCH IS A FAILURE, NOT SUCCESS (#268). This function used to fall
// straight through to markApplied when it found nothing, so "I searched and found
// nothing" and "I deleted it" were recorded identically — the intent retired, no
// retry, and the copy left on the server forever. That is not a hypothetical:
// on a real instance an expunge ran moments after its own APPEND, matched
// nothing, reported applied, and the draft it was supposed to remove was still
// in Drafts an hour later. An operation that cannot report failure is the
// defect; the copy surviving is only the symptom.
//
// So a header-addressed expunge is now enqueued ONLY when a copy is believed to
// exist (store.RetireSentDraftCopy skips it entirely when it cancelled the
// APPEND that would have created one), and finding nothing is a transient
// failure: retry, then dead-letter with a reason the operator can act on. The
// dead reason names BOTH readings honestly, because from here they are genuinely
// indistinguishable — another client may have removed the copy first.
//
// Before giving up, though, there is one case where the server does not need to
// be asked at all: when THIS pass appended the message itself and the server
// returned an APPENDUID. That is the exact race the incident was made of, and a
// UID from an APPEND seconds ago in this same session cannot have been recycled.
func (e *Engine) applyHeaderExpunge(ctx context.Context, in store.Intent, p intentPayload, w IMAPWriter, st *pass) error {
	if p.Mailbox == "" {
		return e.deadLetter(ctx, in, st, "", "expunge: payload names a Message-ID but no mailbox")
	}
	uids, err := w.SearchMessageID(ctx, p.Mailbox, p.MessageIDHeader)
	if err != nil {
		// Unlike the append probe, a failed SEARCH here must NOT be shrugged
		// off: without it we do not know what to delete, and guessing deletes
		// the wrong mail. Retry.
		return e.failTransient(ctx, in, st, fmt.Errorf("expunge: search %q: %w", p.Mailbox, err))
	}
	if len(uids) == 0 {
		uid, ok := st.appendedThisPass(p.Mailbox, p.MessageIDHeader)
		if !ok {
			return e.failTransient(ctx, in, st, fmt.Errorf(
				"expunge: no copy of %s found in %q — it may already have been removed by another client, or the server did not return it",
				p.MessageIDHeader, p.Mailbox))
		}
		// We put it there ourselves, this pass, and the server told us the UID.
		uids = []imapsync.UID{uid}
	}
	for _, uid := range uids {
		// expungeOne is the ONE place STORE \Deleted + UID EXPUNGE happens,
		// shared by all three call sites (#611) — see its doc for how it
		// tells "genuinely gone" apart from "the server kept it anyway".
		// This path already had the strongest evidence available that the
		// copy exists — a SEARCH that found the UID moments ago — but that
		// is a claim about what to target, not about what got destroyed;
		// the server's own removal count is what Expunged counts (#296).
		removed, err := expungeOne(ctx, w, p.Mailbox, uid)
		if err != nil {
			return e.failTransient(ctx, in, st, err)
		}
		if removed {
			st.res.Expunged++
		}
	}
	return e.markApplied(ctx, in, st)
}

// conflictReason decides whether the current provenance diverges from what the
// intent assumed. Provider-wins: any destructive divergence is a conflict the
// engine refuses to apply blindly (see the package doc).
//
// It judges the WHOLE target set, and fails closed on it (#296). The store
// hands back a homogeneous set — all live, or all stale, or empty — but this
// re-checks every element's state rather than trusting the first: the caller of
// this function goes on to write to every element, and a stale coordinate that
// slipped into that loop would mutate whichever message the server has since
// given that UID to. "Do less, not more" is cheap here and unrecoverable if
// wrong.
func conflictReason(targets []store.ProvenanceTarget, expectedUIDValidity *int64) (string, bool) {
	if len(targets) == 0 {
		return "target message has no live provenance in the account (moved or expunged remotely)", true
	}
	for _, t := range targets {
		if t.State != store.ProvenanceLive {
			return "target mailbox UIDVALIDITY is stale (mailbox recreated on the server)", true
		}
	}
	if expectedUIDValidity != nil {
		// One divergent copy conflicts the whole intent rather than applying to
		// the copies that still match. The intent state machine has one state,
		// not one per copy, so "applied to the two that agreed and dead-lettered
		// the third" is not expressible — and a half-applied delta is the shape
		// of bug this issue is about. (No production path sets this baseline
		// today; nothing in internal/store writes `uidvalidity` into a payload.
		// It stays supported and conservative for the day one does.)
		for _, t := range targets {
			if *expectedUIDValidity != t.UIDValidity {
				return fmt.Sprintf("UIDVALIDITY changed since the intent was created (expected %d, now %d)",
					*expectedUIDValidity, t.UIDValidity), true
			}
		}
	}
	return "", false
}

// validateOp checks that the payload carries the argument the op needs, so a
// structurally-invalid intent is dead-lettered rather than retried forever.
func validateOp(op store.IntentOp, p intentPayload) (string, bool) {
	switch op {
	case store.IntentAddFlag, store.IntentRemoveFlag:
		if p.Flag == "" {
			return fmt.Sprintf("op %q: payload missing flag", op), false
		}
	case store.IntentAddKeyword, store.IntentRemoveKeyword:
		if p.Keyword == "" {
			return fmt.Sprintf("op %q: payload missing keyword", op), false
		}
		// A keyword op sends `UID STORE ±FLAGS (<keyword>)`, so a keyword of
		// `\Draft` would set an IMAP SYSTEM flag that the next sync reads back
		// as canonical state (is_draft arms a permanent delete, #49 review).
		// System flags travel only as flag ops, from the store's own constants;
		// a keyword op carrying one is refused here whatever enqueued it.
		if !store.KeywordWritable(p.Keyword) {
			return fmt.Sprintf("op %q: %q is an IMAP system flag or not a keyword; it cannot be written as one", op, p.Keyword), false
		}
	case store.IntentMove:
		if p.Mailbox == "" {
			// The store resolves the intent's target ROLE to each account's
			// actual IMAP mailbox name at enqueue time. An empty mailbox here
			// means that account has no mailbox with that role — so the move
			// cannot be represented, and it is dead-lettered with a reason the
			// user can act on rather than dropped (spec §4a: a user's "archive"
			// is never silently lost).
			if p.TargetRole != "" {
				return fmt.Sprintf("move: this account has no %q mailbox (no RFC 6154 SPECIAL-USE match), so the move cannot be mirrored to IMAP", p.TargetRole), false
			}
			return "move: payload missing destination mailbox", false
		}
	case store.IntentExpunge:
		// no payload args required
	default:
		return fmt.Sprintf("unknown op %q", op), false
	}
	return "", true
}

// applyToTarget issues the IMAP write for a message-targeted op against ONE
// copy's provenance coordinates.
//
// destroyed reports that this call actually removed a message from the server —
// only ever true for an EXPUNGE op, and only when the server said so. It is what
// keeps Result.Expunged from being a count of attempts (#296): a UID EXPUNGE of
// a UID the server no longer holds returns success, so a nil error proves the
// command was accepted, never that mail was destroyed. A move's internal expunge
// does not count: the message was relocated, not destroyed, and Expunged is read
// as "mail the user will not get back".
func (e *Engine) applyToTarget(ctx context.Context, op store.IntentOp, p intentPayload, t store.ProvenanceTarget, w IMAPWriter) (destroyed bool, err error) {
	uid := imapsync.UID(t.UID)
	switch op {
	case store.IntentAddFlag:
		return false, w.StoreFlags(ctx, t.IMAPName, uid, []imapsync.Flag{imapsync.Flag(p.Flag)}, nil)
	case store.IntentRemoveFlag:
		return false, w.StoreFlags(ctx, t.IMAPName, uid, nil, []imapsync.Flag{imapsync.Flag(p.Flag)})
	case store.IntentAddKeyword:
		return false, w.StoreFlags(ctx, t.IMAPName, uid, []imapsync.Flag{imapsync.Flag(p.Keyword)}, nil)
	case store.IntentRemoveKeyword:
		return false, w.StoreFlags(ctx, t.IMAPName, uid, nil, []imapsync.Flag{imapsync.Flag(p.Keyword)})
	case store.IntentMove:
		if w.SupportsMove() {
			return false, w.Move(ctx, t.IMAPName, uid, p.Mailbox)
		}
		// No MOVE extension: copy to dest, then mark \Deleted and expunge the
		// source. Not atomic and not idempotent on the copy step — see the
		// package doc's APPEND/move caveat.
		if err := w.Copy(ctx, t.IMAPName, uid, p.Mailbox); err != nil {
			return false, err
		}
		// expungeOne (#611) removes the source copy and re-asks the server
		// when it claims to have removed nothing, rather than trusting that
		// silently. destroyed stays false regardless: this branch is a
		// MOVE, and the message was relocated, not destroyed — see this
		// function's doc on what destroyed means.
		//
		// A retry after a surviving source RE-RUNS THE COPY — the package
		// doc's existing move-via-copy caveat: this step is not atomic, and
		// a crash (or, now, a source that refuses to go) between the copy
		// and the expunge can duplicate the destination. That is still
		// better than leaving the source live: a duplicated destination is
		// two server copies of IDENTICAL bytes, which content-hash identity
		// (spec §4, "one canonical message with a provenance list") shows as
		// one message; a surviving source is a second placement the user
		// actually sees. Neither is clean on the server — the duplicate copy
		// is the cost, and it is the lesser one.
		_, err := expungeOne(ctx, w, t.IMAPName, uid)
		return false, err
	case store.IntentExpunge:
		// Clamped to one by expungeOne asking about exactly one UID: one
		// copy is the most this call may claim. A server with no UIDPLUS
		// answers a plain EXPUNGE that sweeps every \Deleted message in the
		// mailbox, and reporting five destructions for a one-message
		// request would be a different dishonesty in the other direction.
		return expungeOne(ctx, w, t.IMAPName, uid)
	default:
		// Unreachable: validateOp rejected unknown ops already.
		return false, fmt.Errorf("writeback: unhandled op %q", op)
	}
}

// storedFlags is the flag delta a STORE-type op puts on a copy, in the form
// applyToTarget sends it; ok is false for every other op.
func storedFlags(op store.IntentOp, p intentPayload) (add, remove []string, ok bool) {
	switch op {
	case store.IntentAddFlag:
		return []string{p.Flag}, nil, true
	case store.IntentRemoveFlag:
		return nil, []string{p.Flag}, true
	case store.IntentAddKeyword:
		return []string{p.Keyword}, nil, true
	case store.IntentRemoveKeyword:
		return nil, []string{p.Keyword}, true
	}
	return nil, nil, false
}

// markApplied records success and updates the tally.
func (e *Engine) markApplied(ctx context.Context, in store.Intent, st *pass) error {
	moved, err := e.store.MarkIntentApplied(ctx, in.ID)
	if err != nil {
		return fmt.Errorf("writeback: mark applied %s: %w", in.ID, err)
	}
	if !moved {
		st.res.Vanished++
		return nil
	}
	st.res.Applied++
	return nil
}

// failTransient routes a transient failure: retry if attempts remain, else
// dead-letter. attempts was incremented at claim, so in.Attempts is the count
// including this try.
func (e *Engine) failTransient(ctx context.Context, in store.Intent, st *pass, cause error) error {
	if in.Attempts >= e.maxAttempts {
		reason := fmt.Sprintf("gave up after %d attempts: %v", in.Attempts, cause)
		return e.deadLetter(ctx, in, st, cause.Error(), reason)
	}
	moved, err := e.store.MarkIntentFailed(ctx, in.ID, cause.Error(), "")
	if err != nil {
		return fmt.Errorf("writeback: mark retry %s: %w", in.ID, err)
	}
	if !moved {
		st.res.Vanished++
		return nil
	}
	st.res.Retried++
	return nil
}

// deadLetter moves an intent to the surfaced dead state. deadReason must be
// non-empty (it is what the user sees); errMsg is the optional low-level cause.
func (e *Engine) deadLetter(ctx context.Context, in store.Intent, st *pass, errMsg, deadReason string) error {
	_, err := e.deadLetterCounted(ctx, in, st, errMsg, deadReason)
	return err
}

// deadLetterCounted is deadLetter that also says whether the intent was
// counted as dead, so a caller keeping a subset count (Conflicts) counts only
// rows that exist.
func (e *Engine) deadLetterCounted(ctx context.Context, in store.Intent, st *pass, errMsg, deadReason string) (bool, error) {
	moved, err := e.store.MarkIntentFailed(ctx, in.ID, errMsg, deadReason)
	if err != nil {
		return false, fmt.Errorf("writeback: dead-letter %s: %w", in.ID, err)
	}
	if !moved {
		st.res.Vanished++
		return false, nil
	}
	st.res.DeadLettered++
	return true, nil
}

// withdraw drops a claimed intent that must NOT be applied after all, because
// its precondition stopped holding between enqueue and apply (#555).
//
// ⚠ IT MUST BE VISIBLE, which is why Result.Withdrawn exists rather than this
// returning silently. A destructive op quietly disappearing is its own failure
// mode: "the expunge never ran" would otherwise be indistinguishable from "the
// expunge was never queued". The engine has no logger of its own — the runner
// logs the pass Result — so the counter IS the signal, and the runner surfaces
// it beside expunged in the writeback pass line.
//
// The reason is the ExpungeSkip the re-check returned, kept as a parameter so a
// caller that does grow a log has which precondition went stale, not merely
// that one did.
func (e *Engine) withdraw(ctx context.Context, in store.Intent, st *pass, _ string) error {
	if err := e.store.WithdrawIntent(ctx, in.ID); err != nil {
		return fmt.Errorf("writeback: withdraw %s: %w", in.ID, err)
	}
	st.res.Withdrawn++
	return nil
}

// toFlags converts payload flag strings to sync.Flag.
func toFlags(ss []string) []imapsync.Flag {
	if len(ss) == 0 {
		return nil
	}
	out := make([]imapsync.Flag, len(ss))
	for i, s := range ss {
		out[i] = imapsync.Flag(s)
	}
	return out
}
