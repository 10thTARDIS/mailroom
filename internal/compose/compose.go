// SPDX-License-Identifier: AGPL-3.0-only

// Package compose authors outgoing mail: it builds the RFC 5322 bytes, derives
// the reply-from alias, records drafts and submissions in the canonical store,
// and sends (spec §5).
//
// Three seams, deliberately separate:
//
//   - derive.go — a PURE function over a received message's headers producing
//     ordered reply-from candidates (the catchall/alias story, spec §5 with §4's
//     forwarded-mail caveat). It proposes; it never authorizes.
//   - build.go — a PURE function producing the message bytes, with the
//     Message-ID generated under the sending identity's domain and correct
//     In-Reply-To/References/Date (spec §5).
//   - this file + send.go — the impure parts: the store, the blob store, SMTP.
//
// THE SECURITY BOUNDARY runs through Composer.Submit (specifically its
// mutation-free first half, PlanSubmit) and Sender.Send: both
// resolve the From address against the user's verified identities
// (store.ResolveSendIdentity) and refuse anything else, in the server, in the
// same transaction that records the submission (spec §5: "the server refuses
// any From address not matching a verified identity/domain for the authenticated
// user — even though the provider would also reject it"). A client cannot send
// as an address the user has not verified, and it cannot get there by lying
// about which identity it is using: the identity is re-derived from the From
// address the message actually carries.
//
// TRANSACTION DISCIPLINE: Composer's methods take the CALLER'S *store.UserTx.
// They are called from inside Email/set and EmailSubmission/set, which already
// hold one, and a nested second transaction would self-deadlock on the Email
// change_seq row the outer one has locked. Only the blob write is not a database
// operation, and it is a local content-addressed write (same shape as
// internal/ingest, which also writes the blob inside the ingesting transaction).
package compose

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/attachproj"
	"mailroom/internal/autocrypt"
	"mailroom/internal/blob"
	"mailroom/internal/mime"
	"mailroom/internal/pgpmime"
	"mailroom/internal/provider"
	"mailroom/internal/store"
)

// Keys resolves the per-user secrets the compose/send path needs. It is an
// interface so this package depends on neither the key hierarchy nor the
// account-credential envelope format; cmd/mailroom supplies the real one.
type Keys interface {
	// MailDEK returns the user's mail-blob data-encryption key and the id
	// recorded for it. The caller owns the key and must crypto.Zero it.
	MailDEK(ctx context.Context, userID ulid.ULID) (dek []byte, dekID ulid.ULID, err error)
	// SMTPAuth opens an account's sealed credentials and returns where and how
	// to submit: AUTH PLAIN to the account row's server for a password
	// account (PasswordLogin), XOAUTH2 with a current access token to the
	// PROVIDER's fixed server for an OAuth one (ADR-046 §1). It is called once
	// per send, immediately before it, and never cached.
	SMTPAuth(ctx context.Context, userID ulid.ULID, acct store.Account) (SMTPLogin, error)
}

// SMTPLogin is where and how one submission authenticates.
type SMTPLogin struct {
	Auth        provider.SMTPAuth
	Host        string
	Port        int
	ImplicitTLS bool
}

// PasswordLogin is a password account's login: the account row's server, with
// implicit TLS on 465 and STARTTLS otherwise — exactly what every send did
// before ADR-046.
func PasswordLogin(acct store.Account, username, password string) SMTPLogin {
	return SMTPLogin{Auth: provider.PasswordAuth(username, password),
		Host: acct.SMTPHost, Port: acct.SMTPPort, ImplicitTLS: acct.SMTPPort == 465}
}

// ErrNoDraftsMailbox / ErrNoSentMailbox are returned when the account has no
// mailbox with that RFC 6154 role. They are surfaced, never worked around: a
// guessed folder name is how mail ends up somewhere the user will never look.
var (
	ErrNoDraftsMailbox = errors.New("compose: account has no Drafts mailbox (SPECIAL-USE \\Drafts)")
	ErrNoSentMailbox   = errors.New("compose: account has no Sent mailbox (SPECIAL-USE \\Sent)")
)

// ErrNotVerified re-exports the store's From-validation failure so API callers
// can map it without importing the store's error directly.
var ErrNotVerified = store.ErrFromNotVerified

// ErrNotADraft is returned when a submission targets a message that is not a
// draft. Sending an arbitrary message out of the archive is not a send — it is
// a resend, and it is not what any client means by EmailSubmission/set.
var ErrNotADraft = store.ErrNotADraft

// ErrOverQuota is the sentinel record returns when storing a locally authored
// message (a draft, or the Sent copy) would exceed the user's storage quota. It
// is the compose-layer twin of the API's overQuota SetError: the caller
// (internal/api) maps it to a per-object setOverQuota error so Email/set create
// answers cleanly with nothing stored, rather than silently dropping the compose.
// Match it with errors.Is; read used/limit off *QuotaError with errors.As.
var ErrOverQuota = errors.New("compose: over the storage quota")

// QuotaError carries the storage figures behind an ErrOverQuota so the API layer
// can build the RFC 8620 overQuota SetError (used of limit) without re-reading
// the store. It satisfies errors.Is(err, ErrOverQuota).
type QuotaError struct {
	Used  int64
	Limit int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("compose: over the storage quota (%d of %d bytes used)", e.Used, e.Limit)
}

func (e *QuotaError) Is(target error) bool { return target == ErrOverQuota }

// DraftRequest is a compose request: the JMAP Email/set create for a draft,
// already parsed. From may be empty, in which case the account's default
// identity supplies it.
type DraftRequest struct {
	From    Address
	To      []Address
	Cc      []Address
	Bcc     []Address
	Subject string
	Text    string
	HTML    string

	// InReplyTo / References carry the threading context from the message being
	// replied to (RFC 8621 Email.inReplyTo / Email.references — the client reads
	// them off the parent with Email/get).
	InReplyTo  string
	References []string

	// KeepMessageID re-uses an existing Message-ID rather than generating one.
	// Set when re-saving a draft, so successive versions of the same draft stay
	// in the same thread instead of fragmenting it.
	//
	// ⚠ IT MUST NOT REACH THE JMAP DRAFT PATH, AND THAT IS NOT A STYLE
	// PREFERENCE — IT WOULD DESTROY THE DRAFT THE USER JUST SAVED (#311 item 4).
	//
	// internal/writeback's applyHeaderExpunge expunges EVERY UID its Message-ID
	// SEARCH returns. store.RetireDraft's contract promises the opposite — "its
	// edit is a DIFFERENT message ... it survives as a second draft" — and what
	// actually keeps that promise is that Build mints a fresh Message-ID per
	// revision, so revision N and N+1 never share one.
	//
	// Set this on the draft path and a create/destroy pair in a single Email/set
	// enqueues an APPEND of revision N+1 and a header-expunge of revision N with
	// the SAME Message-ID, in the same transaction — identical created_at, so
	// ClaimPendingIntents' ORDER BY created_at is a tie. If the APPEND applies
	// first, the SEARCH matches BOTH revisions and the expunge loop destroys the
	// one just saved.
	//
	// Wiring it in is a natural-looking change (JMAP clients like a stable
	// messageId across an edit), which is exactly why the reason is written here
	// rather than left to be rediscovered. internal/api/draft.go never sets it,
	// and TestJMAPDraftPathNeverKeepsTheMessageID keeps it that way.
	KeepMessageID string

	// Attachments are the parts to attach, already RESOLVED to their bytes by
	// the caller (internal/api resolves each JMAP blobId to bytes it has proven
	// the user owns — this package never sees a blobId and can therefore never
	// attach a blob nobody checked).
	Attachments []Attachment

	// Calendar makes the draft an iMIP message (see Draft.Calendar). Only the
	// invitation-reply path sets it (internal/api calendar.go); Email/set
	// cannot, so a client can never put a text/calendar part of its own
	// choosing into a message through it.
	Calendar *CalendarPart
}

// DraftResult reports what a saved draft became.
type DraftResult struct {
	EmailID     ulid.ULID
	MailboxID   ulid.ULID
	AccountID   ulid.ULID
	IdentityID  ulid.ULID
	MessageID   string
	ContentHash []byte
	Size        int64
}

// Signer makes a detached OpenPGP signature with a key the crypto process
// holds (ADR-024). It is an interface so this package never sees key material
// and never links the crypto client; cmd/mailroom supplies the real one.
//
// A nil Signer means signing is not configured, which is the ordinary case and
// is NOT an error: mail goes out unsigned, exactly as it did before #47.
type Signer interface {
	// Sign returns an ASCII-armored detached signature over data, and the
	// micalg naming the hash actually used. micalg comes back from the signer
	// because only the key's own algorithm preferences decide it.
	Sign(ctx context.Context, userID, keyID ulid.ULID, data []byte) (signature string, micalg string, err error)
}

// Composer authors drafts and submissions inside the caller's transaction.
type Composer struct {
	blobs blob.BlobStore
	keys  Keys
	// signer is the crypto process, or nil when signing is not configured.
	signer Signer
	// encrypter is the crypto process's encrypt verb, or nil (see encrypt.go).
	encrypter Encrypter
	// undoWindow is how long a submission sits cancelable before the job fires
	// (spec §5: Undo Send and Send Later are ONE mechanism — a durable
	// submission job with a fire-at time; "undo" cancels the job in the window).
	undoWindow time.Duration
}

// WithSigner returns c with an outgoing-mail signer attached. Separate from
// NewComposer so that every existing caller — and every test that does not care
// about PGP — keeps working unchanged and unsigned.
func (c *Composer) WithSigner(s Signer) *Composer {
	c.signer = s
	return c
}

// CanSign reports whether this server can sign outbound mail AT ALL — that a
// crypto process is wired, not that any particular identity has a key bound
// (#492).
//
// It exists so the JMAP session can advertise urn:mailroom:pgp {signing} off THE
// SAME FIELD prepareOutbound consults before signing. A separate "signing is
// enabled" config flag would be free to disagree with the composer that actually
// signs, and the disagreement would surface as a client promising a signature
// the message never gets.
//
// A nil receiver is a server wired with no composer at all: it cannot send, so
// it certainly cannot sign. Nil-safe here rather than a field read at the call
// site, because the caller (internal/api's session) legitimately holds nil.
func (c *Composer) CanSign() bool { return c != nil && c.signer != nil }

// DefaultUndoWindow is the hold time a send waits before it actually goes out.
const DefaultUndoWindow = 10 * time.Second

// NewComposer builds a Composer. A ZERO undoWindow is honored, not defaulted: it
// means "no hold — send as soon as the dispatcher picks it up", which is a
// legitimate operator choice (MAILROOM_UNDO_SEND_WINDOW=0s). Only a NEGATIVE
// window is a mistake — it would schedule the send in the past — and falls back
// to DefaultUndoWindow.
func NewComposer(blobs blob.BlobStore, keys Keys, undoWindow time.Duration) *Composer {
	if undoWindow < 0 {
		undoWindow = DefaultUndoWindow
	}
	return &Composer{blobs: blobs, keys: keys, undoWindow: undoWindow}
}

// SaveDraft builds the message bytes, stores them, records the canonical draft,
// and queues its APPEND to the account's IMAP Drafts folder (spec §5: drafts are
// local + IMAP). It runs entirely inside tx.
//
// The From address is VALIDATED here even for a draft: a draft is what a
// submission later sends, and validating only at send time would let a client
// stage an unsendable message and get a confusing failure minutes later, in a
// background job, instead of a clear error at the call that caused it.
func (c *Composer) SaveDraft(ctx context.Context, tx *store.UserTx, userID ulid.ULID, req DraftRequest) (DraftResult, error) {
	si, err := c.resolveFrom(ctx, tx, req.From.Email)
	if err != nil {
		return DraftResult{}, err
	}
	from := Address{Name: req.From.Name, Email: si.FromAddress}
	if from.Name == "" {
		from.Name = si.DisplayName
	}

	drafts, found, err := tx.MailboxForRole(ctx, si.AccountID, "drafts")
	if err != nil {
		return DraftResult{}, err
	}
	if !found {
		return DraftResult{}, ErrNoDraftsMailbox
	}

	built, err := Build(Draft{
		From: from, To: req.To, Cc: req.Cc, Bcc: req.Bcc,
		Subject: req.Subject, TextBody: req.Text, HTMLBody: req.HTML,
		InReplyTo: req.InReplyTo, References: req.References,
		MessageID:   req.KeepMessageID,
		Attachments: req.Attachments,
		Calendar:    req.Calendar,
	})
	if err != nil {
		return DraftResult{}, err
	}

	// Seen: true (#610). A draft the user is actively writing cannot be
	// "unread" — but every autosave mints a NEW revision (a fresh row, not
	// an update), so without this each one is born unread and inflates the
	// unread count; measured as is_seen = f on every draft in the corpus.
	// The Sent copy is appended \Seen for the identical reason (send.go's
	// EnqueueAppendIntent call), and the IMAP copy here carries \Seen too so
	// an ingest that later merges the remote flags agrees with us instead of
	// flipping it back to unread. The Gmail importer already files drafts as
	// \Draft \Seen (internal/importer/gmail_test.go) — this brings native
	// drafts in line with imported ones.
	id, err := c.record(ctx, tx, userID, built, from, si, drafts, localPlacement{
		Draft: true,
		Seen:  true,
		Flags: []string{`\Draft`, `\Seen`},
	})
	if err != nil {
		return DraftResult{}, err
	}
	sum := sha256.Sum256(built.Raw)
	return DraftResult{
		EmailID: id, MailboxID: drafts.ID, AccountID: si.AccountID,
		IdentityID: si.IdentityID, MessageID: built.MessageID,
		ContentHash: sum[:], Size: int64(len(built.Raw)),
	}, nil
}

// localPlacement is how a locally authored message is filed: the canonical draft
// flag, and the IMAP flags its APPEND carries.
type localPlacement struct {
	Draft bool
	Seen  bool
	Flags []string
}

// record stores the bytes, inserts the canonical message, and queues the APPEND.
func (c *Composer) record(ctx context.Context, tx *store.UserTx, userID ulid.ULID, built Built, from Address, si store.SendIdentity, target store.MailboxTarget, place localPlacement) (ulid.ULID, error) {
	dek, dekID, err := c.keys.MailDEK(ctx, userID)
	if err != nil {
		return ulid.ULID{}, err
	}
	defer zero(dek)

	hash, err := c.blobs.Put(ctx, userID, dek, bytes.NewReader(built.Raw))
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("compose: store blob: %w", err)
	}
	stored, err := c.blobs.StoredSize(ctx, userID, hash)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("compose: blob size: %w", err)
	}

	// Storage quota, checked in the SAME transaction as the write, against usage
	// read under RLS, BEFORE the canonical row (CreateLocalMessage inserts the
	// app.blobs row, so used excludes this message's bytes — delta is `stored`).
	// An over-quota compose records NOTHING: the bytes already written to the blob
	// store are just an unreferenced file GC reclaims, and the caller turns
	// ErrOverQuota into a clean overQuota SetError so the client learns the draft
	// was not saved — never a silent drop (spec §10, same rule as blob upload).
	limit, used, err := tx.StorageQuota(ctx)
	if err != nil {
		return ulid.ULID{}, err
	}
	if !store.QuotaAdmits(limit, used, stored) {
		return ulid.ULID{}, &QuotaError{Used: used, Limit: limit}
	}

	id, err := tx.CreateLocalMessage(ctx, store.LocalMessage{
		ContentHash: hash[:],
		SizePlain:   int64(len(built.Raw)),
		SizeStored:  stored,
		DEKID:       dekID,

		Subject:     built.Subject,
		Snippet:     snippet(built.Preview),
		FromName:    from.Name,
		FromAddress: from.Email,
		SentAt:      built.Date,
		SizeBytes:   int64(len(built.Raw)),

		MessageIDHint:  built.MessageID,
		InReplyTo:      built.InReplyTo,
		ReferenceIDs:   built.References,
		HasAttachments: built.HasAttachments,
		// ⚠ Derived from the BUILT BYTES by re-parsing them, not from
		// built.HasAttachments or the request's attachment list (#393). The
		// request says what the user asked for; the bytes are what exists. #377
		// is the cost of letting a byte-derived fact come from anywhere else,
		// and the part PATH can only come from a parse in any case.
		Attachments: attachproj.Rows(mustParse(built.Raw)),

		MailboxID: target.ID,
		AccountID: si.AccountID,
		Draft:     place.Draft,
		Seen:      place.Seen,
	})
	if err != nil {
		return ulid.ULID{}, err
	}

	// Mirror it to IMAP (spec §5: drafts local + IMAP; §4: Sent APPEND). The
	// engine dedups the append by Message-ID before writing (ADR-001), so a
	// provider that files its own copy never yields two.
	if err := tx.EnqueueAppendIntent(ctx, id, si.AccountID, target.IMAPName, place.Flags,
		built.Raw, hash[:], built.MessageID); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// resolveFrom applies the From-selection rule and THE SECURITY CHECK: an empty
// From falls back to the account's default identity, and any explicit From must
// resolve to a verified identity of this user or the call fails with
// ErrNotVerified. There is no path through this function that yields an
// unverified sender.
func (c *Composer) resolveFrom(ctx context.Context, tx *store.UserTx, from string) (store.SendIdentity, error) {
	if strings.TrimSpace(from) != "" {
		si, err := tx.ResolveSendIdentity(ctx, from)
		if err != nil {
			return store.SendIdentity{}, err
		}
		return si, nil
	}
	// No From given: the sole account's default identity.
	accounts, err := tx.ListAccounts(ctx)
	if err != nil {
		return store.SendIdentity{}, err
	}
	for _, a := range accounts {
		ident, found, err := tx.DefaultIdentityForAccount(ctx, a.ID)
		if err != nil {
			return store.SendIdentity{}, err
		}
		if !found {
			continue
		}
		if ident.Kind == store.IdentityWildcard {
			// A wildcard alone names no concrete address to send from; the
			// client must choose one (that is what the identity picker is for).
			continue
		}
		return store.SendIdentity{
			IdentityID: ident.ID, AccountID: ident.AccountID, Kind: ident.Kind,
			FromAddress: ident.Address, DisplayName: ident.DisplayName,
		}, nil
	}
	return store.SendIdentity{}, store.ErrFromNotVerified
}

// SubmitRequest is an EmailSubmission/set create, already parsed.
type SubmitRequest struct {
	// EmailID is the draft to send.
	EmailID ulid.ULID
	// IdentityID is the identity the client says it is sending as. It is
	// CHECKED against the identity the message's From address actually resolves
	// to — it is not trusted to select the sender.
	IdentityID ulid.ULID
	// EnvelopeFrom / EnvelopeTo override the envelope. Empty means "derive from
	// the message headers".
	EnvelopeFrom string
	EnvelopeTo   []string
	// IdempotencyKey makes this submit SAFE TO RETRY (#62). One key per queued client
	// action; a repeat with the same key returns the submission that already exists and
	// enqueues NO second job. Empty means "not retry-safe", which is what every caller
	// had before and is still right for a CLI send nothing will replay.
	IdempotencyKey string
	// SendAt schedules the send. Zero means "now + the undo window".
	SendAt time.Time
	// ArchiveOnSuccess is Send & archive (#648): messages to archive when, and
	// only when, this submission is actually SENT. Recorded on the submission;
	// the send worker performs it (SendWorker.archiveAfterSend). Not validated
	// beyond being ids: whether each is still this user's, still exists and is
	// still in the Inbox is decided at send time, when it matters.
	ArchiveOnSuccess []ulid.ULID
	// Encrypt asks for the message to be signed and then encrypted to every
	// To/Cc recipient and to the sender (#47, ADR-038). It is a REQUEST that can be
	// refused, never a hint: a send that asked for encryption and cannot have it
	// (no key for someone, a Bcc recipient, no crypto process) fails before
	// anything is written, and is NEVER downgraded to plaintext.
	Encrypt bool
	// APIKeyID is the API key making this send, nil for a person (#49):
	// recorded on the submission so the archive the worker performs after
	// the send is attributed to the key (store.ArchiveAfterSend).
	APIKeyID *ulid.ULID
}

// SubmitResult reports the scheduled submission.
type SubmitResult struct {
	SubmissionID ulid.ULID
	AccountID    ulid.ULID
	IdentityID   ulid.ULID
	SendAt       time.Time
	EnvelopeTo   []string
	// EncryptedTo is the key used for each recipient when the send was encrypted
	// (address and fingerprint; never key material). Empty otherwise.
	EncryptedTo []EncryptTo
}

// SubmitJobKind is the durable-job kind the send worker registers for.
const SubmitJobKind = "submit"

// PlannedSubmit is a send that has passed every check and has NOT BEEN RECORDED:
// PlanSubmit has run, SubmitPlanned has not, and nothing durable exists for it.
//
// It exists so a caller can refuse a send on a policy this package cannot
// evaluate and leave no trace — the case that forced it is ADR-028 §6's
// ingest-only mode, whose effective test is the account's own column ANDed with
// an INSTANCE-level flag that lives on *store.Store, not on the *store.UserTx
// this package is handed. The gate therefore has to sit in the caller, and a
// gate that can only run after Submit returns is not a gate: by then the message
// has been signed and its stored bytes rewritten (see SubmitPlanned).
//
// AccountID and IdentityID are exported because they are all such a caller needs
// to decide. Everything else is carried privately, so a plan cannot be assembled
// by hand, and the bytes that were validated are the bytes that get signed.
type PlannedSubmit struct {
	// AccountID is the account this send would go out on. It is resolved from
	// the From address IN THE MESSAGE BYTES, not from the client's identityId —
	// the same resolution the send itself uses, never a second copy of it.
	AccountID ulid.ULID
	// IdentityID is the verified identity that From resolved to.
	IdentityID ulid.ULID

	// planned distinguishes a real plan from a zero value. SubmitPlanned refuses
	// the latter rather than treating it as an empty send: all the validation is
	// in PlanSubmit, so accepting one would be a route into the send path with
	// no checks at all.
	planned bool
	req     SubmitRequest
	si      store.SendIdentity
	raw     []byte
	rcpt    []string
	// encrypt is the resolved recipient keys when req.Encrypt (nil otherwise).
	encrypt *encryptPlan
}

// Submit validates and schedules a send (spec §5's ONE delayed-submission
// engine: Undo Send and Send Later are the same durable job with a fire-at
// time). It runs inside tx, so the submission row, its change_log row, and the
// job that will send it either all exist or none do.
//
// It is PlanSubmit followed by SubmitPlanned, and exists as one call for every
// caller with nothing to decide in between. A caller that must refuse a send on
// a policy this package cannot see calls the two halves itself.
func (c *Composer) Submit(ctx context.Context, tx *store.UserTx, userID ulid.ULID, req SubmitRequest) (SubmitResult, error) {
	plan, err := c.PlanSubmit(ctx, tx, userID, req)
	if err != nil {
		return SubmitResult{}, err
	}
	return c.SubmitPlanned(ctx, tx, userID, plan)
}

// PlanSubmit runs every check a send must pass and WRITES NOTHING.
//
// Validation, in order, all server-side and all authoritative:
//  1. the Email exists and is a draft;
//  2. its From address resolves to a VERIFIED identity of this user;
//  3. the identity the client named is that identity (a client cannot borrow
//     one identity's authorization to send as another's address);
//  4. the envelope MAIL FROM, if the client supplied one, is that same address;
//  5. there is at least one recipient.
//
// ⚠ NOTHING ADDED HERE MAY MUTATE, and that is the whole point of the split.
// Every statement below is a SELECT (MessageRef, ResolveSendIdentity) and the
// only file touched is a blob READ. So a caller that plans a send and then
// refuses it has changed nothing AND left the transaction usable: no statement
// has errored, so the next object in the same /set batch still commits, with no
// SAVEPOINT needed. The first statement that would need one is SubmitPlanned's
// INSERT.
//
// The account is knowable this early because ResolveSendIdentity below yields
// it, and it runs before the first mutation (prepareOutbound's signing rewrite)
// rather than after. The split costs an extra call and no extra work: the bytes
// this reads are handed on rather than read twice.
func (c *Composer) PlanSubmit(ctx context.Context, tx *store.UserTx, userID ulid.ULID, req SubmitRequest) (PlannedSubmit, error) {
	ref, err := tx.MessageRef(ctx, req.EmailID)
	if err != nil {
		return PlannedSubmit{}, err
	}
	if !ref.IsDraft {
		return PlannedSubmit{}, ErrNotADraft
	}

	// The envelope is read from THE BYTES, not from the projection columns: the
	// bytes are the message (ADR-001), and a draft may have been composed by
	// another client and pulled in over IMAP, in which case Mailroom never saw a
	// compose request for it at all. Reading the blob is also what makes the
	// From check below a check on what will ACTUALLY be transmitted.
	raw, err := c.ReadRaw(ctx, userID, ref.ContentHash)
	if err != nil {
		return PlannedSubmit{}, err
	}
	env := ParseEnvelope(raw)
	if env.From == "" {
		return PlannedSubmit{}, errors.New("compose: the message has no From address")
	}

	si, err := tx.ResolveSendIdentity(ctx, env.From)
	if err != nil {
		return PlannedSubmit{}, err // ErrFromNotVerified — the security boundary
	}
	if req.IdentityID != (ulid.ULID{}) && req.IdentityID != si.IdentityID {
		return PlannedSubmit{}, fmt.Errorf(
			"%w: the message's From address is not authorized by the identity given",
			store.ErrFromNotVerified)
	}
	if req.EnvelopeFrom != "" &&
		store.NormalizeAddress(req.EnvelopeFrom) != store.NormalizeAddress(env.From) {
		// The envelope sender must be the header sender: a mismatch is either a
		// confused client or an attempt to send as someone else at the SMTP
		// layer, and neither is something to guess about.
		return PlannedSubmit{}, fmt.Errorf(
			"%w: the envelope MAIL FROM must equal the message's From address",
			store.ErrFromNotVerified)
	}

	rcpt := req.EnvelopeTo
	if len(rcpt) == 0 {
		rcpt = env.Recipients
	}
	rcpt = dedupeAddresses(rcpt)
	if len(rcpt) == 0 {
		return PlannedSubmit{}, ErrNoRecipients
	}

	// ENCRYPTION IS PLANNED HERE, with the other checks, so that a refusal (a
	// recipient with no key, a Bcc) happens before anything is written. It is a
	// SELECT-only step: the contact keys are read, the crypto process is not
	// called (that happens in SubmitPlanned, with the keys chosen here).
	var encPlan *encryptPlan
	if req.Encrypt {
		encPlan, err = c.planEncrypt(ctx, tx, si, raw, rcpt)
		if err != nil {
			return PlannedSubmit{}, err
		}
	}

	return PlannedSubmit{
		AccountID: si.AccountID, IdentityID: si.IdentityID,
		planned: true, req: req, si: si, raw: raw, rcpt: rcpt, encrypt: encPlan,
	}, nil
}

// SubmitPlanned records the send PlanSubmit validated. It is the DURABLE half
// and every side effect of a submit is here: the signing rewrite of the
// message's stored bytes, the submission row and its change_log row, and the
// job that will send it — all inside the caller's transaction.
//
// It re-validates nothing. The plan carries the bytes PlanSubmit checked and the
// identity it resolved, so what gets signed and sent is what was authorized;
// re-deriving them here would be a second copy of the security boundary, free to
// drift from the one that actually decided.
func (c *Composer) SubmitPlanned(ctx context.Context, tx *store.UserTx, userID ulid.ULID, plan PlannedSubmit) (SubmitResult, error) {
	if !plan.planned {
		return SubmitResult{}, errors.New("compose: submission was not planned (call PlanSubmit first)")
	}
	req, si, raw, rcpt := plan.req, plan.si, plan.raw, plan.rcpt

	// SIGN HERE, NOT AT SEND TIME (#47 increment 5). The stored message must BE
	// the signed message, so this runs before the submission row exists and
	// rewrites the Email's bytes in place.
	//
	// ⚠ IT IS ALSO THE FIRST DURABLE SIDE EFFECT OF A SUBMIT, which is why the
	// checks above it are a separate, mutation-free call (PlanSubmit): a caller
	// that refuses a send after this line has already rewritten the user's
	// message, and ADR-001's identity is the content hash.
	//
	// ⚠ Signing on the wire instead would put the user's own sent mail in their
	// mailbox TWICE. Ingest dedups on a body hash, and a signed body is a
	// multipart/signed wrapper — a different body — so the copy a provider
	// self-files comes back through sync as a SECOND message row, and nothing
	// collapses it. Signing here makes the content hash, the dedup hash, the
	// Sent copy, the blob and the bytes the recipient received one object, and
	// lets the archive prove what was signed.
	//
	// It happens AFTER the From check, deliberately: the key is chosen by the
	// identity PlanSubmit resolved (plan.si, carried here rather than re-derived),
	// so an unverified From can never reach a signing key.
	if err := c.prepareOutbound(ctx, tx, userID, si, req.EmailID, raw, plan.encrypt); err != nil {
		return SubmitResult{}, err
	}
	var encryptedTo []EncryptTo
	if plan.encrypt != nil {
		encryptedTo = plan.encrypt.to
		// TRUST ON FIRST USE: record the fingerprint actually encrypted to. In the
		// same transaction as the send, so a send that does not commit pins nothing.
		for _, t := range encryptedTo {
			if t.Source == "own" {
				continue
			}
			if err := tx.SetContactKeyPin(ctx, t.Address, t.Fingerprint, t.Source); err != nil {
				return SubmitResult{}, err
			}
		}
	}

	sendAt := req.SendAt
	if sendAt.IsZero() {
		sendAt = time.Now().Add(c.undoWindow)
	}
	sendAt = sendAt.UTC()

	subID, created, err := tx.CreateSubmission(ctx, store.SubmissionRow{
		AccountID:        si.AccountID,
		IdentityID:       si.IdentityID,
		MessageID:        req.EmailID,
		FromAddress:      si.FromAddress,
		EnvelopeTo:       rcpt,
		SendAt:           sendAt,
		IdempotencyKey:   req.IdempotencyKey,
		ArchiveOnSuccess: req.ArchiveOnSuccess,
		APIKeyID:         req.APIKeyID,
	})
	if err != nil {
		return SubmitResult{}, err
	}

	// A REPLAY STOPS HERE (#62). `created == false` means this exact send was already
	// recorded under the same idempotency key, so the job that will send it already
	// exists — and re-enqueuing is not merely redundant, it is FATAL while that job is
	// still outstanding: `app.jobs.idempotency_key` is unique across pending and running
	// jobs (jobs_live_idempotency_key) and EnqueueJob is a bare INSERT, so a second call
	// with `submit:<id>` raises a unique violation, which aborts the whole surrounding
	// transaction (SQLSTATE 25P02) and turns an idempotent retry into a permanent
	// serverFail the client would retry for ever.
	//
	// ⚠ That index went partial in #357, so once the send has completed the key is free
	// and the same INSERT would SUCCEED — a second submit job for a message already sent.
	// The guard against that is this branch, not the constraint, and it always was: the
	// arbiter here is app.submissions' own submissions_user_idempotency_key, which is
	// partial only on the key being non-NULL and so survives the send, so `created ==
	// false` is decided by the submission ledger and never by the job queue's memory.
	//
	// Returning the ORIGINAL submission's details is also the honest answer: the caller
	// asked for this send to have happened, and it has.
	if !created {
		existing, err := tx.GetSubmissions(ctx, []ulid.ULID{subID})
		if err != nil {
			return SubmitResult{}, err
		}
		if len(existing) == 0 {
			// Cannot happen — CreateSubmission just resolved this id under the same
			// transaction — but returning a half-filled result would be worse than
			// saying so.
			return SubmitResult{}, fmt.Errorf("compose: submission %s vanished mid-transaction", subID)
		}
		e := existing[0]
		return SubmitResult{
			SubmissionID: e.ID,
			AccountID:    e.AccountID,
			IdentityID:   e.IdentityID,
			SendAt:       e.SendAt,
		}, nil
	}

	jobID, err := tx.EnqueueJob(ctx, SubmitJobKind, sendAt,
		[]byte(`{"submission_id":"`+subID.String()+`"}`), "submit:"+subID.String())
	if err != nil {
		return SubmitResult{}, err
	}
	if err := tx.SetSubmissionJob(ctx, subID, jobID); err != nil {
		return SubmitResult{}, err
	}

	return SubmitResult{
		SubmissionID: subID, AccountID: si.AccountID, IdentityID: si.IdentityID,
		SendAt: sendAt, EnvelopeTo: rcpt, EncryptedTo: encryptedTo,
	}, nil
}

// signSubmission signs the message being submitted, in place, when the sending
// identity has a signing key bound. It is a no-op — not an error — when it does
// not, which is the ordinary case for every user without PGP.
//
// # Why an unsigned send is a success and not a failure
//
// Most users will never have a key. store.SigningKeyForIdentity already reports
// "no key" as found=false rather than an error for exactly this reason, and
// treating it as a failure here would put the common path down the error branch
// of the send everybody uses.
//
// # What a signing FAILURE means, now that signing is here
//
// ⚠ The original plan classified signing errors into requeue-vs-fail inside
// SendWorker. That split does not apply here and is NOT implemented as dead
// code: at this point there is no claim, no reserved quota and no submission
// row, so there is nothing to requeue or release. A failure simply aborts the
// caller's transaction, the submission is never created, and the client is told
// the send did not happen — which is the honest answer and the one that leaves
// nothing half-done.
//
// The practical consequence is worth stating plainly: while the crypto process
// is down, a user WITH a bound key cannot send. That is ADR-024's no-fallback
// rule reaching the send path — signing in the app instead would be the
// boundary deleting itself under load — and it fails loudly at the moment of
// sending rather than silently later.
//
// # IDEMPOTENT (#491)
//
// It rewrites the message's stored bytes, so submitting the same draft twice —
// the blessed re-send after an Undo Send cancellation — hands it its own output.
// outboundAlreadyPrepared (prepared.go) is the gate: bytes that are already
// signed, or already carry an Autocrypt header for this address, are left
// EXACTLY as they are. Signing again nests one multipart/signed inside another,
// and a second Autocrypt header makes a conformant receiver ignore both.
func (c *Composer) prepareOutbound(ctx context.Context, tx *store.UserTx, userID ulid.ULID, si store.SendIdentity, emailID ulid.ULID, raw []byte, enc *encryptPlan) error {
	// FIRST, before the key lookup: the question "are these bytes already what we
	// would send" is answered by the bytes, and a re-send should not re-derive the
	// key it is not going to use. The reason is discarded here and asserted in
	// prepared_test.go — the caller has one thing to do either way.
	//
	// ONE EXCEPTION (#47, ADR-038): the user asks for encryption on a re-send of a
	// draft that an earlier submit signed and the user then un-sent. Those bytes
	// are "prepared" (signed, advertised) but NOT encrypted, and returning here
	// would send the message in the clear after the user asked for it not to be.
	// So a prepared-but-unencrypted message goes on to encrypt, and skips only the
	// Autocrypt and signing steps it already has.
	prepared, _ := outboundAlreadyPrepared(raw, si.FromAddress)
	if prepared && (enc == nil || pgpmime.IsEncrypted(raw)) {
		return nil
	}

	key, ok, err := tx.SigningKeyForIdentity(ctx, si.IdentityID)
	if err != nil {
		return err
	}
	if !ok && enc != nil {
		// Cannot happen (planEncrypt read the same row in this transaction), and a
		// requested encryption is never allowed to fall through to plaintext.
		return ErrEncryptNoSenderKey
	}
	if !ok {
		// No key bound to this identity: nothing to advertise and nothing to
		// sign.
		//
		// ⚠ THIS IS ALSO WHAT KEEPS DERIVED ALIASES OUT (#48). The lookup joins
		// on the identity's default_key_id, and only DESIGNATED identities have
		// one — so a catch-all's derived aliases (potentially dozens on a
		// real instance) advertise nothing, structurally, rather than by a rule
		// somebody has to remember.
		return nil
	}
	if key.Kind != store.KeyPGP {
		// S/MIME is deferred to 2.x (#69). A future kind must not be signed by
		// the PGP path just because it happened to be bound — that would produce
		// a message claiming a signature format it does not have.
		return nil
	}

	outgoing, changed := raw, false

	// LAPSED KEYS (#795, ADR-040): a bound key whose own packets say it has
	// expired or been revoked is neither advertised nor used to sign. An
	// encrypted send never gets here with one: planEncrypt refused it.
	advertise, canSign := ownKeyFitness(key.PublicKey, time.Now())
	if !advertise || !canSign {
		slog.WarnContext(ctx, "compose: the sending identity's PGP key has lapsed; "+
			"not advertising it, and signing only if it still can",
			"key_id", key.ID.String(), "advertise", advertise, "sign", canSign)
	}

	// A prepared-but-unencrypted message (see the top of this function) already
	// has its Autocrypt header and signature; adding them again nests a
	// multipart/signed inside a multipart/signed and deletes the advertised key.
	if !prepared {
		// AUTOCRYPT (#48, spec §10a) — BEFORE signing, deliberately. pgpmime.Sign
		// moves every non-Content-* header to the OUTER message, so a header added
		// here lands where a receiving client looks for it. Added after signing it
		// would still work today, but only because nothing re-wraps afterwards.
		//
		// This is what lets a correspondent encrypt to this mailbox at all, and so
		// it is the thing that eventually gives #47 an inbound population to be
		// verified against.
		if len(key.PublicKey) > 0 && advertise {
			outgoing = prependHeader(outgoing, autocrypt.HeaderName,
				autocrypt.Header(si.FromAddress, key.PublicKey, autocryptPreferMutual))
			changed = true
		}

		// ⚠ SIGNING IS SEPARATELY CONDITIONAL. A bound key is enough to ADVERTISE
		// (the public half is in the row we just read); signing additionally needs
		// the crypto process, because the private half only ever exists behind that
		// boundary (ADR-024). With no crypto socket configured, outgoing mail now
		// carries the key and no signature — which is exactly what
		// cmd/mailroom's startup warning already describes.
		if c.signer != nil && canSign {
			signed, serr := pgpmime.Sign(outgoing, func(canonical []byte) (string, string, error) {
				return c.signer.Sign(ctx, userID, key.ID, canonical)
			})
			if serr != nil {
				return fmt.Errorf("compose: sign outgoing message: %w", serr)
			}
			outgoing, changed = signed, true
		}
	}

	// ENCRYPTION (#47, ADR-038) — AFTER signing: RFC 3156 §6.1's sign-then-encrypt.
	// The signed message's content headers and body become the entity inside the
	// ciphertext, so decrypt-on-open hands pgpverify exactly the multipart/signed
	// this function just made, and the sender's own copy verifies like any other.
	// The outer headers (From, To, Subject, Autocrypt) stay in the clear, because a
	// transport routes on them.
	//
	// ⚠ NO FALLBACK. A failure here fails the submit; it never sends the signed
	// plaintext instead of the encrypted message the user asked for.
	if enc != nil {
		encrypted, eerr := pgpmime.Encrypt(outgoing, func(entity []byte) (string, error) {
			return c.encrypter.Encrypt(ctx, userID, key.ID, enc.to, entity)
		})
		if eerr != nil {
			return fmt.Errorf("compose: encrypt outgoing message: %w", eerr)
		}
		outgoing, changed = encrypted, true
	}

	if !changed {
		return nil
	}
	signed := outgoing

	dek, dekID, err := c.keys.MailDEK(ctx, userID)
	if err != nil {
		return err
	}
	defer zero(dek)

	hash, err := c.blobs.Put(ctx, userID, dek, bytes.NewReader(signed))
	if err != nil {
		return fmt.Errorf("compose: store signed blob: %w", err)
	}
	stored, err := c.blobs.StoredSize(ctx, userID, hash)
	if err != nil {
		return fmt.Errorf("compose: signed blob size: %w", err)
	}

	// ⚠ No quota check here, and that is a decision rather than an omission.
	// The signature adds on the order of a kilobyte to a message the user has
	// already been allowed to store, and refusing the SEND of an
	// already-composed message because of that delta would be a worse outcome
	// than briefly exceeding the limit.
	//
	// ⚠ THE UNSIGNED BLOB IS RECLAIMED HERE, NOT "BY GC" (#300). This comment
	// used to say the replaced blob "becomes unreferenced and is reclaimed by
	// GC". There is no GC. There has never been a GC. Orphan
	// app.blobs rows the audit counted were made by this
	// exact line, and each of them keeps billing the user's storage quota
	// (StorageQuota sums app.blobs.size_stored) for bytes nothing can read.
	// ⚠ RE-DERIVED FROM THE SIGNED BYTES, NOT ASSUMED (#377). Signing wraps the
	// message in multipart/signed and adds an application/pgp-signature part,
	// which internal/mime classifies as an attachment (it is a non-text leaf) —
	// so this is always true in practice today, and hard-coding `true` would
	// work. It parses instead because the value must describe THESE bytes: if
	// pgpmime's output shape ever changes, a constant would quietly start lying,
	// and a projection quietly disagreeing with its own bytes is the exact defect
	// this line exists to fix.
	signedMsg, err := mime.ParseBytes(signed)
	if err != nil {
		return fmt.Errorf("compose: re-parse signed message: %w", err)
	}

	reclaim, err := tx.ReplaceMessageBytes(ctx, emailID, store.BlobMeta{
		ContentHash: hash[:],
		SizePlain:   int64(len(signed)),
		SizeStored:  stored,
		DEKID:       dekID,
		EnvelopeVer: 1,
	}, store.MessageBytesProjection{
		HasAttachments: len(signedMsg.Attachments) > 0,
		// Re-derived from the SIGNED bytes, not carried over from the draft
		// (#393). Signing adds a part, so the pre-signing projection describes
		// a message that no longer exists — the #377 defect, one table over.
		Attachments: attachproj.Rows(signedMsg),
		// An encrypted message must not keep the plaintext snippet derived from the
		// draft (#47, ADR-038).
		ClearSnippet: enc != nil,
	})
	if err != nil {
		return err
	}
	// The FILE, inside the caller's transaction, before it commits — the ordering
	// store.HardDeleteMessage proves. A delete failure aborts the submit, which is
	// the honest outcome: nothing half-signed, and the user is told the send did
	// not happen.
	return c.reclaimBlobFiles(ctx, userID, reclaim)
}

// reclaimBlobFiles deletes the on-disk bytes of blobs whose app.blobs row a store
// call just removed. It MUST run inside the transaction that removed the rows:
// the row delete is invisible to a concurrent ingest until commit, so the
// "not found by content hash" window a byte-identical re-ingest needs in order to
// re-Put these bytes opens only AFTER the file is gone (store.HardDeleteMessage
// has the full proof). An error aborts the caller's transaction, so the failure
// mode is "nothing was destroyed, retry", never a row that outlives its bytes.
func (c *Composer) reclaimBlobFiles(ctx context.Context, userID ulid.ULID, hashes [][]byte) error {
	for _, h := range hashes {
		if err := c.DeleteRaw(ctx, userID, h); err != nil {
			return err
		}
	}
	return nil
}

// dedupeAddresses normalizes and de-duplicates an envelope recipient list.
func dedupeAddresses(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, a := range in {
		n := store.NormalizeAddress(a)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// zero wipes a key slice. It duplicates crypto.Zero rather than importing it, to
// keep this package free of the key hierarchy (see Keys).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ReadRaw returns a message's raw RFC 5322 bytes from the blob store.
func (c *Composer) ReadRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) ([]byte, error) {
	return readRaw(ctx, c.blobs, c.keys, userID, contentHash)
}

// OpenRaw streams a message's bytes out of the blob store without buffering
// them. It is what the JMAP download endpoint serves a whole message with: a
// 50 MiB message must not become a 50 MiB allocation per request.
//
// The returned reader surfaces the AEAD/content-hash verification failure on its
// terminating Read (blob.BlobStore.Get), so bytes are trustworthy only after a
// clean io.EOF — a caller streaming to the network gets a TRUNCATED response
// rather than a complete-looking, unverified one.
func (c *Composer) OpenRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) (io.ReadCloser, error) {
	dek, _, err := c.keys.MailDEK(ctx, userID)
	if err != nil {
		return nil, err
	}
	h, err := toContentHash(contentHash)
	if err != nil {
		zero(dek)
		return nil, err
	}
	rc, err := c.blobs.Get(ctx, userID, dek, h)
	if err != nil {
		zero(dek)
		return nil, fmt.Errorf("compose: open blob: %w", err)
	}
	// The key must outlive this call: blob.Get decrypts LAZILY, in a goroutine
	// draining into a pipe, so a `defer zero(dek)` here would hand that goroutine
	// a key of zeroes and the stream would yield nothing at all. It is wiped when
	// the caller closes the stream instead — which is exactly as long as it is
	// needed, and no longer.
	return &keyedReadCloser{rc: rc, dek: dek}, nil
}

// keyedReadCloser ties a DEK's lifetime to the stream it decrypts.
type keyedReadCloser struct {
	rc  io.ReadCloser
	dek []byte
}

func (k *keyedReadCloser) Read(p []byte) (int, error) { return k.rc.Read(p) }

func (k *keyedReadCloser) Close() error {
	zero(k.dek)
	return k.rc.Close()
}

// HasRaw reports whether THIS USER has a blob with this content hash. The blob
// store namespaces every user's blobs behind a keyed path derivation, so the
// question is an ownership question, not a content-addressed lookup.
func (c *Composer) HasRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) (bool, error) {
	h, err := toContentHash(contentHash)
	if err != nil {
		return false, err
	}
	return c.blobs.Exists(ctx, userID, h)
}

// DeleteRaw removes a user's blob FILE. It is the second half of a blob reclaim
// (#300): the store deletes the app.blobs metadata row under its guard and hands
// the hashes back, and this deletes the bytes — inside the SAME transaction,
// before it commits, which is the race proof store.HardDeleteMessage spells out.
//
// It is deliberately NOT a delete anyone can reach from a JMAP method. There is
// no "delete this blob" verb: every caller is a path where the store has ALREADY
// established, under the shared guard, that nothing references these bytes.
// Deleting a blob that does not exist is not an error (the FS store is
// idempotent), so a retried reclaim is safe.
func (c *Composer) DeleteRaw(ctx context.Context, userID ulid.ULID, contentHash []byte) error {
	h, err := toContentHash(contentHash)
	if err != nil {
		return err
	}
	if err := c.blobs.Delete(ctx, userID, h); err != nil {
		return fmt.Errorf("compose: delete blob: %w", err)
	}
	return nil
}

// StoredBlob is the metadata of a blob PutRaw has just written: what the store's
// app.blobs row needs.
type StoredBlob struct {
	ContentHash []byte
	SizePlain   int64
	SizeStored  int64
	DEKID       ulid.ULID
}

// PutRaw stores an uploaded blob for a user, encrypting it as it streams (the
// bytes are never fully buffered). It is the write half of the JMAP upload
// endpoint, and it goes through the SAME derived mail-blob DEK as everything
// else this package reads and writes — a second key-derivation path for "the
// upload endpoint" is exactly the drift that silently makes a message
// undecryptable later.
func (c *Composer) PutRaw(ctx context.Context, userID ulid.ULID, r io.Reader) (StoredBlob, error) {
	dek, dekID, err := c.keys.MailDEK(ctx, userID)
	if err != nil {
		return StoredBlob{}, err
	}
	defer zero(dek)

	counter := &countingReader{r: r}
	hash, err := c.blobs.Put(ctx, userID, dek, counter)
	if err != nil {
		return StoredBlob{}, fmt.Errorf("compose: store blob: %w", err)
	}
	stored, err := c.blobs.StoredSize(ctx, userID, hash)
	if err != nil {
		return StoredBlob{}, fmt.Errorf("compose: blob size: %w", err)
	}
	return StoredBlob{
		ContentHash: hash[:],
		SizePlain:   counter.n,
		SizeStored:  stored,
		DEKID:       dekID,
	}, nil
}

// countingReader counts the plaintext octets that pass through it — the `size`
// RFC 8620 §6.1 reports for an upload. It also preserves the underlying reader's
// error (an http.MaxBytesError from an oversized upload, notably), which is what
// lets the endpoint answer 413 instead of 500.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// toContentHash converts a raw hash to the blob store's fixed-size type.
func toContentHash(contentHash []byte) (blob.ContentHash, error) {
	var h blob.ContentHash
	if len(contentHash) != len(h) {
		return h, fmt.Errorf("compose: content hash is %d bytes, want %d", len(contentHash), len(h))
	}
	copy(h[:], contentHash)
	return h, nil
}

// readRaw is the shared blob read used by both the Composer and the send worker.
func readRaw(ctx context.Context, blobs blob.BlobStore, keys Keys, userID ulid.ULID, contentHash []byte) ([]byte, error) {
	dek, _, err := keys.MailDEK(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer zero(dek)

	h, err := toContentHash(contentHash)
	if err != nil {
		return nil, err
	}

	rc, err := blobs.Get(ctx, userID, dek, h)
	if err != nil {
		return nil, fmt.Errorf("compose: read blob: %w", err)
	}
	defer func() { _ = rc.Close() }()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		// A failure here is an authentication/hash-verification failure: the
		// bytes are NOT trustworthy and must never be sent.
		return nil, fmt.Errorf("compose: read blob: %w", err)
	}
	return buf.Bytes(), nil
}

// Envelope is the addressing information read back out of a message's bytes.
type Envelope struct {
	From string
	// Recipients is To + Cc + Bcc — the SMTP RCPT TO set.
	Recipients []string
}

// ParseEnvelope extracts the sender and recipients from raw message bytes. It is
// tolerant by construction (mime.ParseBytes never fails hard on hostile input);
// a message with no From yields an empty From, which the caller rejects.
func ParseEnvelope(raw []byte) Envelope {
	parsed, _ := mime.ParseBytes(raw)
	if parsed == nil {
		return Envelope{}
	}
	var env Envelope
	// messageFrom, not an inline copy of it: outboundAlreadyPrepared compares the
	// From on the bytes against the address this send resolved to, and the two
	// derivations must not be able to differ (prepared.go).
	env.From = messageFrom(parsed)
	seen := map[string]bool{}
	for _, list := range [][]mime.Address{
		parsed.Header.To(), parsed.Header.Cc(), parsed.Header.Bcc(),
	} {
		for _, a := range list {
			n := store.NormalizeAddress(a.Address)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			env.Recipients = append(env.Recipients, n)
		}
	}
	return env
}

const snippetLen = 200

func snippet(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if len([]rune(s)) <= snippetLen {
		return s
	}
	return string([]rune(s)[:snippetLen])
}

// mustParse parses bytes this package just built. internal/mime is total on
// hostile input (docs/mime-policy.md) and these bytes are our own, so a parse
// error here is not a runtime condition to handle — it is a bug in build.go.
// Returning nil rather than panicking keeps a compose failure out of the send
// path: attachmentRows(nil) is nil, which projects no rows, and the message
// still records. The alternative — dropping the whole compose — would be a
// worse answer to "our own builder produced something we cannot read".
func mustParse(raw []byte) *mime.Message {
	m, _ := mime.ParseBytes(raw)
	return m
}

// autocryptPreferMutual controls whether outgoing mail advertises
// `prefer-encrypt=mutual`.
//
// ⚠ IT IS FALSE ON PURPOSE, AND FLIPPING IT BELONGS WITH DECRYPTION (#47).
// `mutual` tells every Autocrypt-aware correspondent to encrypt to us BY
// DEFAULT. Mailroom cannot decrypt yet, and an encrypted message today is close
// to inert: the body is unreadable and every attachment is forced to `unknown`
// and withheld (#405, ADR-027 §6). Advertising mutual now would fill the
// mailbox with mail the user cannot read, on the strength of a header we chose
// to send.
//
// Advertising the KEY without the preference is the useful half: correspondents
// learn it, can encrypt when they deliberately choose to, and #47 gets a real
// population to be verified against.
const autocryptPreferMutual = false

// prependHeader puts one header field at the top of a raw RFC 5322 message.
//
// Header order carries no meaning, so prepending is safe and — more to the
// point — needs no parse. The alternative, finding the end of the header block,
// means deciding what to do about a malformed one; there is nothing to decide
// here, because these are bytes this package just built.
func prependHeader(raw []byte, name, value string) []byte {
	out := make([]byte, 0, len(raw)+len(name)+len(value)+4)
	out = append(out, name...)
	out = append(out, ':', ' ')
	out = append(out, value...)
	out = append(out, '\r', '\n')
	return append(out, raw...)
}

// Recipients is the envelope a planned send would go to (copies; the plan is
// unchanged). The API's approval queue reads it to tell a person who a held
// send is addressed to (#49, ADR-042 §7) — from the bytes, like the send.
func (p PlannedSubmit) Recipients() []string { return append([]string(nil), p.rcpt...) }

// HeaderRecipients are the To, Cc and Bcc header addresses of the planned
// bytes, normalised, for a person approving a held send (#49 review). The
// envelope (Recipients) is who actually receives it; these say how each was
// addressed.
func (p PlannedSubmit) HeaderRecipients() (to, cc, bcc []string) {
	m, _ := mime.ParseBytes(p.raw)
	if m == nil {
		return nil, nil, nil
	}
	norm := func(list []mime.Address) []string {
		out := []string{}
		for _, a := range list {
			if n := store.NormalizeAddress(a.Address); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	return norm(m.Header.To()), norm(m.Header.Cc()), norm(m.Header.Bcc())
}

// SendAt is when the send was asked for; zero means "now, after the undo
// window".
func (p PlannedSubmit) SendAt() time.Time { return p.req.SendAt }

// ErrUnlistableAttachments means the planned bytes carry a part this listing
// cannot account for, so they cannot be summarised honestly.
var ErrUnlistableAttachments = errors.New("compose: the message carries a part whose attachments cannot be listed")

// Attachments lists everything this send would transmit beyond the
// composer's own body, for the person approving a held send (#49 review): a
// key can attach any blob of the user's, and the approver must see what
// leaves, not only who it goes to.
//
// ⚠ IT DOES NOT ASK WHICH PARTS "LOOK LIKE" ATTACHMENTS. The display
// classification (attachproj.Rows, mime.IsAttachmentLeaf) leaves inline and
// text parts out, so a blob attached as text/plain, inline, with a Content-ID
// would be summarised as "no attachments" while it still went out. This
// works from the shape Build writes instead: with attachments the message is
// multipart/mixed, the FIRST child is the body and EVERY later child is one
// attachment, whatever its type or disposition. Inside the body only bare
// text/plain, text/html and text/calendar leaves (no disposition, no
// Content-ID, no filename) are body; any other leaf there is listed too. An
// attachment that parses as a container (a blob declared multipart/*) cannot
// be sized or named honestly and is refused with ErrUnlistableAttachments.
// Errors in the other direction (listing a body part) are acceptable; missing
// an attachment is not.
func (p PlannedSubmit) Attachments() ([]store.AttachmentRow, error) {
	m, err := mime.ParseBytes(p.raw)
	if err != nil {
		return nil, err
	}
	root := m.Structure
	if root == nil {
		return nil, ErrUnlistableAttachments
	}
	var out []store.AttachmentRow
	row := func(n *mime.Structure) store.AttachmentRow {
		return store.AttachmentRow{Filename: n.Filename, ContentType: n.ContentType, SizeBytes: int64(n.OctetSize)}
	}
	var body func(n *mime.Structure)
	body = func(n *mime.Structure) {
		if mime.IsMultipart(n.ContentType) {
			for _, sub := range n.SubParts {
				body(sub)
			}
			return
		}
		switch n.ContentType {
		case "text/plain", "text/html", "text/calendar":
			if n.Disposition == "" && n.ContentID == "" && n.Filename == "" {
				return
			}
		}
		out = append(out, row(n))
	}
	if root.ContentType != "multipart/mixed" {
		body(root)
		return out, nil
	}
	for i, child := range root.SubParts {
		if i == 0 {
			body(child)
			continue
		}
		if mime.IsMultipart(child.ContentType) || child.SubParts != nil {
			return nil, ErrUnlistableAttachments
		}
		out = append(out, row(child))
	}
	return out, nil
}

// DraftAccount is the account a draft From would be saved under: the same
// resolution SaveDraft runs, writing nothing. The API uses it to keep an
// account-restricted API key's drafts inside its accounts (#49, ADR-042 §5).
func (c *Composer) DraftAccount(ctx context.Context, tx *store.UserTx, fromEmail string) (ulid.ULID, error) {
	si, err := c.resolveFrom(ctx, tx, fromEmail)
	if err != nil {
		return ulid.ULID{}, err
	}
	return si.AccountID, nil
}
