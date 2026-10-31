// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// This file holds the store surface for account configuration (spec §4 "Account
// = one IMAP/SMTP login", §12 in-UI account management). Accounts carry the
// connection endpoints plus the envelope-encrypted account secret
// (credentials_ct/credentials_dek, spec §9).
//
// Change-log discipline (the load-bearing invariant — CLAUDE.md): an Account IS
// a JMAP-visible object as of in-UI account management (mailroom#98/#45): the
// urn:mailroom:accounts extension's Account/get, /query, /changes and /set
// expose it. So every create/update/destroy here appends exactly one
// ObjectAccount change_log row in the SAME UserTx as the mutation (the boot-time
// accounts.json reconcile drives the very same CreateAccount, so declared
// accounts flow through /changes too). Every method runs on a UserTx, so RLS
// scopes each statement to the owning user: the accounts_isolation policy's
// WITH CHECK (user_id = app.current_user_id()) lets the app role insert only its
// own rows, and USING hides other users' rows on read — fail closed, no
// identity, no rows. A cross-user leak would expose another user's mail-server
// CREDENTIALS, so the isolation here is a security boundary
// (internal/api/rls_integration_test.go proves it).

// ErrAccountNotFound is returned by GetAccount / SetAccountSyncEnabled when the
// user has no account with the requested id.
var ErrAccountNotFound = errors.New("store: account not found")

// ErrAccountNameTaken is returned when a create/rename would collide with the
// caller's UNIQUE (user_id, name) constraint on app.accounts. It is a
// RECOVERABLE, per-object /set outcome, so the INSERT/UPDATE that hits it runs
// under a SAVEPOINT (see CreateAccount / UpdateAccount).
var ErrAccountNameTaken = errors.New("store: account name already exists")

// accountColumns is the SELECT list shared by GetAccount and the list methods,
// in the exact order scanAccount expects. Every user of it selects FROM
// app.accounts unaliased, which the grant-state subquery's `accounts.` refers
// to.
//
// The last column is the account's OAuth grant state (#52, ADR-046 §5),
// empty when there is no grant: a password account, or an OAuth account that was
// disconnected. A scalar subquery rather than a join so the FOR UPDATE in
// UpdateAccount still locks exactly one row of exactly one table; the grant is
// 1:1 with the account (its primary key), and RLS scopes the subquery to the
// same user as the outer row.
const accountColumns = `id, name, provider_profile, imap_host, imap_port,
	smtp_host, smtp_port, auth_kind, credentials_ct, credentials_dek,
	envelope_ver, sync_enabled, outbound_daily_quota, color, writeback_enabled, notify_push,
	COALESCE((SELECT g.state FROM app.oauth_grants g
	           WHERE g.user_id = accounts.user_id AND g.account_id = accounts.id), '')`

// grantNeedsReauth is the predicate "this OAuth account is waiting for its
// user to reconnect", for the account lists below: its grant is needs_reauth,
// or it has no grant at all (disconnected — whose sync Account/set could
// switch back on). The SQL twin of Account.AwaitingReconnect.
const grantNeedsReauth = `(accounts.auth_kind = 'oauth2' AND NOT EXISTS (SELECT 1 FROM app.oauth_grants g
	WHERE g.user_id = accounts.user_id AND g.account_id = accounts.id AND g.state <> 'needs_reauth'))`

// AccountRow is the input to CreateAccount. The caller supplies the already
// envelope-encrypted secret (credentials_ct) and the DEK reference/scheme
// sentinel (credentials_dek); the store never sees or handles plaintext
// credentials. Zero-valued optional fields fall back to the schema defaults.
type AccountRow struct {
	Name            string
	ProviderProfile string // "" -> "generic"
	IMAPHost        string
	IMAPPort        int // <= 0 -> 993
	SMTPHost        string
	SMTPPort        int    // <= 0 -> 465
	AuthKind        string // "" -> "password"
	CredentialsCT   []byte // AEAD envelope over the account secret (spec §9)
	CredentialsDEK  ulid.ULID
	EnvelopeVer     int // <= 0 -> 1
	SyncEnabled     bool
	Color           *string // presentation color (nil -> NULL, spec §12)
	// WritebackEnabled is INGEST-ONLY mode at creation time (ADR-028). nil
	// takes the schema default, which is true.
	//
	// ⚠ A POINTER ON PURPOSE. As a plain bool, every existing caller that does
	// not know about this field would construct the zero value and silently
	// create a SHADOWED account whose mutations go nowhere — the failure this
	// mode exists to make impossible, introduced by the mode itself.
	//
	// It exists at creation, rather than only as a later patch, so a
	// real account can be attached already shadowed in ONE statement,
	// with no window in which it exists live.
	//
	// ⚠ An earlier version of this comment justified that by saying ingest-time
	// rule labelling could enqueue an intent in the gap. That is FALSE and was
	// corrected in review: every ingest-time rule arm writes directly
	// (message_labels insert, keyword/flag UPDATE, moved_mailbox_id UPDATE) and
	// none of them enqueues. The window is real for anything else that can
	// mutate in that interval — the drawer sweep, a connected client, an
	// apply-rules run — which is reason enough, but not the reason first given.
	WritebackEnabled *bool
	// OutboundDailyQuota is the daily send cap at creation; <= 0 keeps the
	// schema default (a profile's stated value, provider.OutboundDailyQuota).
	OutboundDailyQuota int
}

// Account is a stored account projection carrying everything the sync loop
// needs to connect and decrypt (spec §4). CredentialsCT is the sealed secret;
// the caller re-derives the credentials key and opens it at sync time.
type Account struct {
	ID              ulid.ULID
	Name            string
	ProviderProfile string
	IMAPHost        string
	IMAPPort        int
	SMTPHost        string
	SMTPPort        int
	AuthKind        string
	CredentialsCT   []byte
	CredentialsDEK  ulid.ULID
	EnvelopeVer     int
	SyncEnabled     bool
	// OutboundDailyQuota is the provider's per-account daily send cap
	// (app.accounts.outbound_daily_quota). It is read-only on the JMAP surface.
	OutboundDailyQuota int
	// Color is the user-chosen presentation color, nil when none is set.
	Color *string
	// WritebackEnabled false is INGEST-ONLY mode (ADR-028): the account syncs
	// normally but Mailroom never constructs an IMAPWriter for it, so it cannot
	// mutate the remote mailbox. Orthogonal to SyncEnabled, which gates the
	// whole pass including ingest.
	//
	// ⚠ This is the per-account half only. The effective answer is this AND the
	// instance-level switch — use Store.WritebackDisabled() / the UserTx helper
	// rather than reading this field alone when deciding whether to write.
	WritebackEnabled bool
	// NotifyPush false silences Web Push for new mail in this account (#59),
	// on every device. Default true.
	NotifyPush bool
	// AuthState is the account's OAuth grant state (OAuthStateActive,
	// OAuthStateNeedsReauth, OAuthStateOperatorError), "" when it has no grant
	// — every password account, and an OAuth account that was disconnected.
	// Read-only here: it changes only through the grant methods, which append
	// the Account change row (oauth_grants.go).
	AuthState string
}

// CreateAccount inserts a new account for the transaction's user, appends the
// ObjectAccount change_log row (the invariant), and returns its app-generated id
// (ADR-007). UNIQUE(user_id, name) rejects a duplicate name for the same user as
// ErrAccountNameTaken.
//
// The INSERT runs under a SAVEPOINT: a UNIQUE (user_id, name) violation is a
// RECOVERABLE, caller-visible outcome, and without the savepoint the failed
// statement would abort the whole surrounding UserTx (SQLSTATE 25P02) — so an
// Account/set that reports one duplicate name in notCreated and continues would
// lose every other create in the same call (the pattern CreateLabel uses).
func (tx *UserTx) CreateAccount(ctx context.Context, a AccountRow) (ulid.ULID, error) {
	switch {
	case a.Name == "":
		return ulid.ULID{}, errors.New("store: create account: name is required")
	case a.IMAPHost == "":
		return ulid.ULID{}, errors.New("store: create account: imap host is required")
	case a.SMTPHost == "":
		return ulid.ULID{}, errors.New("store: create account: smtp host is required")
	case len(a.CredentialsCT) == 0:
		return ulid.ULID{}, errors.New("store: create account: credentials ciphertext is required")
	}

	id := NewULID()
	imapPort := a.IMAPPort
	if imapPort <= 0 {
		imapPort = 993
	}
	smtpPort := a.SMTPPort
	if smtpPort <= 0 {
		smtpPort = 465
	}
	profile := a.ProviderProfile
	if profile == "" {
		profile = "generic"
	}
	authKind := a.AuthKind
	if authKind == "" {
		authKind = "password"
	}
	envVer := a.EnvelopeVer
	if envVer <= 0 {
		envVer = 1
	}

	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: create account (savepoint): %w", err)
	}
	if _, err := sp.Exec(ctx, `
		INSERT INTO app.accounts (
			id, user_id, name, provider_profile,
			imap_host, imap_port, smtp_host, smtp_port,
			auth_kind, credentials_ct, credentials_dek, envelope_ver, sync_enabled,
			color, writeback_enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,
			COALESCE($15, true))`,
		id.String(), tx.userID.String(), a.Name, profile,
		a.IMAPHost, imapPort, a.SMTPHost, smtpPort,
		authKind, a.CredentialsCT, a.CredentialsDEK.String(), envVer, a.SyncEnabled,
		a.Color, a.WritebackEnabled,
	); err != nil {
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return ulid.ULID{}, ErrAccountNameTaken
		}
		return ulid.ULID{}, fmt.Errorf("store: create account: %w", err)
	}
	if a.OutboundDailyQuota > 0 {
		// A profile's stated send cap (provider.OutboundDailyQuota); without
		// one the column keeps its schema default, which this never restates.
		if _, err := sp.Exec(ctx, `
			UPDATE app.accounts SET outbound_daily_quota = $3 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), id.String(), a.OutboundDailyQuota); err != nil {
			_ = sp.Rollback(ctx)
			return ulid.ULID{}, fmt.Errorf("store: create account (quota): %w", err)
		}
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return ulid.ULID{}, fmt.Errorf("store: create account (release): %w", err)
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, id, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return id, nil
}

// ListSyncableAccounts returns the user's sync_enabled accounts, ordered by
// name — the set the sync runner iterates each pass, the IDLE watchers follow
// and a writeback nudge drains.
//
// An OAuth account whose grant NEEDS REAUTH is left out (#52, ADR-046 §5):
// the provider has already said its refresh token is dead, so a pass could
// only open a connection to fail it, and every held writeback intent stays
// pending, uncharged, until the user reconnects. Reconnecting returns the
// grant to active, and the account is listed again on the next pass with
// nothing else to switch back on. ListAwaitingReconnect names the ones left
// out, so the runner can say so rather than fall silent about them.
func (tx *UserTx) ListSyncableAccounts(ctx context.Context) ([]Account, error) {
	return tx.queryAccounts(ctx, `
		SELECT `+accountColumns+`
		  FROM app.accounts
		 WHERE user_id = $1 AND sync_enabled = true AND NOT `+grantNeedsReauth+`
		 ORDER BY name`, tx.userID.String())
}

// ListAwaitingReconnect returns the user's sync-enabled accounts that
// ListSyncableAccounts leaves out because their OAuth grant needs the user to
// reconnect, ordered by name.
func (tx *UserTx) ListAwaitingReconnect(ctx context.Context) ([]Account, error) {
	return tx.queryAccounts(ctx, `
		SELECT `+accountColumns+`
		  FROM app.accounts
		 WHERE user_id = $1 AND sync_enabled = true AND `+grantNeedsReauth+`
		 ORDER BY name`, tx.userID.String())
}

// ListDisabledAccounts returns the user's accounts whose sync is switched off,
// ordered by name — the complement of ListSyncableAccounts. The sync runner
// names these at WARN on every pass (mailroom#435): a disabled account is
// otherwise invisible, because a pass that lists zero accounts reads exactly
// like a correctly idle instance.
func (tx *UserTx) ListDisabledAccounts(ctx context.Context) ([]Account, error) {
	return tx.queryAccounts(ctx, `
		SELECT `+accountColumns+`
		  FROM app.accounts
		 WHERE user_id = $1 AND sync_enabled = false
		 ORDER BY name`, tx.userID.String())
}

// ListAccounts returns every account owned by the user (enabled or not),
// ordered by name — used by the operator CLI's list-accounts.
func (tx *UserTx) ListAccounts(ctx context.Context) ([]Account, error) {
	return tx.queryAccounts(ctx, `
		SELECT `+accountColumns+`
		  FROM app.accounts
		 WHERE user_id = $1
		 ORDER BY name`, tx.userID.String())
}

// GetAccount returns one account by id, or ErrAccountNotFound.
func (tx *UserTx) GetAccount(ctx context.Context, id ulid.ULID) (Account, error) {
	a, err := scanAccount(tx.tx.QueryRow(ctx, `
		SELECT `+accountColumns+`
		  FROM app.accounts
		 WHERE user_id = $1 AND id = $2`, tx.userID.String(), id.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("store: get account %s: %w", id, ErrAccountNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("store: get account: %w", err)
	}
	return a, nil
}

// GetAccounts returns the account projections for the requested ids (RLS-scoped).
// Ids not returned were not found or belong to another user — indistinguishable
// by design. Result order is unspecified; the caller re-associates by id. It is
// the multi-id read Account/get uses.
func (tx *UserTx) GetAccounts(ctx context.Context, ids []ulid.ULID) ([]Account, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return tx.queryAccounts(ctx, `
		SELECT `+accountColumns+`
		  FROM app.accounts
		 WHERE user_id = $1 AND id = ANY($2)`, tx.userID.String(), ulidStrings(ids))
}

// AccountConnection is the endpoint + sealed-credential set an UpdateAccount
// applies atomically when the caller rotates credentials or changes endpoints.
// The API only ever populates it AFTER re-validating the endpoint by dialing
// IMAP and re-sealing the secret, so the store never sees plaintext. Endpoints
// are always fully supplied on a connection change (the client re-sends the
// non-secret host/port it already has from Account/get, plus the re-entered
// username/password), so UpdateAccount needs no plaintext of the old secret.
type AccountConnection struct {
	ProviderProfile string
	IMAPHost        string
	IMAPPort        int
	SMTPHost        string
	SMTPPort        int
	CredentialsCT   []byte
	CredentialsDEK  ulid.ULID
	EnvelopeVer     int
}

// AccountPatch is a partial update for UpdateAccount. A nil field means "leave
// unchanged". Connection, when non-nil, replaces the endpoints and sealed
// credentials as one unit (see AccountConnection).
type AccountPatch struct {
	Name        *string
	SyncEnabled *bool
	Connection  *AccountConnection
	// Color is applied only when SetColor is true; Color==nil then clears it to
	// SQL NULL. SetColor stays false for a patch that does not mention color, so
	// an unrelated update never touches it (nil-vs-absent, which a bare *string
	// could not express).
	SetColor bool
	Color    *string
	// WritebackEnabled toggles INGEST-ONLY mode (ADR-028). nil leaves it alone.
	//
	// Turning it OFF is not enough on its own: the caller must also call
	// SuppressAccountIntents in the SAME transaction, or intents queued before
	// the switch survive as 'pending' and fire the instant the mode is lifted.
	// UpdateAccount does not do it implicitly, because a store method that
	// silently rewrites a queue as a side effect of a metadata patch is worse
	// than an explicit call the API layer makes.
	WritebackEnabled *bool
	// NotifyPush switches new-mail pushes for this account on or off (#59).
	// nil leaves it alone.
	NotifyPush *bool
}

// UpdateAccount applies a partial update to an account and appends exactly one
// ObjectAccount change_log row when something actually changes (a no-op patch is
// a true no-op). found is false when the account does not exist for the caller.
// A rename that collides on UNIQUE (user_id, name) is ErrAccountNameTaken,
// surfaced under a SAVEPOINT so it does not poison the surrounding UserTx.
func (tx *UserTx) UpdateAccount(ctx context.Context, id ulid.ULID, patch AccountPatch) (found bool, err error) {
	// Lock and read current state so unspecified fields are preserved and the
	// guarded UPDATE can detect a genuine no-op.
	var cur Account
	cur, err = scanAccount(tx.tx.QueryRow(ctx, `
		SELECT `+accountColumns+`
		  FROM app.accounts
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`, tx.userID.String(), id.String()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: update account (read): %w", err)
	}

	// ADR-046 §9 obligation (e): an OAuth account's hosts are its provider's
	// and its secret is its grant's. A connection change would re-seal a
	// password over the refresh token and orphan the grant while auth_kind
	// stayed 'oauth2' — an incoherent state this refuses outright, here at the
	// one write path, whichever caller asks (Account/set refuses earlier with
	// a friendlier message; this is the guarantee).
	if patch.Connection != nil && cur.AuthKind == "oauth2" {
		return true, ErrOAuthConnectionImmutable
	}

	n := cur // merged next state
	if patch.Name != nil {
		n.Name = *patch.Name
	}
	if patch.SyncEnabled != nil {
		n.SyncEnabled = *patch.SyncEnabled
	}
	if patch.SetColor {
		n.Color = patch.Color
	}
	if patch.WritebackEnabled != nil {
		n.WritebackEnabled = *patch.WritebackEnabled
	}
	if patch.NotifyPush != nil {
		n.NotifyPush = *patch.NotifyPush
	}
	if c := patch.Connection; c != nil {
		n.ProviderProfile = firstNonEmptyStr(c.ProviderProfile, "generic")
		n.IMAPHost, n.IMAPPort = c.IMAPHost, defaultPort(c.IMAPPort, 993)
		n.SMTPHost, n.SMTPPort = c.SMTPHost, defaultPort(c.SMTPPort, 465)
		n.CredentialsCT = c.CredentialsCT
		n.CredentialsDEK = c.CredentialsDEK
		n.EnvelopeVer = c.EnvelopeVer
		if n.EnvelopeVer <= 0 {
			n.EnvelopeVer = 1
		}
	}

	sp, err := tx.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("store: update account (savepoint): %w", err)
	}
	var touched string
	err = sp.QueryRow(ctx, `
		UPDATE app.accounts
		   SET name = $3, sync_enabled = $4, provider_profile = $5,
		       imap_host = $6, imap_port = $7, smtp_host = $8, smtp_port = $9,
		       credentials_ct = $10, credentials_dek = $11, envelope_ver = $12,
		       color = $13, writeback_enabled = $14, notify_push = $15
		 WHERE user_id = $1 AND id = $2
		   AND (name, sync_enabled, provider_profile, imap_host, imap_port,
		        smtp_host, smtp_port, credentials_ct, credentials_dek, envelope_ver, color,
		        writeback_enabled, notify_push)
		       IS DISTINCT FROM
		       ($3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::boolean)
		RETURNING id`,
		tx.userID.String(), id.String(), n.Name, n.SyncEnabled, n.ProviderProfile,
		n.IMAPHost, n.IMAPPort, n.SMTPHost, n.SMTPPort,
		n.CredentialsCT, n.CredentialsDEK.String(), n.EnvelopeVer, n.Color,
		n.WritebackEnabled, n.NotifyPush).Scan(&touched)
	if err != nil {
		if isNoRows(err) {
			// The row exists (we locked it) but nothing changed: a true no-op, so no
			// change_log row. No SQL error occurred, so release rather than roll back.
			if cerr := sp.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("store: update account (release): %w", cerr)
			}
			return true, nil
		}
		_ = sp.Rollback(ctx) // ROLLBACK TO SAVEPOINT — the outer tx stays usable
		if isUniqueViolation(err) {
			return true, ErrAccountNameTaken
		}
		return false, fmt.Errorf("store: update account (write): %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("store: update account (release): %w", err)
	}
	if patch.Connection != nil {
		// New server or credentials: a receive-only verdict about the old ones
		// (ADR-046 §12) says nothing about these. The next send finds out.
		if _, err := tx.ClearSendBlocked(ctx, id); err != nil {
			return false, err
		}
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, id, OpUpdated); err != nil {
		return false, err
	}
	return true, nil
}

// DeleteAccount destroys an account, appends the change_log rows the cascade
// owes, and returns the content hashes whose app.blobs row it reclaimed — which
// the caller must delete the FILES for in THIS transaction. found is false when
// the account does not exist for the caller.
//
// Cascade (spec §12, "remove an account"): the account's mailboxes, message
// provenance, identities, and submissions are all FK ON DELETE CASCADE off
// app.accounts, so deleting the account row removes that whole account-scoped
// surface. This ALSO hard-deletes the messages that lived ONLY in this account —
// a message whose every provenance row (and any local/moved placement) is this
// account's. Without that, the cascade would strip a message of all mailbox
// membership yet leave the app.messages row, and Email/query (which reads
// app.messages directly, not gated on provenance) would still list it: a ghost
// in no mailbox. A message shared with another account via content-hash dedup
// (ADR-001) is KEPT — it still lives there.
//
// Ordering: every id the change rows and the reclaim need is captured FIRST
// (while provenance and identities still resolve), THEN the account row is
// deleted (its cascade removes this account's submissions / identities /
// provenance / mailboxes / compose snippets and NULLs the keys those identities
// held), and ONLY THEN are the orphaned messages deleted. Messages go last
// because their delete must not race the cascade that owns the rows pointing at
// them; the explicit submissions delete just before it clears a rare
// cross-account draft submission whose account is NOT the one being removed (and
// which the cascade therefore never touches).
//
// # WHAT THIS EMITS, AND WHAT IT DELIBERATELY DOES NOT (#299)
//
// The audit found this method documenting exactly two deferrals while silently
// destroying five JMAP-visible types. Every type is now accounted for here.
//
// EMITTED, because each is bounded by construction and each is something a
// second client is actively rendering right now:
//
//   - Mailbox `destroyed`, per mailbox. An account has tens of folders, and the
//     folder list is the most visible thing on screen; without this a second
//     client polls Mailbox/changes, is told "nothing changed", and keeps
//     offering the deleted account's folders forever.
//   - Identity `destroyed`, per identity. A handful per account, and a stale
//     send-as identity is worse than a stale folder: choosing it composes mail
//     that cannot be sent.
//   - ComposeSnippet `destroyed`, per snippet bound to one of those identities
//     (compose_snippets.identity_id ON DELETE CASCADE). Signatures for an
//     address that no longer exists.
//   - Key `updated`, per key bound to one of those identities
//     (user_keys.identity_id ON DELETE SET NULL). The key SURVIVES — only its
//     binding is cleared — and BindKeyToIdentity established that binding as
//     JMAP-visible Key state, so unbinding owes the same row it did.
//
// DEFERRED, with the reason and the observable consequence stated rather than
// left to be discovered:
//
//   - Per-Email `destroyed` is NOT fanned out. An account may hold 10^6
//     messages and this runs inside a JMAP request; the same call DeleteLabel
//     makes for its per-Email keyword changes. WHAT A SECOND CLIENT OBSERVES:
//     Email/changes keeps returning nothing while its cached mail no longer
//     exists, until it re-queries — which the Account `destroyed` row it DOES
//     receive is the trigger for, since every mail view is scoped to an account.
//   - EmailSubmission `destroyed` is NOT fanned out, for the same reason and
//     one more: a submission is meaningless without the Email it sent, whose
//     destruction is already deferred above, so emitting one without the other
//     would tell a client half a story. WHAT A SECOND CLIENT OBSERVES: stale
//     outbound history for the removed account until the same re-query.
//   - Thread rows are not emitted. Threads are not account-scoped and a thread
//     that lost all its messages here is reached through the same re-query.
//
// The blob METADATA rows of the removed messages ARE reclaimed, through the one
// shared guard (blobreclaim.go). ⚠ THE COMMENT THIS REPLACES SAID THEY WERE
// RECLAIMED BY "THE BACKGROUND PURGE REAPER", AND NO SUCH REAPER CAN REACH THEM:
// DueForPurge only ever scans `trashed_at IS NOT NULL`, and a row deleted here is
// invisible to every sweep there is. That sentence is how two of the four orphan
// app.blobs rows the audit counted came to exist (#300).
func (tx *UserTx) DeleteAccount(ctx context.Context, id ulid.ULID) (found bool, reclaim [][]byte, err error) {
	// Capture the messages that live ONLY in this account, BEFORE the cascade
	// removes the provenance that identifies them, WITH the bytes they hold — the
	// hash cannot be read back once the row is gone. Scoped by account_id, so if
	// the account is not the caller's this matches nothing.
	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, m.content_hash FROM app.messages m
		 WHERE m.user_id = $1
		   AND (EXISTS (SELECT 1 FROM app.message_provenance p
		                 WHERE p.user_id = $1 AND p.message_id = m.id AND p.account_id = $2)
		     OR m.moved_mailbox_id IN (SELECT id FROM app.mailboxes
		                                WHERE user_id = $1 AND account_id = $2)
		     OR m.local_mailbox_id IN (SELECT id FROM app.mailboxes
		                                WHERE user_id = $1 AND account_id = $2))
		   AND NOT EXISTS (SELECT 1 FROM app.message_provenance p
		                WHERE p.user_id = $1 AND p.message_id = m.id AND p.account_id <> $2)`,
		tx.userID.String(), id.String())
	if err != nil {
		return false, nil, fmt.Errorf("store: delete account (candidate messages): %w", err)
	}
	var (
		orphans      []string
		orphanHashes [][]byte
	)
	for rows.Next() {
		var (
			mid  string
			hash []byte
		)
		if err := rows.Scan(&mid, &hash); err != nil {
			rows.Close()
			return false, nil, fmt.Errorf("store: delete account (scan candidate): %w", err)
		}
		orphans = append(orphans, mid)
		orphanHashes = append(orphanHashes, hash)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("store: delete account (candidate messages): %w", err)
	}

	// The cascade's JMAP-visible casualties, captured while they still resolve.
	cascaded, err := tx.accountCascadeVictims(ctx, id)
	if err != nil {
		return false, nil, err
	}

	var deleted string
	err = tx.tx.QueryRow(ctx, `
		DELETE FROM app.accounts WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil, nil // the account was not the caller's — leave messages alone
		}
		return false, nil, fmt.Errorf("store: delete account: %w", err)
	}

	if len(orphans) > 0 {
		// Any submission still pointing at a captured message. This account's own
		// submissions are already gone via the cascade above; this clears a rare
		// cross-account draft submission, whose own account survives and therefore
		// never cascaded.
		if _, err := tx.tx.Exec(ctx,
			`DELETE FROM app.submissions WHERE user_id = $1 AND message_id = ANY($2)`,
			tx.userID.String(), orphans); err != nil {
			return false, nil, fmt.Errorf("store: delete account (orphan submissions): %w", err)
		}
		if _, err := tx.tx.Exec(ctx,
			`DELETE FROM app.messages WHERE user_id = $1 AND id = ANY($2)`,
			tx.userID.String(), orphans); err != nil {
			return false, nil, fmt.Errorf("store: delete account (orphan messages): %w", err)
		}
		if reclaim, err = tx.reclaimUnreferencedBlobs(ctx, orphanHashes); err != nil {
			return false, nil, err
		}
	}

	if _, err := tx.AppendChange(ctx, ObjectAccount, id, OpDestroyed); err != nil {
		return false, nil, err
	}
	for _, mb := range cascaded.mailboxes {
		if _, err := tx.AppendChange(ctx, ObjectMailbox, mb, OpDestroyed); err != nil {
			return false, nil, err
		}
	}
	for _, ident := range cascaded.identities {
		if _, err := tx.AppendChange(ctx, ObjectIdentity, ident, OpDestroyed); err != nil {
			return false, nil, err
		}
	}
	for _, sn := range cascaded.snippets {
		if _, err := tx.AppendChange(ctx, ObjectComposeSnippet, sn, OpDestroyed); err != nil {
			return false, nil, err
		}
	}
	for _, k := range cascaded.keys {
		if _, err := tx.AppendChange(ctx, ObjectKey, k, OpUpdated); err != nil {
			return false, nil, err
		}
	}
	return true, reclaim, nil
}

// accountCascadeVictims is the set of JMAP-visible objects that deleting one
// account destroys or mutates THROUGH A FOREIGN KEY rather than through a
// statement anyone can see (#299).
//
// It exists because a cascade is invisible at the call site: `DELETE FROM
// app.accounts` is one line, and four object types quietly change underneath it.
// Reading them first is the only way to name them in the change log afterwards —
// once the cascade has run there is nothing left to enumerate.
type accountCascade struct {
	mailboxes  []ulid.ULID
	identities []ulid.ULID
	snippets   []ulid.ULID // compose_snippets.identity_id ON DELETE CASCADE
	keys       []ulid.ULID // user_keys.identity_id ON DELETE SET NULL — the key SURVIVES
}

func (tx *UserTx) accountCascadeVictims(ctx context.Context, accountID ulid.ULID) (accountCascade, error) {
	var out accountCascade
	read := func(label, sql string) ([]ulid.ULID, error) {
		rows, err := tx.tx.Query(ctx, sql, tx.userID.String(), accountID.String())
		if err != nil {
			return nil, fmt.Errorf("store: delete account (%s): %w", label, err)
		}
		defer rows.Close()
		var ids []ulid.ULID
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, fmt.Errorf("store: delete account (scan %s): %w", label, err)
			}
			u, perr := ulid.Parse(s)
			if perr != nil {
				return nil, fmt.Errorf("store: delete account (%s id %q): %w", label, s, perr)
			}
			ids = append(ids, u)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: delete account (%s): %w", label, err)
		}
		return ids, nil
	}

	var err error
	if out.mailboxes, err = read("cascaded mailboxes",
		`SELECT id FROM app.mailboxes WHERE user_id = $1 AND account_id = $2`); err != nil {
		return accountCascade{}, err
	}
	if out.identities, err = read("cascaded identities",
		`SELECT id FROM app.identities WHERE user_id = $1 AND account_id = $2`); err != nil {
		return accountCascade{}, err
	}
	// Snippets and keys hang off the IDENTITIES, not off the account — a second
	// hop, which is exactly why they were missed. Expressed as a subquery rather
	// than by feeding the ids back in, so the two reads cannot disagree.
	if out.snippets, err = read("cascaded compose snippets",
		`SELECT id FROM app.compose_snippets
		  WHERE user_id = $1
		    AND identity_id IN (SELECT id FROM app.identities
		                         WHERE user_id = $1 AND account_id = $2)`); err != nil {
		return accountCascade{}, err
	}
	if out.keys, err = read("cascaded key bindings",
		`SELECT id FROM app.user_keys
		  WHERE user_id = $1
		    AND identity_id IN (SELECT id FROM app.identities
		                         WHERE user_id = $1 AND account_id = $2)`); err != nil {
		return accountCascade{}, err
	}
	return out, nil
}

// firstNonEmptyStr returns a, or b when a is empty.
func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// defaultPort returns p, or def when p is non-positive.
func defaultPort(p, def int) int {
	if p <= 0 {
		return def
	}
	return p
}

// SetAccountSyncEnabled toggles an account's sync_enabled flag (spec §16:
// pause/resume) and appends the ObjectAccount change_log row (the invariant —
// an account is JMAP-visible). A missing account is an error. The guarded UPDATE
// only touches — and only logs — when the flag actually flips, so a redundant
// call is a true no-op. It is the narrow toggle the boot onboarding path uses to
// disable sync on a validation failure; the JMAP Account/set update path goes
// through UpdateAccount.
func (tx *UserTx) SetAccountSyncEnabled(ctx context.Context, id ulid.ULID, enabled bool) error {
	var exists bool
	err := tx.tx.QueryRow(ctx, `
		SELECT true FROM app.accounts WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String()).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: set account sync enabled %s: %w", id, ErrAccountNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: set account sync enabled (read): %w", err)
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.accounts SET sync_enabled = $3
		 WHERE user_id = $1 AND id = $2 AND sync_enabled IS DISTINCT FROM $3`,
		tx.userID.String(), id.String(), enabled)
	if err != nil {
		return fmt.Errorf("store: set account sync enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // no-op: already in the requested state
	}
	if _, err := tx.AppendChange(ctx, ObjectAccount, id, OpUpdated); err != nil {
		return err
	}
	return nil
}

// queryAccounts runs a multi-row account query and scans the results.
func (tx *UserTx) queryAccounts(ctx context.Context, sql string, args ...any) ([]Account, error) {
	rows, err := tx.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list accounts: %w", err)
	}
	return out, nil
}

// scanAccount scans one account row in accountColumns order.
func scanAccount(row pgx.Row) (Account, error) {
	var (
		a      Account
		idStr  string
		dekStr string
	)
	if err := row.Scan(&idStr, &a.Name, &a.ProviderProfile, &a.IMAPHost, &a.IMAPPort,
		&a.SMTPHost, &a.SMTPPort, &a.AuthKind, &a.CredentialsCT, &dekStr,
		&a.EnvelopeVer, &a.SyncEnabled, &a.OutboundDailyQuota, &a.Color,
		&a.WritebackEnabled, &a.NotifyPush, &a.AuthState); err != nil {
		return Account{}, err
	}
	id, err := ulid.Parse(idStr)
	if err != nil {
		return Account{}, fmt.Errorf("parse account id %q: %w", idStr, err)
	}
	dek, err := ulid.Parse(dekStr)
	if err != nil {
		return Account{}, fmt.Errorf("parse credentials dek %q: %w", dekStr, err)
	}
	a.ID = id
	a.CredentialsDEK = dek
	return a, nil
}
