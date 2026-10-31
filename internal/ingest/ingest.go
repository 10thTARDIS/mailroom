// SPDX-License-Identifier: AGPL-3.0-only

package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"mailroom/internal/alias"
	"mailroom/internal/attachproj"
	"mailroom/internal/autocrypt"
	"mailroom/internal/blob"
	"mailroom/internal/clamd"
	"mailroom/internal/dsn"
	"mailroom/internal/kind"
	"mailroom/internal/mime"
	"mailroom/internal/ml"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/placement"
	"mailroom/internal/rules"
	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// ErrQuotaExhausted is the sentinel ingestNew returns when storing a NEW message
// would push the user over their storage quota. It is THE load-bearing pause
// signal (spec §10, family of #86/#88/#93): the provider still holds mail we have
// no room for, and the only safe answer is to STOP and leave that mail on the
// server, never a silent drop. The sync engine turns this — and ONLY this — into
// a whole-account pause that raises account health; every other ingest error
// keeps its per-mailbox isolation. Match it with errors.Is; read the storage
// figures off *QuotaExhaustedError with errors.As.
//
// It lives in internal/ingest (which internal/mailsync already imports) rather
// than in internal/api, because the ingest path may not import internal/api
// (layering — api imports ingest). The allow/deny rule itself is the shared
// store.QuotaAdmits, so this pause and the request-time overQuota refusal decide
// identically.
var ErrQuotaExhausted = errors.New("ingest: paused: over the storage quota")

// QuotaExhaustedError carries the storage figures behind an ErrQuotaExhausted so
// the sync runner can record a meaningful account-health reason. It satisfies
// errors.Is(err, ErrQuotaExhausted).
type QuotaExhaustedError struct {
	Used  int64
	Limit int64
}

func (e *QuotaExhaustedError) Error() string {
	return fmt.Sprintf("paused: over the storage quota (%d of %d bytes used)", e.Used, e.Limit)
}

func (e *QuotaExhaustedError) Is(target error) bool { return target == ErrQuotaExhausted }

// Provenance describes where a raw message physically came from.
type Provenance struct {
	AccountID    ulid.ULID
	MailboxID    ulid.ULID
	UID          int64
	UIDValidity  int64
	RemoteFlags  []string  // IMAP flags/keywords as seen on the server
	InternalDate time.Time // server INTERNALDATE (earliest wins as received_at)
	GmMsgID      string    // optional X-GM-MSGID
	// DateFromHeader is set when InternalDate was not evidence from the provider
	// or the importer but the sender-controlled Date header (or now). Such a time
	// is good enough for received_at, which is NOT NULL, but must not stamp a
	// contact's mail dates (#829).
	DateFromHeader bool
	// Gmail is the X-GM-EXT-1 reading of this copy (#52, ADR-047): its labels,
	// message id and thread id. Nil for every non-Gmail account. When set,
	// the labels are applied in the SAME transaction as the ingest or flag
	// refresh (store.ApplyGmailObservation), so the Email change_log row lands
	// with the mutation.
	Gmail *GmailReading
}

// GmailReading is what X-GM-EXT-1 says about one physical copy.
type GmailReading struct {
	MsgID  uint64
	ThrID  uint64
	Labels []string // decoded X-GM-LABELS; never nil for a real reading
}

// Archive describes a message ingested from a LOCAL ARCHIVE — an mbox, a
// maildir, a Google Takeout (spec §4 bulk import, internal/importer) — rather
// than from an IMAP server.
//
// The difference that matters is NOT the file format: it is that such a message
// has NO IMAP PROVENANCE and never will. There is no (mailbox, uidvalidity, uid)
// coordinate for it, because no server ever assigned one. That single fact
// decides three things, and IngestArchive exists to make all three structural
// rather than remembered:
//
//   - MEMBERSHIP comes from app.messages.local_mailbox_id, the same column a
//     not-yet-APPENDed draft uses (store/jmap_email.go effectiveMembershipSQL,
//     branch 2), because there is no provenance row to derive it from.
//   - WRITEBACK NEVER FIRES for it. Every Email writeback intent is enqueued once
//     per LIVE ACCOUNT, and the live account set is read from message_provenance
//     (store/jmap_email.go liveAccountIDs). No provenance ⇒ no accounts ⇒ no
//     intents — today, and for every future Email/set the user performs on it.
//     Imported mail is local-only mail; it must never be pushed at somebody's
//     IMAP server.
//   - THE EXPUNGE REAPER CANNOT TOUCH IT. The reaper only ever runs against a
//     mailbox it has just polled, and it explicitly refuses to soft-delete a
//     message that has a local placement (store/reaper.go reapMessage, `!hasLocal`).
type Archive struct {
	// MailboxID is the local mailbox the archive folder maps to. Required.
	MailboxID ulid.ULID
	// ReceivedAt is the archive's best evidence of delivery time (the mbox
	// `From ` line, the maildir filename/mtime). Zero falls back to the Date
	// header, then to now.
	ReceivedAt time.Time
	// Flags are IMAP-style flag tokens recovered from the archive (maildir
	// `:2,S`, an mbox `Status:` header, Gmail's Unread/Starred pseudo-labels).
	// They are mapped by the same mapFlags the sync path uses.
	Flags []string
	// LabelIDs are Mailroom labels to attach (Gmail labels, from X-Gmail-Labels).
	LabelIDs []ulid.ULID
}

// Result reports the outcome of an Ingest call.
type Result struct {
	MessageID   ulid.ULID
	ThreadID    ulid.ULID
	ContentHash [32]byte
	Created     bool // true if a new canonical message was created (vs deduped)

	// Muted is set on a NEW message (Created) that arrived in the inbox of a
	// conversation the user has muted and was therefore placed in the account's
	// Archive — in Mailroom only, unread, no writeback (see divertIfMuted). The
	// sync engine counts these so the new-mail push can leave them out: a muted
	// conversation must not wake the user's phone. Never set on a dedup hit,
	// on bulk archive import, or when the reply stayed in the inbox (no Archive
	// mailbox, or it did not arrive in the inbox at all).
	Muted bool

	// Blocked is set on a NEW message from a blocked sender (#59), live path
	// only. Like Muted it keeps the message out of the new-mail push — the user
	// blocked the sender, so a phone must not buzz for them — and that holds
	// even when the message could not be moved (it arrived outside the inbox,
	// or the account has no Junk/Trash folder; see moveBlockedArrival). Never
	// set on a dedup hit or on bulk archive import.
	Blocked bool

	// LeftAlone is set only by IngestArchive: the archived bytes deduplicated
	// onto a message that IMAP SYNC ALREADY OWNS (it has live provenance), so the
	// archive copy adds nothing and the message was not touched at all. See
	// IngestArchive for why touching it would be actively harmful.
	LeftAlone bool

	// AwaitingExpunge is set when NOTHING was recorded because this copy is one
	// a queued draft EXPUNGE is about to remove from this very mailbox (#686,
	// store.DraftsCopyAwaitingExpunge). MessageID is zero and no row of any kind
	// was written. The caller must not count it as ingested: the server still
	// lists the UID until the expunge runs, and the mailbox's view is complete
	// only once that copy is gone — or, if the expunge gives up, once it is
	// ingested after all.
	AwaitingExpunge bool

	// Gmail is what applying Provenance.Gmail changed (zero when it was nil).
	Gmail store.GmailApplyResult
}

// Ingestor ingests raw messages into the canonical store + blob store.
type Ingestor struct {
	store *store.Store
	blobs blob.BlobStore
	// scorer is the T1 priority scorer (#33); nil disables scoring — the common
	// case for non-serving Ingestors (bulk import, the CLIs, the sync tests).
	scorer *ml.Scorer
	// classifier runs the per-label models (#169); nil disables categorisation,
	// on the same terms as scorer.
	classifier LabelClassifier
	// scanner is the malware scanner (#394, ADR-027); nil disables scanning, on
	// the same terms again — rows then keep app.attachments' 'unscanned'
	// default, which is a reported state rather than a silent one (§5d).
	scanner Scanner
	// breaker is the per-message circuit breaker that keeps a file which CRASHES
	// the scanner from halting the account forever (ADR-027 §4b step 3). It is
	// on the Ingestor, not per-call, precisely because its whole job is to
	// remember across passes. See scanBreaker.
	breaker *scanBreaker
	// The cached signature-database version stamped onto verdicts (#407). On
	// the Ingestor for the same reason as breaker: ingest has no per-pass
	// object, and a socket round trip per message is what scanVersionTTL
	// exists to avoid. A nil version with a recent versionAt means the daemon
	// was asked and would not say.
	versionMu sync.Mutex
	version   *clamd.Version
	versionAt time.Time
	// trustedAuthservIDs are the RFC 8601 authserv-ids whose verdicts learned
	// placement believes (#714): the configured MAILROOM_AUTHSERV_IDS, the
	// same list the sender warnings read. Empty = believe none.
	trustedAuthservIDs []string
	// localDKIM is the local DKIM check at ingest (#776, ADR-036); nil disables
	// it. See localdkim.go.
	localDKIM *localDKIM
	// learnedObserve receives the counter reason of every learned-placement
	// arrival decision (#840); nil counts nothing. See WithLearnedPlacement.
	learnedObserve func(reason string)
	// prefetchMaxAge is the image prefetch's arrival window (#922); zero
	// disables queueing the prefetch. See WithImagePrefetch.
	prefetchMaxAge time.Duration
}

// WithImagePrefetch queues the image proxy's arrival prefetch (#922, ADR-026
// amendment of 2026-10-27) for each NEW message on the live path whose sender
// passes the proxy's own remote-content rule and which was received within
// maxAge. Only the job is queued here — one read of the grants and one INSERT,
// under a savepoint inside the ingest transaction; the fetching happens off the
// ingest path, in the job.
// Without it (bulk import, the CLIs, the tests) nothing is queued. maxAge <= 0
// is the same as not passing it.
func WithImagePrefetch(maxAge time.Duration) Option {
	return func(in *Ingestor) { in.prefetchMaxAge = maxAge }
}

// WithLearnedPlacement sets the sink for learned placement's arrival
// decisions (#840): observe receives one reason (store.LearnedPlacementLabels,
// a closed set) per decision, "placed" and "error" included, so an abstention
// the log never showed is countable. Without it ingest counts nothing.
func WithLearnedPlacement(observe func(reason string)) Option {
	return func(in *Ingestor) { in.learnedObserve = observe }
}

// Option configures an Ingestor.
type Option func(*Ingestor)

// WithScorer injects the T1 priority scorer (spec §8, #33). When set, the LIVE
// ingest path scores each new message and records the result (best-effort — a
// scoring failure never blocks delivery). It is an Option, not a required
// positional argument, so the many non-scoring callers (bulk import, the admin/
// drill CLIs, the sync integration tests) keep constructing an Ingestor
// unchanged; only cmd/mailroom wires a scorer.
func WithScorer(sc *ml.Scorer) Option {
	return func(in *Ingestor) { in.scorer = sc }
}

// WithClassifier injects the per-label classifier (spec §8, #169). When set, the
// LIVE ingest path categorises each new message: a confident label and an
// uncertain one BOTH queue a question for the user (differing only in where they
// sort), and confident rejections do nothing. Like WithScorer it is opt-in, so
// the many non-serving callers (bulk
// import, the admin CLIs, the sync tests) categorise nothing — a mass import
// must not run a model over a decade of mail, nor fill the review queue with a
// decade of questions.
func WithClassifier(c LabelClassifier) Option {
	return func(in *Ingestor) { in.classifier = c }
}

// WithTrustedAuthservIDs names the Authentication-Results identities whose
// verdicts learned placement may act on (#714, config.EnvAuthservIDs). Without
// it, learned placement acts only on mail whose authentication did not
// clearly fail (placement.SenderAuth.Authenticated).
func WithTrustedAuthservIDs(ids []string) Option {
	return func(in *Ingestor) { in.trustedAuthservIDs = ids }
}

// LabelClassifier is the per-label categorisation ingest depends on, named as an
// interface rather than taking *ml.Classifier directly.
//
// It is here so the SUGGEST-ONLY INVARIANT is testable. That path had no test at
// all — a change from "apply the label" to "record a suggestion" left the whole
// suite green — because a concrete *ml.Classifier cannot be made to return a
// chosen verdict without training a real model first. An interface makes
// "a confident verdict must not write a label" a two-line fake and a real
// assertion.
type LabelClassifier interface {
	Classify(ctx context.Context, userID ulid.ULID, labelIDs []ulid.ULID, facts ml.MessageFacts) map[ulid.ULID]ml.LabelVerdict
}

// New constructs an Ingestor. Pass WithScorer to enable priority scoring on the
// live path; without it, ingest is exactly as before (no scoring).
func New(s *store.Store, b blob.BlobStore, opts ...Option) *Ingestor {
	in := &Ingestor{store: s, blobs: b, breaker: newScanBreaker()}
	for _, o := range opts {
		o(in)
	}
	return in
}

// Ingest canonicalizes one raw RFC 5322 message for userID. dek is the user's
// unwrapped mail-blob data-encryption key and dekID its stored reference; both
// are resolved by the caller (key management lives outside this package). It
// deduplicates by content hash, records provenance, merges flags, threads the
// message, and appends the required change_log rows — all in one transaction.
func (in *Ingestor) Ingest(ctx context.Context, userID ulid.ULID, dek []byte, dekID ulid.ULID, raw []byte, prov Provenance) (Result, error) {
	sum := sha256.Sum256(raw)
	parsed, _ := mime.ParseBytes(raw) // best-effort: hostile input degrades, never fatal here
	proj := projectionFrom(parsed)

	// MALWARE SCAN, BEFORE THE TRANSACTION OPENS (#394, ADR-027 §4). The only
	// error this returns is *ScannerUnavailableError, and returning it here —
	// with nothing written and no transaction started — is what makes the
	// refusal fail-closed: the mail is still on the provider, the cursors are
	// never advanced, and mailsync skips the reaper for the whole account. See
	// scan.go for why per-file failures are verdicts rather than errors.
	//
	// ⚠ IT RUNS BEFORE WE KNOW WHETHER THE MESSAGE DEDUPLICATES, because finding
	// that out needs the transaction this deliberately precedes. A redelivery is
	// therefore re-scanned; clamd caches clean results by content hash, which is
	// what makes that cheap (53 ms against 366 ms, §4a). The alternative — a
	// second round trip to Postgres before the real one — would buy less than it
	// costs, and refusing a redelivery during an outage is the safe direction
	// anyway.
	//
	// ⚠ AND ON A DEDUP HIT THE VERDICT IS DISCARDED, which is a real gap named
	// rather than papered over. A message ingested BEFORE the scanner was
	// configured keeps its 'unscanned' rows even if the same bytes are delivered
	// again. Writing them here would be three lines and would fix a fraction of
	// a percent of such rows — which is exactly why it is not done: it would
	// leave "already-stored mail gets scanned" looking handled when it is not.
	// Coverage of existing mail is ADR-027 increment 4's re-scan tiers, which
	// need the scanned-at and DB-version columns this increment does not add,
	// and §5d's aggregate reports every 'unscanned' row until then.
	verdicts, scanStamp, err := in.scanMessage(ctx, sum, parsed)
	if err != nil {
		return Result{}, err
	}

	// LOCAL DKIM, BEFORE THE TRANSACTION OPENS, for the same reason as the scan
	// (#776, ADR-036): DNS latency is never held inside a Postgres transaction.
	// It runs before we know whether the message deduplicates; a redelivery is
	// re-checked and its verdict discarded (ingestExisting never writes one),
	// which is cheap because only mail with no trusted Authentication-Results
	// gets here at all, and bounded (3s a lookup, 4 lookups, 8s a message).
	// The outcome never fails the ingest.
	dkimOutcome := in.checkLocalDKIM(ctx, raw, parsed, proj.FromAddress, prov.InternalDate)

	res := Result{ContentHash: sum}
	err = in.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		// ⚠ BEFORE findExisting, AND THAT ORDER IS PART OF THE FIX (#686). A copy
		// whose removal is already queued is not mail, whether or not its bytes
		// match a row: on the draft-retire path they match nothing (the row is
		// gone) and would become a phantom draft; on the send path they match the
		// SENT message and would pull it back into Drafts with \Draft merged on.
		// It defers and never deletes — see DraftsCopyAwaitingExpunge for why the
		// scope cannot reach a copy that expunge would not itself remove.
		awaiting, err := tx.DraftsCopyAwaitingExpunge(ctx, prov.MailboxID, proj.MessageID)
		if err != nil {
			return err
		}
		if awaiting {
			res.AwaitingExpunge = true
			return nil
		}
		existingID, exists, err := in.findExisting(ctx, tx, sum, raw)
		if err != nil {
			return err
		}
		if exists {
			if err := in.ingestExisting(ctx, tx, existingID, prov, &res); err != nil {
				return err
			}
			return in.applyGmail(ctx, tx, res.MessageID, prov, &res.Gmail)
		}
		if err := in.ingestNew(ctx, tx, userID, dek, dekID, raw, sum, proj, parsed, prov, &res, nil, verdicts, scanStamp, dkimOutcome.row); err != nil {
			return err
		}
		if err := in.applyGmail(ctx, tx, res.MessageID, prov, &res.Gmail); err != nil {
			return err
		}
		// DELIVERY STATUS NOTIFICATIONS (spec §4, #30). A bounce is ingested like
		// any other message; recognizing and recording it happens HERE, in the
		// same transaction, and only for a brand-new canonical message. Tying it
		// to the ingestNew path is what makes it idempotent for free: a re-poll or
		// replay of the same DSN dedups on content hash to ingestExisting above and
		// is never re-recorded. It is strictly best-effort — a malformed DSN must
		// never fail the ingest of the message carrying it (see maybeRecordDSN).
		in.maybeRecordDSN(ctx, tx, res.MessageID, parsed)
		in.maybeQueueImagePrefetch(ctx, tx, &res, proj, parsed, prov)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	// Counted only for a message this call CREATED, and after the commit, so a
	// redelivery or a rolled-back transaction is not counted as a check.
	if res.Created && dkimOutcome.label != "" && in.localDKIM.observe != nil {
		in.localDKIM.observe(dkimOutcome.label)
	}
	return res, nil
}

// maybeQueueImagePrefetch queues the image prefetch for a message this call
// created (#922). STRICTLY FAIL-SOFT: the store runs it under a savepoint and an
// error is logged and swallowed, so it can never cost the mail it is about.
//
// Only a NEW message (a redelivery was queued, or deliberately not, when it
// first arrived), never a blocked sender's (they are not mail the user wants
// shown), and only inside the arrival window — a first sync of years of mail
// queues nothing for the old part of it. An unknown received time is treated
// as outside the window: guessing "now" would prefetch history.
//
// It also requires the sender to be AUTHENTICATED by the verdict learned
// placement acts on (LearnedSender — one function, so the two cannot drift)
// and not failing, and the arrival mailbox not to be junk (the store checks
// that, under the same savepoint as the grant read). The job checks both again
// when it runs.
func (in *Ingestor) maybeQueueImagePrefetch(ctx context.Context, tx *store.UserTx, res *Result, proj projection, parsed *mime.Message, prov Provenance) {
	if in.prefetchMaxAge <= 0 || !res.Created || res.Blocked || res.AwaitingExpunge {
		return
	}
	now := time.Now()
	received := prov.InternalDate
	if received.IsZero() || received.Before(now.Add(-in.prefetchMaxAge)) {
		return
	}
	if parsed == nil {
		return
	}
	sender := in.learnedSender(proj, parsed, received)
	if _, _, ok := sender.Keys(); !ok || sender.Ambiguous() || sender.Auth().AuthFail || !sender.Auth().Authenticated() {
		return
	}
	if _, _, err := tx.EnqueueProxyPrefetchIfAllowed(ctx, res.MessageID, proj.FromAddress, prov.MailboxID, now); err != nil {
		slog.WarnContext(ctx, "ingest: queueing the image prefetch failed; the message is stored and its images load when opened",
			"message_id", res.MessageID.String(), "err", err)
	}
}

// maybeRecordDSN recognizes a delivery status notification among freshly
// ingested mail and records it as delivery health linked to the message we sent
// (spec §4, #30). It is deliberately total: it never returns an error and never
// propagates one, because a DSN we cannot parse or record must not disturb the
// ingest of the message that carried it. dsn.Parse is panic-safe and
// store.RecordDSN runs every write under a SAVEPOINT, so a hostile DSN is
// contained at both layers.
func (in *Ingestor) maybeRecordDSN(ctx context.Context, tx *store.UserTx, dsnMessageID ulid.ULID, parsed *mime.Message) {
	if parsed == nil {
		return
	}
	report, ok := dsn.Parse(parsed)
	if !ok {
		return
	}
	recips := make([]store.DSNRecipient, 0, len(report.Recipients))
	for _, r := range report.Recipients {
		recips = append(recips, store.DSNRecipient{
			Recipient:  r.Address,
			Action:     r.Action,
			Status:     r.Status,
			Diagnostic: r.Diagnostic,
		})
	}
	_, _ = tx.RecordDSN(ctx, dsnMessageID, report.OriginalMessageID, recips)
}

// IngestArchive canonicalizes one raw message that came out of a LOCAL ARCHIVE
// (spec §4 bulk import). It is the SAME ingest — same content-hash identity, same
// blob, same threading, same change_log rows, in one transaction — differing only
// in that the message is placed by local_mailbox_id and gets NO provenance row
// (see Archive for why that is the whole point).
//
// Idempotence is the content hash, exactly as for IMAP ingest: re-running an
// interrupted import re-presents bytes already stored and they deduplicate.
//
// The one case that needs judgment is a dedup hit onto a message the IMAP SYNC
// owns — very likely when the operator imports a Takeout of the same mailbox they
// also sync. Then the archive knows nothing the server has not already told us,
// and the message is LEFT ENTIRELY ALONE: no flag merge (the archive's flags are a
// years-old snapshot and merging them would silently re-mark live mail), and above
// all no label attach, because a label attach on a message that HAS provenance
// would enqueue one add-keyword writeback intent per live account — i.e. a bulk
// import would push millions of keyword writes at the user's real mail server.
func (in *Ingestor) IngestArchive(ctx context.Context, userID ulid.ULID, dek []byte, dekID ulid.ULID, raw []byte, arc Archive) (Result, error) {
	if arc.MailboxID == (ulid.ULID{}) {
		return Result{}, errors.New("ingest: archive ingest requires a local mailbox")
	}
	sum := sha256.Sum256(raw)
	parsed, _ := mime.ParseBytes(raw) // best-effort: hostile input degrades, never fatal here
	proj := projectionFrom(parsed)

	// received_at is NOT NULL, and an archive is often the only witness of when a
	// message arrived: prefer the archive's own evidence (the mbox `From ` line,
	// the maildir filename), fall back to the Date header, and only then to now.
	prov := Provenance{InternalDate: arc.ReceivedAt, RemoteFlags: arc.Flags}
	if prov.InternalDate.IsZero() {
		prov.DateFromHeader = true
		switch {
		case proj.SentAt != nil:
			prov.InternalDate = proj.SentAt.UTC()
		default:
			prov.InternalDate = time.Now().UTC()
		}
	}

	// The same pre-transaction scan the IMAP path runs (ADR-027 §4, §11). An
	// import is exactly where unscanned mail would otherwise enter: the archive
	// is years of mail nothing has ever looked at. Rows this pass cannot scan
	// land 'unscanned', but the server's re-scan job does pick those up later —
	// a never-scanned row counts as stale, and one with no content digest is
	// due whatever its age (rescanDueSQL in internal/store/rescan.go) — just
	// not before the mail lands, and only on a server that has a scanner. The
	// importer stops on ErrScannerUnavailable rather than burning its
	// consecutive-failure budget blaming the database (importer.go).
	verdicts, scanStamp, err := in.scanMessage(ctx, sum, parsed)
	if err != nil {
		return Result{}, err
	}

	res := Result{ContentHash: sum}
	err = in.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		existingID, exists, err := in.findExisting(ctx, tx, sum, raw)
		if err != nil {
			return err
		}
		if exists {
			res.MessageID = existingID
			sets, err := tx.LiveProvenanceFlags(ctx, existingID)
			if err != nil {
				return err
			}
			if len(sets) > 0 {
				res.LeftAlone = true
				return nil
			}
			// A message we imported on an earlier (interrupted) pass. Re-attaching
			// its labels is idempotent and closes the crash window between creating
			// the message and labelling it.
			return in.attachLabels(ctx, tx, existingID, arc.LabelIDs)
		}
		mailbox := arc.MailboxID
		if err := in.ingestNew(ctx, tx, userID, dek, dekID, raw, sum, proj, parsed, prov, &res, &mailbox, verdicts, scanStamp, nil); err != nil {
			return err
		}
		return in.attachLabels(ctx, tx, res.MessageID, arc.LabelIDs)
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// attachLabels puts an imported message into its Gmail labels, in the ingesting
// transaction.
//
// It goes through ApplyEmailUpdate — the ONE canonical Email/set write path —
// rather than reaching into app.message_labels, so the label rows, the keyword
// projection and the Email change_log row stay consistent with what a client-side
// Email/set would have produced. That is safe here, and only here, precisely
// because the message has no provenance: ApplyEmailUpdate enqueues its writeback
// intents once per LIVE ACCOUNT (store/jmap_email.go liveAccountIDs), the account
// set of a provenance-less message is empty, and every enqueue loop therefore runs
// zero times. Callers must never point this at a message with provenance —
// IngestArchive checks that before it gets here.
func (in *Ingestor) attachLabels(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, labelIDs []ulid.ULID) error {
	if len(labelIDs) == 0 {
		return nil
	}
	if _, _, err := tx.ApplyEmailUpdate(ctx, msgID, store.EmailUpdate{AddLabelIDs: labelIDs}); err != nil {
		return err
	}
	return nil
}

// ingestExisting handles a dedup hit: a new physical location and/or flag
// change for a message already in the store.
func (in *Ingestor) ingestExisting(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, prov Provenance, res *Result) error {
	res.MessageID = msgID
	res.Created = false

	// nowLive is "this message has a live physical location here that it did not
	// have a moment ago" — a brand-new (mailbox, uidvalidity, uid), or a coordinate
	// that had been retired by a UIDVALIDITY roll and that the server is serving
	// again (store.UpsertProvenance). Both are membership changes; an ordinary
	// re-poll of an already-live row is not.
	nowLive, err := tx.UpsertProvenance(ctx, provenanceRow(msgID, prov))
	if err != nil {
		return err
	}

	// RETRACTION (mailroom#86, #88). Mailroom soft-deletes a message it finds in no
	// location at all — the expunge reaper when the server stops listing its UID,
	// the roll reconciler when a recreated mailbox does not bring it back — always
	// inferring "no live provenance anywhere ⇒ gone everywhere". That inference
	// races with another client MOVING a message between folders: the destination
	// can be polled before the move and the source reconciled after it, and for that
	// window the message is legitimately in no folder we know of. A NEWLY LIVE
	// provenance row is proof the inference was wrong — the message is right there
	// on the server — so it is withdrawn. RetractVanished's own guard (vanished_at
	// IS NOT NULL) means this can only ever undo MAILROOM's conclusion: a message
	// the user trashed is never resurrected by syncing the IMAP Trash folder it now
	// lives in.
	var retracted bool
	if nowLive {
		if retracted, err = tx.RetractVanished(ctx, msgID); err != nil {
			return err
		}
	}

	_, err = in.mergeAndRecord(ctx, tx, msgID, nowLive || retracted)
	return err
}

// mergeAndRecord merges a message's live provenance flag sets into its
// canonical flags and appends the Email change_log row the load-bearing
// invariant requires, iff something a client can see actually moved.
//
// It is the ONE tail shared by ingestExisting (the full body dedup path) and
// RefreshFlags (the flags-only path, #523), so the two can never drift apart
// on what counts as a real change. membershipChanged carries what only the
// CALLER can know — a newly-live location or a retraction, neither of which
// touches flags but both of which are still an Email update — while `changed`
// here is purely about the flags this call itself just merged.
func (in *Ingestor) mergeAndRecord(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, membershipChanged bool) (bool, error) {
	sets, err := tx.LiveProvenanceFlags(ctx, msgID)
	if err != nil {
		return false, err
	}
	merged := mergeFlags(sets)
	changed, err := tx.UpdateMessageFlagsIfChanged(ctx, msgID, merged)
	if err != nil {
		return false, err
	}
	// A newly live location changes JMAP mailbox membership; a flag change changes
	// keywords; a retraction un-deletes. Any of them is an Email update the
	// change_log must carry — and exactly one row covers all of them.
	if membershipChanged || changed {
		if _, err := tx.AppendChange(ctx, store.ObjectEmail, msgID, store.OpUpdated); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// FlagRefresh is the outcome of RefreshFlags.
type FlagRefresh struct {
	Found     bool      // a live provenance row existed at the coordinate
	MessageID ulid.ULID // set when Found
	Changed   bool      // the canonical flags changed (and an Email change_log row was appended)
	// Gmail is what applying Provenance.Gmail changed (zero when it was nil).
	Gmail store.GmailApplyResult
}

// applyGmail applies a Gmail label reading to the message just ingested or
// refreshed at prov's coordinate, inside the caller's transaction (#52,
// ADR-047; store.ApplyGmailObservation). A nil reading is a no-op.
func (in *Ingestor) applyGmail(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, prov Provenance, out *store.GmailApplyResult) error {
	// A nil label list is NO READING (#52 review H1) and is never turned into
	// an empty one here or anywhere below.
	if prov.Gmail == nil || prov.Gmail.Labels == nil || msgID == (ulid.ULID{}) {
		return nil
	}
	r, err := tx.ApplyGmailObservation(ctx, msgID, store.GmailObservation{
		MailboxID: prov.MailboxID, UIDValidity: prov.UIDValidity, UID: prov.UID,
		MsgID: prov.Gmail.MsgID, ThrID: prov.Gmail.ThrID, Labels: prov.Gmail.Labels,
	})
	if err != nil {
		// STRICTLY FAIL-SOFT, like applyRules (#52 review H2): the labels
		// were applied under a savepoint (ApplyGmailObservation), so a label
		// that cannot be stored never blocks the message itself — which
		// would fail it on every pass and re-download it every time. The
		// reading is retried on the next sweep.
		slog.WarnContext(ctx, "ingest: applying Gmail labels failed; the message is stored without them",
			"message_id", msgID.String(), "err", err)
		return nil
	}
	*out = r
	return nil
}

// RefreshFlags applies a flags-only server observation to the message already
// living at prov's (mailbox, uidvalidity, uid) — no body, no parse, no scan, no
// blob access (#523). It is the projection of Ingest the CONDSTORE flag-
// reconcile pass uses instead of a full re-fetch, because that pass already
// knows the only thing that could have changed is flags.
//
// ⚠ SOUND ONLY BECAUSE OF ONE IMAP INVARIANT: under a fixed UIDVALIDITY, a
// UID's CONTENT is immutable (RFC 9051 §2.3.1.1) — a server may only reuse a
// UID after bumping UIDVALIDITY, which retires every row at the old one
// (MarkMailboxProvenanceStale) and forces a full re-fetch. So the live
// provenance row already sitting at this coordinate is already pointing at the
// right message; a flags-only server change can only ever be about flags,
// never about which message this is.
//
// found=false is deliberately NOT handled here — it falls back to the full
// Ingest/body path, which is what CREATES a brand-new message, REVIVES a
// stale_uidvalidity row, or lets RetractVanished undo a standing soft-delete.
// All three need the bytes (content-hash identity, FindThreadByHints,
// RetractVanished's message row) that only that path has; this function
// touches nothing when found is false.
func (in *Ingestor) RefreshFlags(ctx context.Context, userID ulid.ULID, prov Provenance) (FlagRefresh, error) {
	var out FlagRefresh
	err := in.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		msgID, found, err := tx.RefreshLiveProvenanceFlags(ctx, prov.MailboxID, prov.UIDValidity, prov.UID, nonNil(prov.RemoteFlags))
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		out.Found = true
		out.MessageID = msgID
		changed, err := in.mergeAndRecord(ctx, tx, msgID, false)
		if err != nil {
			return err
		}
		out.Changed = changed
		if err := in.applyGmail(ctx, tx, msgID, prov, &out.Gmail); err != nil {
			return err
		}
		out.Changed = out.Changed || out.Gmail.Changed
		return nil
	})
	if err != nil {
		return FlagRefresh{}, err
	}
	return out, nil
}

// ingestNew stores the blob and creates the canonical message, thread link,
// provenance, and change_log rows.
//
// localMailbox is nil for the IMAP path (the message gets a provenance row — its
// physical (mailbox, uidvalidity, uid) coordinate). It is non-nil for the ARCHIVE
// path (IngestArchive), where the message has no coordinate to record: it is
// placed by local_mailbox_id and gets NO provenance row. Everything else — blob,
// content hash, threading, flags, the Email/Thread change_log rows — is identical,
// which is the point: there is one ingest, not two.
func (in *Ingestor) ingestNew(ctx context.Context, tx *store.UserTx, userID ulid.ULID, dek []byte, dekID ulid.ULID, raw []byte, sum [32]byte, proj projection, parsed *mime.Message, prov Provenance, res *Result, localMailbox *ulid.ULID, verdicts []store.ScanVerdict, scanStamp *store.ScanStamp, localDKIM *store.LocalDKIM) error {
	ch, err := in.blobs.Put(ctx, userID, dek, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("ingest: store blob: %w", err)
	}
	if ch != blob.ContentHash(sum) {
		return fmt.Errorf("ingest: blob hash %s != message hash %x", ch, sum)
	}
	storedSize, err := in.blobs.StoredSize(ctx, userID, ch)
	if err != nil {
		return fmt.Errorf("ingest: blob stored size: %w", err)
	}

	// STORAGE QUOTA — THE MID-SYNC PAUSE (spec §10). A new message that would push
	// the user over their cap is refused HERE, in the ingesting transaction, BEFORE
	// any canonical/provenance/change_log row exists — the message is treated as if
	// never seen. Aborting the tx rolls back everything that would have committed;
	// the blob bytes already written to the object store are an unreferenced file GC
	// reclaims (same shape as the blob upload endpoint). Returning ErrQuotaExhausted
	// (not a generic error) is what the sync engine keys on to PAUSE the whole
	// account and leave this mail on the server — never a silent drop.
	//
	// The dedup branch (ingestExisting) never reaches here and adds no bytes, so a
	// message we already store is correctly not gated (store.QuotaAdmits admits a
	// delta <= 0 anyway). Delta is the STORED (ciphertext) size, matching what
	// StorageQuota sums (app.blobs.size_stored).
	limit, used, err := tx.StorageQuota(ctx)
	if err != nil {
		return err
	}
	if !store.QuotaAdmits(limit, used, storedSize) {
		return &QuotaExhaustedError{Used: used, Limit: limit}
	}

	if err := tx.InsertBlobMeta(ctx, store.BlobMeta{
		ContentHash: sum[:], SizePlain: int64(len(raw)), SizeStored: storedSize,
		DEKID: dekID, EnvelopeVer: 1,
	}); err != nil {
		return err
	}

	threadID, found, err := tx.FindThreadByHints(ctx, proj.MessageID, union(proj.References, proj.InReplyTo))
	if err != nil {
		return err
	}
	if !found && prov.Gmail != nil {
		// Gmail's own conversation id as a FALLBACK hint (#52, ADR-047 §2,
		// ADR-030): only when no Message-ID hint matched, and only within the
		// same account — X-GM-THRID is Gmail's, and means nothing across
		// accounts. It never overrides a header match.
		if threadID, found, err = tx.ThreadByGmailThrID(ctx, prov.AccountID, prov.Gmail.ThrID); err != nil {
			return err
		}
	}
	if !found {
		if threadID, err = tx.CreateThreadForMessage(ctx, proj.Subject, prov.InternalDate); err != nil {
			return err
		}
	}

	// THE ALIAS STORY (spec §5): work out which of the user's addresses this
	// message was actually delivered to, so a reply defaults to coming FROM that
	// exact address. The candidates come from the headers (a heuristic over
	// attacker-supplied data); the CHOICE is made by the identity store, which
	// only ever picks an address this user is verified to send as. A message that
	// resolves to nothing keeps its top candidate as a literal hint with no
	// identity attached — honest, and re-resolvable later if the user adds the
	// identity.
	addressed, identityID, err := in.deriveAddressedIdentity(ctx, tx, proj.Candidates)
	if err != nil {
		return err
	}

	msgID := store.NewULID()
	flags := mapFlags(prov.RemoteFlags)
	flags.Keywords = nonNil(flags.Keywords)
	if err := tx.InsertMessage(ctx, store.MessageRow{
		ID: msgID, ContentHash: sum[:], ThreadID: threadID,
		// Recorded on every new message so the NEXT delivery of it is recognised
		// (#206). Computed here from the same raw bytes rather than threaded in
		// from the caller, so no ingest path can forget it and quietly stop
		// participating.
		DedupHash: dedupOf(raw),
		Subject:   proj.Subject, Snippet: proj.Snippet, FromName: proj.FromName, FromAddress: proj.FromAddress,
		SentAt: proj.SentAt, ReceivedAt: prov.InternalDate, SizeBytes: int64(len(raw)),
		HasAttachments: proj.HasAttachments,
		MessageIDHint:  proj.MessageID, InReplyTo: first(proj.InReplyTo), ReferenceIDs: nonNil(proj.References),
		Flags: flags,

		AddressedAddress:    addressed,
		AddressedIdentityID: identityID,

		LocalMailboxID: localMailbox,

		// #157: persisted so the training path sees the same bulk signals the
		// serve path does. RecipientCount comes from the same To+Cc list the
		// contacts layer uses, so the two can never disagree about fan-out.
		ListID:          proj.ListID,
		ListUnsubscribe: proj.ListUnsubscribe,
		RecipientCount:  len(proj.Participants),
		AuthFail:        proj.AuthFail,

		// #156: the rest of kind.Facts (migration 00027), same reasoning.
		ListUnsubscribePost: proj.ListUnsubscribePost,
		AutoSubmitted:       proj.AutoSubmitted,
		Precedence:          proj.Precedence,
		FeedbackID:          proj.FeedbackID,
		DKIMDomains:         proj.DKIMDomains,
		ReturnPath:          proj.ReturnPath,

		// #228: the category-tab classifier's verdict, stored so Email/query can
		// filter by tab in SQL (ADR-022). NOT the message's tab — that is
		// COALESCE(mapping, this), with the mapping half read live at query time,
		// so re-pointing a label still moves its mail with no backfill.
		ClassifiedTab:     string(tabs.Classify(tabFactsFor(proj, len(proj.Participants))).Tab),
		ClassifierVersion: tabs.ClassifierVersion,
	}); err != nil {
		return err
	}
	// THE ATTACHMENT PROJECTION (#393, ADR-027 §3), in the SAME transaction as
	// the message it describes — so a message can never exist with its parts
	// unprojected, which is the state the whole coverage report exists to
	// detect.
	if err := tx.ReplaceAttachments(ctx, msgID, proj.Attachments); err != nil {
		return err
	}
	// AND ITS VERDICTS (#394, ADR-027 §4), in that same transaction. The scan
	// itself ran before the transaction opened — clamd latency must never be
	// held inside a Postgres transaction — but the row and the judgement about
	// it commit together, so no message is ever visible with parts nothing has
	// said anything about. verdicts is nil when no scanner is configured, and
	// SetScanVerdicts is then a no-op loop over nothing.
	//
	// ⚠ AND WITH THE STAMP THAT SAYS WHAT THE SCANNER KNEW (#407). This used to
	// go in unstamped, which made `attachment-report` count mail scanned seconds
	// ago as "never scanned" — the one field whose purpose is to make a stalled
	// backlog legible — and made every new message due for an immediate,
	// redundant re-scan on a dispatcher that `submit` shares. scanStamp is nil
	// only when the daemon would not report its version, which is honest: the
	// row then genuinely has no verdict age to claim.
	if err := tx.SetScanVerdicts(ctx, msgID, verdicts, scanStamp); err != nil {
		return err
	}
	// ITS LOCAL DKIM VERDICT (#776, ADR-036), when the check reached a terminal
	// result. Live IMAP path only (IngestArchive passes nil: an archive is
	// history, and its signatures have long expired). ⚠ NO change_log ROW OF
	// ITS OWN, and that is correct: this row is part of the message just
	// created, which InsertMessage announced with the Email 'created' row in
	// this same transaction, and it is never updated afterwards.
	if localDKIM != nil && localMailbox == nil {
		if err := tx.SetLocalDKIM(ctx, msgID, *localDKIM); err != nil {
			return err
		}
	}
	// CALENDAR INVITATIONS (#725): index an invitation or cancellation by its
	// event, so an earlier message's card can say it was updated or
	// cancelled. Fail-soft and on both paths — an imported mailbox's history
	// of invitations links up the same way a live one does.
	// ⚠ The received time a date-bounded authserv-id entry is judged at (#766)
	// is provider evidence only: an IMAP INTERNALDATE. An archive row
	// (localMailbox set) takes received_at from the archive or, failing that,
	// the sender's Date header, which a forger controls; it gets the zero time,
	// which trusts no bounded entry.
	trustTime := prov.InternalDate
	if localMailbox != nil {
		trustTime = time.Time{}
	}
	in.indexCalendarInvite(ctx, tx, msgID, parsed, trustTime)
	// AUTO-DERIVED CONTACTS (#37, spec §10a). In the SAME ingest transaction,
	// teach the address book who this message is from/to and which alias the
	// sender reached the user at. The store applies the me-exclusion and appends
	// a Contact change_log row only when client-visible state actually moves
	// (never on a pure re-sighting — see store.UpsertContactsFromMessage).
	if err := tx.UpsertContactsFromMessage(ctx, store.ContactSourceMessage{
		FromName:         proj.FromName,
		FromAddress:      proj.FromAddress,
		Recipients:       proj.Participants,
		AddressedAddress: addressed,
		// The message's own date, not the moment of ingest (#829): a bulk import of
		// old mail must not make every correspondent look like they wrote today.
		// TRUSTED SOURCES: the provider's INTERNALDATE (live IMAP) and the archive's
		// own received time (mbox From_ line, maildir). The Date header fallback is
		// sender-controlled, so it is passed as zero: a new contact gets now() and an
		// existing one keeps its dates (DateUntrusted).
		ReceivedAt:    prov.InternalDate,
		DateUntrusted: prov.DateFromHeader || prov.InternalDate.IsZero(),
		Draft:         flags.Draft,
	}); err != nil {
		return err
	}
	// REPLIES FROM ANY CLIENT (#842). A message SENT BY the user that answers an
	// inbound one is the strongest engagement signal the mailbox holds, and the
	// only seam that used to record it was EmailSubmission/set — so a reply
	// written anywhere else taught the priority model nothing (no reply events
	// recorded although sent messages existed). Both paths, like the contact
	// upsert above: an imported Sent folder is history the model should learn
	// from. Once per original, so the Sent copy of a Mailroom send that the
	// submission seam already recorded adds nothing. STRICTLY FAIL-SOFT under
	// its own savepoint. Operational data, so no change_log row.
	// The mailbox it is arriving into is the authorship evidence: a From header
	// naming the user proves nothing (a spoofer writes it), a Sent mailbox does.
	replySource := prov.MailboxID
	if localMailbox != nil {
		replySource = *localMailbox
	}
	if _, err := tx.RecordReplySignalsAtIngest(ctx, msgID, threadID, found, proj.FromAddress, proj.MessageID, replySource); err != nil {
		slog.WarnContext(ctx, "ingest: recording a reply signal failed; the message is stored without it",
			"message_id", msgID.String(), "err", err)
	}
	// AUTOCRYPT KEY DISCOVERY (#48, spec §10a). Runs immediately after the
	// contact upsert above, because it needs the contact row that upsert may
	// have just created.
	in.recordAutocryptKey(ctx, tx, proj, parsed, in.autocryptAuthenticated(proj, parsed, localDKIM, localMailbox == nil, trustTime))
	// T0 RULES ENGINE (spec §8, mailroom#32). Deterministic user rules evaluated
	// at ingest, on NEW canonical messages arriving from a LIVE sync only
	// (localMailbox == nil — bulk archive imports are never rule-filtered, so a
	// mass import can never trigger millions of rule actions). It runs HERE, BEFORE
	// the provenance row is written below, so the label/keyword/placement mutations
	// enqueue ZERO IMAP writeback (liveAccountIDs is empty at this point); they
	// mirror out later only if the user acts on them. It is strictly FAIL-SOFT and
	// MAIL-SAFE: a broken rule is rolled back to a savepoint and skipped, and the
	// message ingests normally — a rule can never abort the ingest or lose mail.
	// BLOCK SENDER (#59) is decided FIRST and, when it applies, INSTEAD OF
	// everything below: rules, mute, scoring, categorisation, learned placement
	// and the desk's area label. A blocked sender's mail is not mail the user
	// wants sorted, scored or placed, and a rule or a learned placement must
	// not be able to file it back into view. The move itself happens after the
	// provenance row exists (below), because unlike every write in this block
	// it is a REAL move that writes back to the server. Live path only, like
	// everything it replaces: a bulk import is history, not an arrival.
	blockDest := ""
	if localMailbox == nil {
		blockDest = in.blockedDestination(ctx, tx, proj.FromAddress)
	}
	if localMailbox == nil && blockDest == "" {
		in.applyRules(ctx, tx, msgID, proj, parsed, addressed, prov)
		// MUTE (spec §6, #51). A reply arriving in a muted conversation skips the
		// inbox — the same local placement a skipInbox rule makes, through the same
		// store function. Same gate as applyRules (localMailbox == nil): a bulk
		// archive import is history being loaded, not a reply arriving, so it is
		// never diverted. It runs AFTER applyRules and whether or not any rule
		// matched: if a rule already archived the message the placement is a no-op
		// (first placement wins), and the message is still reported as a muted
		// arrival so it does not notify. Only a JOINED thread can be muted — a
		// thread CreateThreadForMessage made a moment ago cannot — so a message
		// that started its own conversation costs no query at all.
		if found {
			res.Muted = in.divertIfMuted(ctx, tx, msgID, threadID, prov)
		}
		// T1 PRIORITY SCORING (spec §8, #33). Only the LIVE path scores — like
		// applyRules, a bulk archive import does not, so a mass import never runs
		// millions of scorings. It runs HERE, after applyRules and BEFORE the
		// provenance row below, and is strictly FAIL-SOFT: any error is swallowed,
		// so a scoring failure can never abort the ingest or lose mail. The score
		// rides along with the Email-create change_log row appended below (it is
		// operational data and appends no change_log row of its own).
		in.scoreMessage(ctx, tx, userID, msgID, proj, parsed, prov, addressed)
		// AUTOMATIC CATEGORISATION (spec §8, #169). Same placement and the same
		// contract as the two above: LIVE path only, before provenance, strictly
		// fail-soft. Additive by construction — it can apply a label or queue a
		// question, and it has no vocabulary for moving or archiving anything.
		in.classifyMessage(ctx, tx, userID, msgID, proj, parsed, addressed)
		// LEARNED PLACEMENT (#714, ADR-032). AFTER applyRules, so a rule's area
		// label is already on the message and learned placement leaves it
		// alone (it never acts over a label-decided area). BEFORE the desk's
		// own label below, so that once the learned area's own label is on,
		// the message resolves BY LABEL and applyAreaLabel writes no desk
		// label, exactly how a rule's label already beats the desk's. Live
		// path only, for applyAreaLabelAtIngest's reason. Same store write as
		// the desk (store.insertAreaOwnLabel), same change_log story (it rides
		// the Email `created` row below), and STRICTLY FAIL-SOFT.
		in.applyLearnedPlacementAtIngest(ctx, tx, msgID, in.learnedSender(proj, parsed, trustTime))
		// THE AREA'S OWN LABEL (#442 increment 3, "areas are labels"). Runs AFTER
		// classifyMessage — and after applyRules before it — because
		// areas_resolve.go's ResolveAreas checks the message's LABELS FIRST and
		// only falls back to its resolved tab: a rule's or a mapped label's own
		// area must win that resolution before the desk ever gets to write
		// anything, or the desk could write a label the labels-first rule would
		// then have overridden anyway. Runs BEFORE the provenance row below, for
		// the SAME reason applyRules/scoreMessage/classifyMessage do: no live
		// account exists yet at this point, so this enqueues ZERO IMAP writeback;
		// the label mirrors out later only if the user acts on the message.
		//
		// LIVE PATH ONLY, deliberately, like the three calls above it: a bulk
		// archive import must not label millions of messages by a tab verdict
		// nobody has looked at — mailroom-admin materialize-area-labels exists
		// for that mail precisely because it reports its numbers before writing
		// anything (CLAUDE.md: build the report before the fix).
		//
		// THE SAME STORE WRITE AS THE BACKFILL, not a second one:
		// ApplyAreaLabelForNewMessage and MaterializeAreaLabel both call the one
		// unexported store.applyAreaLabel (area_labels.go) and differ only in
		// whether a change_log row follows — applyrules.go's own rule: "never a
		// second write path". STRICTLY FAIL-SOFT, the same shape as the three
		// calls above: an error is logged and swallowed, never returned, so
		// resolving an area can never abort the ingest of the mail it is about.
		in.applyAreaLabelAtIngest(ctx, tx, msgID)
	}
	// If we joined an existing thread, its rollups grow (a Thread update). A
	// freshly created thread already reflects this message (a Thread create).
	if found {
		if err := tx.AddMessageToThread(ctx, threadID, prov.InternalDate); err != nil {
			return err
		}
	}
	if localMailbox == nil {
		if _, err := tx.UpsertProvenance(ctx, provenanceRow(msgID, prov)); err != nil {
			return err
		}
	}
	if blockDest != "" {
		in.moveBlockedArrival(ctx, tx, msgID, prov, blockDest)
		res.Blocked = true
	}
	if _, err := tx.AppendChange(ctx, store.ObjectEmail, msgID, store.OpCreated); err != nil {
		return err
	}

	res.MessageID = msgID
	res.ThreadID = threadID
	res.Created = true
	return nil
}

// deriveAddressedIdentity picks the first candidate reply-from address the user
// is actually verified to send as (spec §5). See DeriveAddressedIdentity, which
// it delegates to in full: the alias backfill (mailroom#177) re-runs this same
// decision over already-stored mail, and a backfill that could disagree with
// ingest would be worse than none — so there is ONE implementation, not two.
func (in *Ingestor) deriveAddressedIdentity(ctx context.Context, tx *store.UserTx, candidates []alias.Candidate) (string, *ulid.ULID, error) {
	return DeriveAddressedIdentity(ctx, tx, candidates)
}

// DeriveAddressedIdentity picks the first candidate reply-from address the user
// is actually verified to send as (spec §5). It returns the chosen address and
// its identity; when NOTHING is verified it returns the top candidate with a nil
// identity, so the derived alias is still recorded (a user who adds the identity
// later can have it re-resolved) but nothing is ever authorized by it.
//
// That "re-resolved later" is not hypothetical: it is exactly what happens when a
// catchall user finally provisions their domain_wildcard identity, at which point
// every address at the domain becomes verified and the winning candidate can
// change. mailroom-admin's alias backfill calls THIS function so the re-resolution
// is the identical decision ingest would make today.
func DeriveAddressedIdentity(ctx context.Context, tx *store.UserTx, candidates []alias.Candidate) (string, *ulid.ULID, error) {
	if len(candidates) == 0 {
		return "", nil, nil
	}
	si, ok, err := tx.MatchVerifiedAddress(ctx, alias.Addresses(candidates))
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return candidates[0].Address, nil, nil
	}
	id := si.IdentityID
	return si.FromAddress, &id, nil
}

// applyRules loads the user's enabled T0 rules, evaluates them against a facts
// projection of this message, and applies the resulting actions in the ingesting
// transaction. It is total: a failure to LOAD rules, or any per-action failure,
// is swallowed (per-action failures are contained by savepoints inside
// store.ApplyRuleActions), so rule evaluation can never fail the ingest of the
// message that triggered it — a broken rule never loses mail.
func (in *Ingestor) applyRules(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, proj projection, parsed *mime.Message, addressed string, prov Provenance) {
	rs, err := tx.RulesForEval(ctx)
	if err != nil || len(rs) == 0 {
		return
	}
	actions := rules.EvaluateAttributed(rs, messageFacts(proj, parsed, addressed, rs))
	if len(actions) == 0 {
		return
	}
	_ = tx.ApplyRuleActions(ctx, msgID, prov.AccountID, prov.MailboxID, actions)
}

// blockedDestination reports where new mail from this From address goes when
// its contact is blocked ("junk" or "trash"), or "" when it is not blocked.
// The match is by EXACT address (store.BlockedDestination); why a forged From
// is harmless here is spelled out in store/contact_flags.go — the worst it can
// do is send legitimate mail to Junk. STRICTLY FAIL-SOFT: a lookup error is
// logged and the message is treated as not blocked, so a block can never
// abort an ingest or lose mail.
func (in *Ingestor) blockedDestination(ctx context.Context, tx *store.UserTx, from string) string {
	dest, blocked, err := tx.BlockedDestination(ctx, from)
	if err != nil {
		slog.WarnContext(ctx, "ingest: blocked-sender lookup failed; the message is treated as not blocked", "err", err)
		return ""
	}
	if !blocked {
		return ""
	}
	return dest
}

// moveBlockedArrival moves a blocked sender's new message to Junk or Trash
// (store.MoveBlockedArrival — the ordinary move, writeback included) and
// reports whether it moved. STRICTLY FAIL-SOFT, like divertIfMuted: the store
// call runs under a savepoint, an error is logged and swallowed, and the
// message then stays where it arrived.
func (in *Ingestor) moveBlockedArrival(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, prov Provenance, dest string) bool {
	moved, err := tx.MoveBlockedArrival(ctx, msgID, prov.AccountID, prov.MailboxID, dest)
	if err != nil {
		slog.WarnContext(ctx, "ingest: moving a blocked sender's message failed; it stays where it arrived",
			"message_id", msgID.String(), "destination", dest, "err", err)
		return false
	}
	return moved
}

// divertIfMuted places a new message that arrived in a muted conversation's
// inbox into the account's Archive (store.DivertMutedArrival) and reports
// whether it is there. STRICTLY FAIL-SOFT, like applyRules beside it: the store
// call runs under a savepoint, and an error is logged and swallowed — the reply
// then ingests normally, in the inbox, and notifies. A mute can cost a
// notification the user did not want; it can never abort an ingest or lose mail.
func (in *Ingestor) divertIfMuted(ctx context.Context, tx *store.UserTx, msgID, threadID ulid.ULID, prov Provenance) bool {
	diverted, err := tx.DivertMutedArrival(ctx, msgID, threadID, prov.AccountID, prov.MailboxID)
	if err != nil {
		slog.WarnContext(ctx, "ingest: diverting a reply in a muted conversation failed; it stays in the inbox",
			"message_id", msgID.String(), "thread_id", threadID.String(), "err", err)
		return false
	}
	return diverted
}

// classifyMessage runs the per-label classifiers over a freshly delivered
// message (spec §8, #169). Two of the three verdict shapes produce a SUGGESTION —
// a confident yes and an abstention alike — and a confident rejection produces
// nothing at all. It NEVER applies a label.
//
// ⚠ THIS COMMENT USED TO SAY "confident labels are applied", and it was wrong for
// as long as the suggest-only change had been shipped: the inline comment 20
// lines below spells out why v.Apply stopped writing a label, and the code has no
// branch that could. A doc comment that contradicts its own body is worse than
// none — it is the version a reader trusts, and here it described the exact
// behaviour #156's invariant forbids. (Fixed with #242, which adds the OTHER
// producer of suggestion rows and made the disagreement impossible to keep
// ignoring.)
//
// ADDITIVE ONLY, and structurally so rather than by convention: the only write it
// can make is a suggestion row. There is no branch here that labels, moves,
// archives or trashes anything, so getting a category wrong costs a glance and
// never a lost message.
//
// STRICTLY FAIL-SOFT, like applyRules and scoreMessage: a nil classifier is a
// no-op (the common non-serving Ingestor), and every error is logged and
// swallowed. Classification can never abort an ingest or lose mail.
func (in *Ingestor) classifyMessage(
	ctx context.Context,
	tx *store.UserTx,
	userID, msgID ulid.ULID,
	proj projection,
	parsed *mime.Message,
	addressed string,
) {
	if in.classifier == nil {
		return
	}
	labels, err := tx.LabelsWithModels(ctx)
	if err != nil || len(labels) == 0 {
		return
	}

	verdicts := in.classifier.Classify(ctx, userID, labels, in.mlFacts(ctx, tx, proj, parsed, Provenance{}, addressed))
	for labelID, v := range verdicts {
		// THE MODEL SUGGESTS; IT DOES NOT APPLY (#156's own invariant).
		//
		// v.Apply used to write the label straight onto the message with
		// LabelByML attribution and no human anywhere in the path. That violated
		// the epic's stated rule — "nothing is applied without the blast radius
		// being visible first" — which the rules path honours (Induce reports
		// Collateral and a person accepts) and this path did not.
		//
		// It is not a hypothetical concern at this data volume. The promotion
		// gate's holdout is not sender-grouped, so a label with 8 messages from 4
		// senders scores ~0.99 by recalling senders it trained on and promotes,
		// while a label with 22 messages from 17 senders scores near chance and
		// never does. The labels that earn an automatic model are exactly the ones
		// a one-line `alias =` rule already solves; the label that would actually
		// need a model cannot get one. Auto-applying that is how a user concludes
		// the system is doing this to every message they have not read, and turns
		// it off.
		//
		// A confident verdict is therefore a HIGH-CONFIDENCE SUGGESTION: same row,
		// ordered first in the needs-a-category queue. Being wrong about an
		// ordering is cheap; being wrong about a decision is not. When the corpus
		// is large enough to calibrate against (#169's data floor), applying
		// automatically can be revisited — with a measurement behind it.
		if !v.Apply && !v.Abstain {
			// A confident rejection writes nothing: no label, and no question
			// either. Queuing "I am sure this is not Promotions" would bury the
			// real uncertainty under everything the model got right.
			continue
		}
		if err := tx.PutLabelSuggestion(ctx, store.LabelSuggestion{
			MessageID:    msgID,
			LabelID:      labelID,
			Confidence:   v.Confidence,
			ModelVersion: v.ModelVersion,
			// WHY, stored with the question (#176). ml.Decide has always filled
			// Reasons; until migration 00019 there was nowhere to put it, so it was
			// read and thrown away right here — which made the review surface's
			// explainability contract impossible to honour. A question whose
			// reasoning is unavailable is one the user can only accept or ignore.
			Reasons: suggestionReasons(ctx, v.Reasons),
			// STATED, not left to the column default (#242). This row's Confidence
			// is a model probability and the rule path's is an ordering rank, and
			// `source` is the only thing that tells them apart downstream; writing
			// it explicitly here means the classifier's claim about its own output
			// is in the code that makes it, not inherited from a DDL default that a
			// later migration could change under it.
			Source: store.SuggestionSourceModel,
		}); err != nil {
			slog.WarnContext(ctx, "ingest: recording a label suggestion failed",
				"message_id", msgID.String(), "label_id", labelID.String(), "err", err)
		}
	}
}

// applyAreaLabelAtIngest is the ingest half of #442 increment 3: see the call
// site's comment (above, in ingestNew) for WHY it runs where it does. It is a
// thin wrapper around store.UserTx.ApplyAreaLabelForNewMessage purely so the
// fail-soft log-and-swallow shape lives beside applyRules/scoreMessage/
// classifyMessage rather than inline in ingestNew's already-long body.
func (in *Ingestor) applyAreaLabelAtIngest(ctx context.Context, tx *store.UserTx, msgID ulid.ULID) {
	if _, err := tx.ApplyAreaLabelForNewMessage(ctx, msgID); err != nil {
		slog.WarnContext(ctx, "ingest: applying the area's own label failed",
			"message_id", msgID.String(), "err", err)
	}
}

// applyLearnedPlacementAtIngest wraps store.UserTx.LearnedPlacementForNewMessage
// in the fail-soft log-and-swallow shape of its neighbours: an error is
// logged and swallowed (the store ran it under a savepoint, so the ingest
// transaction is untouched), and learned placement can never abort the
// ingest of the mail it is about.
//
// A GATED decision is logged at WARN on every message it holds back: an area
// whose correction rate has switched learned placement off is a state the
// operator must be able to see without running the report.
func (in *Ingestor) applyLearnedPlacementAtIngest(ctx context.Context, tx *store.UserTx, msgID ulid.ULID, sender placement.Sender) {
	res, err := tx.LearnedPlacementForNewMessage(ctx, msgID, sender)
	if in.learnedObserve != nil {
		if err != nil {
			in.learnedObserve(store.LearnedLabelError)
		} else {
			in.learnedObserve(res.Label())
		}
	}
	switch {
	case err != nil:
		slog.WarnContext(ctx, "ingest: learned placement failed; the desk places the message",
			"message_id", msgID.String(), "err", err)
	case res.Reason == store.LearnedGated && res.AreaID != nil && res.Gate != nil:
		slog.WarnContext(ctx, "ingest: learned placement is stopped for this area by its correction gate",
			"message_id", msgID.String(), "area_id", res.AreaID.String(),
			"resolved", res.Gate.Resolved, "precision_lcb", res.Gate.LCB,
			"floor", placement.GateMinLCB)
	case res.Placed:
		slog.DebugContext(ctx, "ingest: learned placement filed the message",
			"message_id", msgID.String(), "area_id", res.AreaID.String(), "key_kind", string(res.Key.Kind))
	}
}

// learnedSender builds THE identity learned placement acts on (#714) for a
// message being ingested: LearnedSender on proj.FromAddress, the very value
// stored as messages.from_address and the first mailbox of
// parsed.Header.From().
func (in *Ingestor) learnedSender(proj projection, parsed *mime.Message, received time.Time) placement.Sender {
	return LearnedSender(proj.FromAddress, parsed, in.trustedAuthservIDs, received)
}

// suggestionReasons marshals a verdict's feature contributions for storage
// (#176), in the SAME shape as the triage row's top_features so both "why?"
// payloads decode through one renderer.
//
// NIL AND EMPTY ARE DIFFERENT HERE, and the distinction is carried, not
// flattened. ml.TopContributions returns a nil slice when the model is not an
// Explainer at all, and an empty non-nil slice when it explained and nothing
// cleared the noise floor. Those become SQL NULL ("no record of why") and '[]'
// ("looked, found nothing") respectively, which is what
// label_suggestions.reasons is nullable for — unlike triage_scores.top_features,
// which is NOT NULL DEFAULT '[]' and cannot say the first thing at all.
//
// So the length is deliberately NOT tested: json.Marshal renders a nil slice as
// `null` and an empty one as `[]`, which is exactly the mapping wanted.
//
// A marshal failure is LOGGED AND SWALLOWED, like every other failure on this
// path: losing the explanation for a suggestion must not cost the suggestion,
// and it must certainly not abort an ingest.
func suggestionReasons(ctx context.Context, reasons []ml.FeatureContribution) []byte {
	if reasons == nil {
		return nil
	}
	payload, err := json.Marshal(reasons)
	if err != nil {
		slog.WarnContext(ctx, "ingest: marshalling label-suggestion reasons failed; the suggestion is stored without them",
			"err", err)
		return nil
	}
	return payload
}

// scoreMessage computes the T1 priority score for a freshly delivered message
// and records it in app.triage_scores, in the ingesting transaction (spec §8,
// #33). It is strictly BEST-EFFORT and MAIL-SAFE, exactly like applyRules:
//
//   - a nil scorer (the common non-serving Ingestor) is a no-op;
//   - any error — scoring, or the upsert — is LOGGED and SWALLOWED, never
//     returned, so a scoring failure can NEVER abort the ingest or lose mail.
//
// The message is already inserted; the score is additive operational data that
// appends NO change_log row of its own and simply rides along with the
// Email-create change the caller appends next.
func (in *Ingestor) scoreMessage(ctx context.Context, tx *store.UserTx, userID, msgID ulid.ULID, proj projection, parsed *mime.Message, prov Provenance, addressed string) {
	if in.scorer == nil {
		return
	}
	score, modelVersion, topFeatures, err := in.scorer.ScoreMessage(ctx, userID, in.mlFacts(ctx, tx, proj, parsed, prov, addressed))
	if err != nil {
		slog.WarnContext(ctx, "ingest: priority scoring failed; message delivered unscored",
			"message_id", msgID.String(), "err", err)
		return
	}
	// topFeatures carries the per-feature contributions behind the score (the #34
	// explainability payload); the scorer returns the default empty array when the
	// model cannot explain itself, and the store coerces empty/nil the same way.
	if err := tx.UpsertTriageScore(ctx, msgID, score, modelVersion, topFeatures); err != nil {
		slog.WarnContext(ctx, "ingest: recording priority score failed; message delivered unscored",
			"message_id", msgID.String(), "err", err)
	}
}

// mlFacts builds the ml.MessageFacts for a live message from the in-hand ingest
// data — the SERVE-side counterpart to ml.StoreExtractor's replay-side build.
// Both feed the ONE ml.featuresFromFacts, so there is no train/serve hashing
// skew; only the FACTS sources differ (here the parsed headers give List-Id /
// List-Unsubscribe / the To+Cc count that are not projected onto the row).
//
// Addresses are normalized through the same store helpers the replay path uses,
// so the from:/dom:/alias: keys are byte-identical across the two paths. The
// sender's contact frequency is best-effort: a lookup error leaves the sender
// simply "unknown", never failing the (already best-effort) scoring.
func (in *Ingestor) mlFacts(ctx context.Context, tx *store.UserTx, proj projection, parsed *mime.Message, prov Provenance, addressed string) ml.MessageFacts {
	sender := store.NormalizeAddress(proj.FromAddress)
	facts := ml.MessageFacts{
		SenderAddress:   sender,
		SenderDomain:    store.AddressDomain(sender),
		Subject:         proj.Subject,
		ToCcCount:       len(proj.Participants),
		IsReplyInThread: len(proj.InReplyTo) > 0,
		HourOfDayUTC:    prov.InternalDate.UTC().Hour(),
		HasAttachments:  proj.HasAttachments,
		AddressedAlias:  store.NormalizeAddress(addressed),
	}
	if parsed != nil {
		facts.IsListMail = parsed.Header.Get("List-Id") != ""
		facts.HasListUnsubscribe = parsed.Header.Get("List-Unsubscribe") != ""
		// Authentication-Results is repeated per verifying hop; pass ALL values to
		// the (conservative) parser. It sets AuthFail only on a clear failure, so
		// mail that merely lacks auth headers is never penalized (spec §8).
		facts.AuthFail = ml.AuthResultsFail(parsed.Header.Values("Authentication-Results"))
	}
	rf := mapFlags(prov.RemoteFlags)
	facts.RemoteFlagged = rf.Flagged || rf.Junk
	// CORRESPONDENCE, not receipts (#157). The old code asked "is this sender in
	// the address book", which is true of everyone who has ever mailed you — so a
	// daily newsletter looked like the closest correspondent in the mailbox and
	// took the largest UP weight in the model. What counts is whether the USER has
	// written to them.
	if sent, received, err := tx.OutboundCountForAddress(ctx, proj.FromAddress); err == nil {
		facts.HasCorresponded = sent > 0
		facts.OutboundCount = sent
		facts.InboundCount = received
	}
	return facts
}

// messageFacts builds the DOM-free MessageFacts projection the rules evaluator
// reads. From/domain/subject/alias/recipients come from the already-computed
// projection; List-Id, List-Unsubscribe and any arbitrary headers the loaded
// rules reference come from the parsed header (panic-safe, best-effort).
func messageFacts(proj projection, parsed *mime.Message, addressed string, rs []rules.Rule) rules.MessageFacts {
	f := rules.MessageFacts{
		From:       proj.FromAddress,
		FromDomain: domainOf(proj.FromAddress),
		Subject:    proj.Subject,
		Alias:      addressed,
		Headers:    map[string]string{},
	}
	for _, p := range proj.Participants {
		if p.Address != "" {
			f.To = append(f.To, p.Address)
		}
	}
	if parsed != nil {
		f.ListID = parsed.Header.Get("List-Id")
		f.ListUnsubscribe = parsed.Header.Get("List-Unsubscribe")
		for _, name := range rules.HeaderNames(rs) {
			f.Headers[strings.ToLower(name)] = parsed.Header.Get(name)
		}
	}
	return f
}

// domainOf returns the lowercased domain part of an email address, or "".
func domainOf(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return ""
	}
	return strings.ToLower(addr[at+1:])
}

func provenanceRow(msgID ulid.ULID, prov Provenance) store.ProvenanceRow {
	return store.ProvenanceRow{
		MessageID: msgID, AccountID: prov.AccountID, MailboxID: prov.MailboxID,
		UID: prov.UID, UIDValidity: prov.UIDValidity, GmMsgID: prov.GmMsgID,
		RemoteFlags: nonNil(prov.RemoteFlags),
	}
}

// projection is the subset of parsed-message fields projected onto the message
// row (list views + threading + search feed).
type projection struct {
	Subject        string
	Snippet        string
	FromName       string
	FromAddress    string
	MessageID      string
	InReplyTo      []string
	References     []string
	SentAt         *time.Time
	HasAttachments bool
	// Attachments is the projected part list (#393, ADR-027 §3), derived from
	// the SAME parse as HasAttachments above so the flag and the rows cannot
	// disagree — which is exactly what attachment-report compares.
	Attachments []store.AttachmentRow
	// Candidates are the ordered reply-from addresses derived from the trace
	// headers (spec §5's alias story, with §4's forwarded-mail fallback).
	Candidates []alias.Candidate
	// Participants are the To + Cc recipients (address + display name), the
	// correspondent evidence the auto-derived contacts layer records (#37).
	Participants []store.ContactParticipant

	// The bulk signals, read off the headers here and PERSISTED on the row
	// (migration 00016, #157) so the training path sees the same facts the serve
	// path does. Before this they were parsed at serve time and thrown away, so
	// the model could never learn from them.
	ListID          string
	ListUnsubscribe string
	AuthFail        bool

	// The rest of kind.Facts (migration 00027, #156): six more header-only
	// signals with the identical shape, extracted by FunctionSignalsFromHeader
	// so a replay can judge a message from a sender it has never seen. See that
	// function and the migration for the NULL-semantics decision. Flat fields
	// here (rather than an embedded store.FunctionSignals), matching the #157
	// block above.
	ListUnsubscribePost string
	AutoSubmitted       string
	Precedence          string
	FeedbackID          string
	DKIMDomains         []string
	ReturnPath          string
}

func projectionFrom(m *mime.Message) projection {
	var p projection
	if m == nil {
		return p
	}
	p.Subject = m.Header.Subject()
	if from := m.Header.From(); len(from) > 0 {
		p.FromName = from[0].Name
		p.FromAddress = from[0].Address
	}
	p.Snippet = snippet(m.Text, m.HTML)
	p.MessageID = m.Header.MessageID()
	p.InReplyTo = m.Header.InReplyTo()
	p.References = m.Header.References()
	if d, ok := m.Header.Date(); ok {
		p.SentAt = &d
	}
	p.HasAttachments = len(m.Attachments) > 0
	p.Attachments = attachproj.Rows(m)
	p.Candidates, _ = alias.DeriveFrom(m.Header)
	p.Participants = participantsFrom(m)
	// Bulk signals, persisted rather than re-derived (#157). Authentication-Results
	// is repeated per verifying hop, so ALL values go to the conservative parser —
	// it sets AuthFail only on a clear failure, never on merely-absent auth.
	p.ListID = strings.TrimSpace(m.Header.Get("List-Id"))
	p.ListUnsubscribe = strings.TrimSpace(m.Header.Get("List-Unsubscribe"))
	p.AuthFail = ml.AuthResultsFail(m.Header.Values("Authentication-Results"))
	// The rest of kind.Facts (#156), through the ONE shared extraction — see
	// FunctionSignalsFromHeader.
	fs := FunctionSignalsFromHeader(m.Header)
	p.ListUnsubscribePost = fs.ListUnsubscribePost
	p.AutoSubmitted = fs.AutoSubmitted
	p.Precedence = fs.Precedence
	p.FeedbackID = fs.FeedbackID
	p.DKIMDomains = fs.DKIMDomains
	p.ReturnPath = fs.ReturnPath
	return p
}

// tabFactsFor assembles the category-tab classifier's input from a projection
// (#228, ADR-022).
//
// IT READS THE PROJECTED FIELDS, NOT THE HEADER, and that is the point rather
// than a convenience: store.MessageTabFactsAfter builds kind.Facts from exactly
// the columns these fields become, so ingest and replay cannot see a message
// differently. Re-parsing the header here would be a second extraction, which is
// the train/serve gap #157 was fixed to close.
//
// recipientCount is passed separately because the row derives it from the
// participant list rather than from the projection, and the two must not
// disagree about fan-out.
//
// HasCorresponded is deliberately never set: a tab decided partly by the user's
// own behaviour would forfeit sender-independence, which is what makes a tab
// right on a stranger's FIRST message.
func tabFactsFor(p projection, recipientCount int) kind.Facts {
	domain := ""
	if at := strings.LastIndex(p.FromAddress, "@"); at >= 0 && at < len(p.FromAddress)-1 {
		domain = strings.ToLower(strings.TrimSpace(p.FromAddress[at+1:]))
	}
	return kind.Facts{
		FromAddress:         p.FromAddress,
		FromDomain:          domain,
		Subject:             p.Subject,
		ListID:              p.ListID,
		ListUnsubscribe:     p.ListUnsubscribe,
		ListUnsubscribePost: p.ListUnsubscribePost,
		AutoSubmitted:       p.AutoSubmitted,
		Precedence:          p.Precedence,
		FeedbackID:          p.FeedbackID,
		DKIMDomains:         p.DKIMDomains,
		ReturnPath:          p.ReturnPath,
		RecipientCount:      recipientCount,
		IsReply:             len(p.InReplyTo) > 0,
	}
}

// participantsFrom collects the To and Cc recipients (address + display name)
// for the auto-derived contacts layer (#37). It reuses the same decoded address
// lists the alias derivation reads; the contacts store applies the me-exclusion
// and change-log discipline over them.
func participantsFrom(m *mime.Message) []store.ContactParticipant {
	var out []store.ContactParticipant
	add := func(as []mime.Address) {
		for _, a := range as {
			if a.Address == "" {
				continue
			}
			out = append(out, store.ContactParticipant{Address: a.Address, Name: a.Name})
		}
	}
	add(m.Header.To())
	add(m.Header.Cc())
	return out
}

const snippetLen = 200

// SnippetFromRaw parses raw RFC 5322 bytes and derives the list-view preview
// snippet from them, using the SAME generator (snippet) as live ingest so a
// backfill produces byte-identical previews and there is exactly one snippet
// definition. It is the exported entry point the mailroom-admin `resnippet`
// backfill uses to re-derive previews for mail ingested before the generator was
// fixed to strip HTML and fall back to the HTML body. mime.ParseBytes is tolerant
// by construction (it recovers panics into a truncated partial message rather
// than failing hard), so this returns a best-effort snippet for hostile input; a
// nil parse yields "".
func SnippetFromRaw(raw []byte) string {
	m, _ := mime.ParseBytes(raw)
	if m == nil {
		return ""
	}
	return snippet(m.Text, m.HTML)
}

// snippet builds the list-view preview from a message body. It ALWAYS runs the
// HTML stripper (htmlToText), even over the text/plain part, because some
// senders put markup — a full <style> block, tags — inside text/plain; those
// bytes must never reach the preview. It prefers the text/plain part, but when
// that yields nothing (HTML-only mail: no text/plain part, so text is empty) it
// derives the preview from the HTML part instead. The result is whitespace-
// collapsed and truncated to snippetLen runes.
func snippet(text, htmlBody string) string {
	s := collapseWhitespace(htmlToText(text))
	if s == "" {
		s = collapseWhitespace(htmlToText(htmlBody))
	}
	if len([]rune(s)) <= snippetLen {
		return s
	}
	return string([]rune(s)[:snippetLen])
}

// collapseWhitespace normalizes all runs of whitespace to single spaces and
// trims the ends — the list preview is a single line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// htmlToText extracts human-readable text from an HTML — or possibly-HTML —
// fragment for a preview snippet. It parses with x/net/html (the HTML5 tree
// algorithm, the same one internal/api/sanitize.go relies on) and walks the
// tree, and it deliberately does NOT emit the contents of subtrees the browser
// would never render as body text: <script>, <style>, <head>, <title>, and
// <noscript>. Their bytes (e.g. `.Roll2:hover{…}` from a <style> block) are
// dropped ENTIRELY rather than shown. Entity decoding is free: x/net/html has
// already decoded entities in the text nodes it hands back, so `&amp;`/`&#39;`
// arrive as `&`/`'`. Whitespace collapsing and truncation are the caller's job.
//
// Plain text (no tags) parses to a single text node and passes through
// unchanged apart from that entity decoding, which is harmless for a preview.
func htmlToText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		// x/net/html only errors on a malformed *context* node, which we never
		// pass; fall back to the raw bytes rather than losing the preview.
		return s
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.Script, atom.Style, atom.Head, atom.Title, atom.Noscript:
				// Non-rendered subtree: skip it and everything under it.
				return
			}
			// Block-level boundaries would otherwise jam adjacent cells/lines
			// together ("HelloWorld"); a leading space keeps words apart. It is
			// collapsed away by the caller wherever it was already whitespace.
			if isBlockElement(n.DataAtom) {
				b.WriteByte(' ')
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return b.String()
}

// isBlockElement reports whether an element introduces a text boundary in a
// rendered document — a place where adjacent text must not be concatenated.
// Inline elements (b, i, span, a, …) are intentionally absent so that
// "e<b>mail</b>" stays "email".
func isBlockElement(a atom.Atom) bool {
	switch a {
	case atom.P, atom.Div, atom.Br, atom.Hr, atom.Li, atom.Ul, atom.Ol,
		atom.Tr, atom.Td, atom.Th, atom.Table, atom.Thead, atom.Tbody, atom.Tfoot,
		atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Blockquote, atom.Pre, atom.Section, atom.Article,
		atom.Header, atom.Footer, atom.Aside, atom.Nav, atom.Dd, atom.Dt:
		return true
	}
	return false
}

// mapFlags maps IMAP flags/keywords to canonical MessageFlags. System flags
// become booleans; \Deleted/\Recent are lifecycle/transient and dropped;
// non-system tokens become keywords.
func mapFlags(remote []string) store.MessageFlags {
	f := store.MessageFlags{Keywords: []string{}}
	for _, fl := range remote {
		switch strings.ToLower(fl) {
		case `\seen`:
			f.Seen = true
		case `\flagged`:
			f.Flagged = true
		case `\answered`:
			f.Answered = true
		case `\draft`:
			f.Draft = true
		case `\deleted`, `\recent`:
			// lifecycle / transient — not canonical flags
		default:
			// The junk-verdict tokens are store's list (store.JunkKeywords,
			// NotJunkKeywords), shared with the raw-keyword backstop
			// store.KeywordReserved so the two cannot drift (#49 review).
			l := strings.ToLower(fl)
			if slices.Contains(store.JunkKeywords, l) {
				f.Junk = true
				continue
			}
			if slices.Contains(store.NotJunkKeywords, l) {
				// explicit not-junk marker; leave Junk false
				continue
			}
			if !strings.HasPrefix(fl, `\`) {
				f.Keywords = append(f.Keywords, fl)
			}
		}
	}
	return f
}

// mergeFlags folds the flag sets of every live provenance location into the
// canonical flags: a positive flag set anywhere wins; keywords are unioned.
func mergeFlags(sets [][]string) store.MessageFlags {
	merged := store.MessageFlags{Keywords: []string{}}
	seen := map[string]bool{}
	for _, set := range sets {
		f := mapFlags(set)
		merged.Seen = merged.Seen || f.Seen
		merged.Flagged = merged.Flagged || f.Flagged
		merged.Answered = merged.Answered || f.Answered
		merged.Draft = merged.Draft || f.Draft
		merged.Junk = merged.Junk || f.Junk
		for _, k := range f.Keywords {
			if !seen[k] {
				seen[k] = true
				merged.Keywords = append(merged.Keywords, k)
			}
		}
	}
	sort.Strings(merged.Keywords)
	return merged
}

// union concatenates a and b, de-duplicated, order-preserving.
func union(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// recordAutocryptKey stores the sender's OpenPGP public key when the message
// carries a usable Autocrypt header (#48, spec §10a). This is how a
// correspondent's key arrives without either party doing anything deliberate,
// and it is the route that gives encrypt/decrypt (#47) something to work with.
//
// ⚠ STRICTLY FAIL-SOFT. Every failure here — a header that must be ignored, a
// key that will not parse, a contact that cannot be resolved, a write that
// errors — leaves the message ingesting normally. A stranger's malformed header
// must never cost the user their mail, and this is decoration on an already
// complete ingest.
//
// ⚠ IT RUNS ON EVERY PATH, INCLUDING BULK ARCHIVE IMPORT, unlike applyRules and
// scoring. Those are excluded from imports because they take ACTIONS — labels,
// placements, writeback — and a mass import must not fire millions of them.
// This only records a fact that was already sitting in the mail, causes no
// writeback and no user-visible mutation, and an archive is exactly where a
// mailbox's existing correspondent keys are.
func (in *Ingestor) recordAutocryptKey(ctx context.Context, tx *store.UserTx, proj projection, parsed *mime.Message, authenticated bool) {
	if parsed == nil || proj.FromAddress == "" {
		return
	}
	values := parsed.Header.Values(autocrypt.HeaderName)
	if len(values) == 0 {
		return
	}

	peer, ok, err := autocrypt.Parse(values)
	if err != nil || !ok {
		// ErrIgnore is a normal outcome on hostile or old mail, not an incident.
		slog.DebugContext(ctx, "ingest: autocrypt header not usable",
			"from", proj.FromAddress, "headers", len(values), "err", err)
		return
	}

	// ⚠ THE ADDRESS MUST MATCH THE From. Without this a sender could attach a
	// key claiming to be somebody else's address, and a later encrypt-to-contact
	// would reach for the attacker's key when writing to that third party.
	// Autocrypt Level 1 requires the check for exactly this reason.
	if !peer.MatchesFrom(proj.FromAddress) {
		slog.WarnContext(ctx, "ingest: autocrypt header claims an address other than From — ignored",
			"from", proj.FromAddress, "claimed", peer.Addr)
		return
	}

	key, err := pgpkeys.Parse(peer.KeyData)
	if err != nil {
		// Includes the case worth naming: a correspondent who exported a SECRET
		// key by mistake. pgpkeys refuses it, and we must not store it.
		slog.WarnContext(ctx, "ingest: autocrypt key rejected",
			"from", proj.FromAddress, "err", err)
		return
	}

	contactID, found, err := tx.ContactIDForAddress(ctx, proj.FromAddress)
	if err != nil || !found {
		// Not found is possible on the me-exclusion path: a message from one of
		// the user's OWN addresses creates no contact, so there is nothing to
		// hang a key on. That is correct, not a miss.
		slog.DebugContext(ctx, "ingest: no contact for autocrypt key",
			"from", proj.FromAddress, "err", err)
		return
	}

	created, err := tx.UpsertContactKey(ctx, store.ContactKeyRow{
		ContactID:   contactID,
		Kind:        store.KeyPGP,
		Fingerprint: key.Fingerprint,
		PublicKey:   peer.KeyData,
		Source:      store.ContactKeyAutocrypt,
		// Whether the carrying message was authenticated for its From domain
		// decides whether this key may ever be encrypted to (#47, ADR-038).
		Authenticated: authenticated,
	})
	if err != nil {
		slog.WarnContext(ctx, "ingest: storing autocrypt key failed",
			"from", proj.FromAddress, "err", err)
		return
	}
	if created {
		slog.InfoContext(ctx, "ingest: learned a correspondent's PGP key from an Autocrypt header",
			"from", proj.FromAddress, "fingerprint", key.Fingerprint,
			"uid_covers_address", key.CoversAddress(proj.FromAddress))
	}
}

// autocryptAuthenticated reports whether the message that carries an Autocrypt
// header was AUTHENTICATED for its From domain, which is what makes the key it
// carries eligible for encryption (#47, ADR-038). Either:
//
//   - a DMARC pass BOUND TO THE FROM DOMAIN under the TOPMOST trusted
//     Authentication-Results (ml.TopmostTrustedAuthResults, judged at trustTime
//     by ml.TrustedAt) — the same decision learned placement acts on; or
//   - Mailroom's own local DKIM verdict: a pass that is aligned and displayable
//     (store.LocalDKIM.Aligned is false whenever DisplayBlocked is set), recorded
//     for a LIVE message only.
//
// ⚠ Anything else is false, including "no trusted authserv-id configured" and
// "no Authentication-Results at all": the Autocrypt header is then exactly as
// forgeable as the From line it sits beside, and a stranger could plant a key
// that the user's next encrypted mail to that address is readable with.
func (in *Ingestor) autocryptAuthenticated(proj projection, parsed *mime.Message, local *store.LocalDKIM, live bool, trustTime time.Time) bool {
	if parsed == nil || proj.FromAddress == "" {
		return false
	}
	domain := domainOf(proj.FromAddress)
	if domain == "" {
		return false
	}
	top := ml.TopmostTrustedAuthResults(parsed.Header.Values("Authentication-Results"),
		ml.TrustedAt(in.trustedAuthservIDs, trustTime))
	// STRICT alignment (#846): key eligibility for encryption is ADR-038's
	// decision, and relaxing it was not part of the change that relaxed
	// learned placement.
	if top.DMARCPassFor(domain, ml.DMARCStrict) {
		return true
	}
	return live && local != nil && local.Result == "pass" && local.Aligned && local.DisplayBlocked == ""
}

// Preflight answers, before any message is downloaded, whether ingest would
// refuse every new message right now (#52 review H2): the user is at their
// storage quota (ErrQuotaExhausted), or a scanner is configured and does not
// answer PING (ErrScannerUnavailable). The Gmail sync path asks it before a
// pass fetches any body, because there every downloaded byte is budget and a
// refused pass is retried within minutes. A PING failure here is read as a
// pause (fail closed on bandwidth), where scanMessage treats one as a
// possible blip: the cost of being wrong here is one skipped Gmail pass.
func (in *Ingestor) Preflight(ctx context.Context, userID ulid.ULID) error {
	var limit, used int64
	if err := in.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var err error
		limit, used, err = tx.StorageQuota(ctx)
		return err
	}); err != nil {
		return err
	}
	if limit > 0 && used >= limit {
		return &QuotaExhaustedError{Used: used, Limit: limit}
	}
	if in.scanner != nil {
		if err := in.scanner.Ping(ctx); err != nil {
			return &ScannerUnavailableError{Cause: err}
		}
	}
	return nil
}
