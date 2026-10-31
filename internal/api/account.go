// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/oauthmail"
	"mailroom/internal/onboarding"
	"mailroom/internal/store"
)

// Account method handlers for the `urn:mailroom:accounts` extension (spec §12,
// mailroom#98/#45): in-UI management of a user's own IMAP/SMTP mail accounts.
// Today accounts are also declared in the deployment's accounts.json and
// reconciled at boot; this surface lets any provisioned user add, edit, and
// remove their own accounts without it.
//
// An Account is a JMAP-visible object (store.ObjectAccount), so Account/set's
// creates/updates/destroys each append exactly one change_log row in the SAME
// UserTx as the mutation (the store enforces it) — Account/changes then feeds
// client sync cursors like any other type.
//
// SECURITY — credentials are write-only and sealed SERVER-side. The client sends
// the IMAP/SMTP password ONCE, on create or a connection change; the server
// seals it with the user's derived credentials key (AccountSecrets, the SAME
// derivation onboarding and compose use — never a second DEK path) and it is
// NEVER returned by Account/get and NEVER logged. A cross-user Account leak would
// expose another user's mail-server credentials, so RLS isolation here is a hard
// boundary (rls_integration_test.go probes every Account/* method).

// AccountSecrets seals an account's IMAP/SMTP credentials server-side, under the
// user's derived credentials key. It is deliberately the ONLY thing the API
// layer knows about the key hierarchy: cmd/mailroom wires it from the same
// keyResolver that seals credentials at onboarding and opens them at sync time,
// so there is exactly one credentials-DEK derivation path (spec §9).
type AccountSecrets interface {
	// SealAccountCredentials envelope-encrypts creds for userID and returns the
	// ciphertext plus the credentials-scheme sentinel to store in credentials_dek.
	SealAccountCredentials(ctx context.Context, userID ulid.ULID, creds onboarding.Credentials) (ct []byte, scheme ulid.ULID, err error)
}

// WithAccountSecrets enables Account/set credential sealing. Without it the read
// and metadata paths still work, but a create (or a connection-changing update)
// returns notImplemented rather than storing an unsealed secret.
func WithAccountSecrets(s AccountSecrets) Option {
	return func(a *API) { a.accountSecrets = s }
}

// WithAccountDialer overrides the IMAP dialer Account/set create/validation uses
// (default onboarding's sync.Dial). It exists for tests that inject a fake IMAP
// server at the onboarding seam.
func WithAccountDialer(d onboarding.Dialer) Option {
	return func(a *API) { a.accountDialer = d }
}

// WithAllowPrivateAccountHosts widens the account-validation egress guard to
// permit private/loopback IMAP hosts (#527, config.AllowPrivateAccountHosts).
// Default false — Account/set refuses them, which is what stops it being a
// port-scan oracle against everything the container can reach.
func WithAllowPrivateAccountHosts(allow bool) Option {
	return func(a *API) { a.accountAllowPrivate = allow }
}

// accountValidateOpts is the option set every Account/set validation dial uses.
// It exists so the two call sites cannot drift: a guard applied at one of them
// and forgotten at the other is the shape of defect #527 is about.
func (a *API) accountValidateOpts() []onboarding.Option {
	return []onboarding.Option{onboarding.WithAllowPrivateHosts(a.accountAllowPrivate)}
}

// setValidationFailed is the Mailroom Account/set per-object error for a create
// or connection change whose IMAP endpoint/credentials could not be validated
// (dial or login failed, or the folder list could not be read). It is a clear,
// per-object /set outcome — not a 500 — so a batch reports the one bad account
// and keeps going. The description never carries the password.
const setValidationFailed = "validationFailed"

// accountObject renders a stored account as a JMAP Account object, honoring a
// requested property subset (id is always present). It NEVER carries the
// password or username — those are write-only credential fields (username lives
// only inside the sealed secret in 0.x; the client set it and does not need it
// echoed back). Endpoints and the sync flag are the manageable, non-secret
// surface.
func accountObject(a store.Account, g *store.OAuthGrant, props map[string]bool) map[string]any {
	full := map[string]any{
		"id":               a.ID.String(),
		"name":             a.Name,
		"providerProfile":  a.ProviderProfile,
		"imapHost":         a.IMAPHost,
		"imapPort":         a.IMAPPort,
		"smtpHost":         a.SMTPHost,
		"smtpPort":         a.SMTPPort,
		"syncEnabled":      a.SyncEnabled,
		"writebackEnabled": a.WritebackEnabled,
		// notifyPush (#59): whether new mail in this account sends Web Push to
		// the user's devices at all. Settable; default true.
		"notifyPush": a.NotifyPush,
	}
	// outboundDailyQuota is the provider's static per-account send cap, exposed
	// READ-ONLY (the live send meter is EmailSubmission/getSendQuota, 0.9.0), so
	// Account/set rejects any attempt to set it.
	full["outboundDailyQuota"] = a.OutboundDailyQuota
	// color is the user-chosen presentation tint and the only settable
	// presentation prop; it is JSON null when unset.
	if a.Color != nil {
		full["color"] = *a.Color
	} else {
		full["color"] = nil
	}
	// How the account signs in (#52, ADR-046 §5), all four READ-ONLY.
	for k, v := range accountAuthProps(a, g) {
		full[k] = v
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": a.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// Account auth states as Account/get reports them (authState). The first three
// are app.oauth_grants.state verbatim; "disconnected" is an OAuth account with
// no grant (Account/oauthDisconnect), which reconnects through
// Account/oauthStart {id}.
const authStateDisconnected = "disconnected"

// accountAuthProps is the sign-in half of an Account object (#52):
//
//   - authKind: "password" or "oauth2" (app.accounts.auth_kind).
//   - authState: null for a password account; for an OAuth one "active",
//     "needs_reauth" (the user must sign in at the provider again),
//     "operator_error" (the provider refused this SERVER; only its operator
//     can fix it) or "disconnected".
//   - authProvider: "google" or "microsoft" for an OAuth account, else null.
//   - authAddress: the address the provider vouched for, from the grant; null
//     without one.
//
// The change_log invariant holds without a new write path: these change only
// when the grant is created, re-authenticated, changes state or is deleted,
// and every one of those appends the Account change row in its own
// transaction (store/oauth_grants.go, oauth_connect.go). Refresh-token
// rotation touches none of them and appends nothing (ADR-046 §4).
func accountAuthProps(a store.Account, g *store.OAuthGrant) map[string]any {
	out := map[string]any{"authKind": a.AuthKind, "authState": nil, "authProvider": nil, "authAddress": nil}
	if a.AuthKind != "oauth2" {
		return out
	}
	provider := func() any {
		if p := oauthmail.ProviderForProfile(a.ProviderProfile); p != "" {
			return p
		}
		return nil
	}
	switch {
	case g != nil:
		out["authState"], out["authProvider"], out["authAddress"] = g.State, g.Provider, g.Address
	case a.AuthState != "":
		// A caller that read the account without its grant (a just-created
		// object): the state is on the row; the provider follows from it.
		out["authState"], out["authProvider"] = a.AuthState, provider()
	default:
		out["authState"], out["authProvider"] = authStateDisconnected, provider()
	}
	return out
}

// ---- Account/get ----------------------------------------------------------

type accountGetArgs struct {
	AccountID  string    `json:"accountId"`
	IDs        *[]string `json:"ids"`
	Properties []string  `json:"properties"`
}

func (a *API) accountGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args accountGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	var props map[string]bool
	if args.Properties != nil {
		props = make(map[string]bool, len(args.Properties))
		for _, p := range args.Properties {
			props[p] = true
		}
	}

	var (
		state     int64
		byID      = make(map[ulid.ULID]store.Account)
		grants    map[ulid.ULID]store.OAuthGrant
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var accounts []store.Account
		var err error
		if args.IDs == nil {
			// ids:null means "all accounts" (RFC 8620 §5.1).
			accounts, err = tx.ListAccounts(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, ac := range accounts {
				requested = append(requested, requestedID{raw: ac.ID.String(), id: ac.ID, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			accounts, err = tx.GetAccounts(ctx, valid)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		ids := make([]ulid.ULID, 0, len(accounts))
		for _, ac := range accounts {
			byID[ac.ID] = ac
			ids = append(ids, ac.ID)
		}
		if grants, err = tx.OAuthGrantsFor(ctx, ids); err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		state, err = tx.ChangeState(ctx, store.ObjectAccount)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}

	list := make([]map[string]any, 0, len(requested))
	for _, req := range requested {
		if !req.ok {
			continue
		}
		ac, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		var g *store.OAuthGrant
		if gr, ok := grants[ac.ID]; ok {
			g = &gr
		}
		list = append(list, accountObject(ac, g, props))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// ---- Account/query --------------------------------------------------------

// accountQuery returns every account id in the store's stable name order.
// Filtering/sorting is not offered (the set is tiny); the response is a
// well-formed Foo/query the client windows itself.
func (a *API) accountQuery(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var probe struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rawArgs, &probe); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, probe.AccountID); mErr != nil {
		return nil, mErr
	}

	var (
		ids   []ulid.ULID
		state int64
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		accounts, err := tx.ListAccounts(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		ids = make([]ulid.ULID, len(accounts))
		for i, ac := range accounts {
			ids[i] = ac.ID
		}
		state, err = tx.ChangeState(ctx, store.ObjectAccount)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	})
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(queryResult{
		AccountID:           userID.String(),
		QueryState:          encodeState(state),
		CanCalculateChanges: false,
		Position:            0,
		Total:               len(ids),
		IDs:                 ulidStrings(ids),
	}), nil
}

// ---- Account/changes ------------------------------------------------------

func (a *API) accountChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectAccount)
}

// ---- Account/set ----------------------------------------------------------

// accountCreate is the parsed create form. name/imapHost/smtpHost/username/
// password are required; ports default to implicit-TLS 993/465; syncEnabled
// defaults to true. password (and username) are write-only.
type accountCreate struct {
	Name            *string `json:"name"`
	ProviderProfile *string `json:"providerProfile"`
	IMAPHost        *string `json:"imapHost"`
	IMAPPort        *int    `json:"imapPort"`
	SMTPHost        *string `json:"smtpHost"`
	SMTPPort        *int    `json:"smtpPort"`
	Username        *string `json:"username"`
	Password        *string `json:"password"`
	SyncEnabled     *bool   `json:"syncEnabled"`
	Color           *string `json:"color"`
	// WritebackEnabled is ADR-028's per-account ingest-only switch. A POINTER,
	// like SyncEnabled: an absent property must take the schema default (true),
	// not silently create a shadowed account nobody asked to shadow.
	WritebackEnabled *bool `json:"writebackEnabled"`
}

// preparedCreate is a create object after the OUT-OF-TRANSACTION work (seal +
// IMAP validation) has run: either a ready-to-insert row, or the SetError to
// report. Doing the network dial and the sealing before opening the write tx
// keeps a multi-second IMAP round-trip out of the database transaction.
type preparedCreate struct {
	row          store.AccountRow
	identityAddr string // default fixed send-as identity to create (username, if an address)
	syncEnabled  bool
	err          *SetError
}

// preparedUpdate is an update object after any out-of-transaction work. patch is
// the store patch to apply (its Connection, when set, already carries the
// re-sealed, re-validated credentials); err is the SetError to report instead.
type preparedUpdate struct {
	patch store.AccountPatch
	err   *SetError
}

func (a *API) accountSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // shared {accountId, ifInState, create, update, destroy} shape
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	res := setResult{
		AccountID:    userID.String(),
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}

	// Phase 1 — OUTSIDE any transaction: parse, seal, and validate (dial IMAP)
	// every create and connection-changing update. Network I/O and key work
	// happen here so the write transaction below never holds a database
	// connection open across an IMAP round-trip.
	preparedCreates := make(map[string]preparedCreate, len(args.Create))
	for cid, raw := range args.Create {
		preparedCreates[cid] = a.prepareAccountCreate(ctx, userID, raw)
	}
	preparedUpdates := make(map[string]preparedUpdate, len(args.Update))
	for id, patch := range args.Update {
		preparedUpdates[id] = a.prepareAccountUpdate(ctx, userID, id, patch)
	}
	// Security review M1 (#897): an OAuth account's grant is revoked at the
	// provider when the account is deleted. What to revoke is read HERE,
	// before the delete cascades the grant and the sealed token away; the
	// revocation itself runs after the commit, only for accounts actually
	// destroyed, and can never fail or block the delete.
	revokers := map[string]func(context.Context){}
	if a.oauth != nil {
		for _, id := range args.Destroy {
			if aid, err := ulid.Parse(id); err == nil {
				if fn := a.oauth.PrepareRevoke(ctx, userID, aid); fn != nil {
					revokers[id] = fn
				}
			}
		}
	}

	// Phase 2 — ONE UserTx brackets oldState/newState and every mutation; a
	// per-object failure still commits the successful ones. A stateMismatch
	// aborts the whole method (RFC 8620 §5.3).
	var (
		stateErr *MethodError
		created  = map[string]ulid.ULID{}
	)
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		oldState, err := tx.ChangeState(ctx, store.ObjectAccount)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		for cid, raw := range args.Create {
			_ = raw
			pc := preparedCreates[cid]
			if pc.err != nil {
				res.NotCreated[cid] = *pc.err
				continue
			}
			id, cerr := tx.CreateAccount(ctx, pc.row)
			if cerr != nil {
				if errors.Is(cerr, store.ErrAccountNameTaken) {
					res.NotCreated[cid] = SetError{Type: setInvalidProps,
						Description: "an account with that name already exists", Properties: []string{"name"}}
					continue
				}
				return cerr
			}
			// A default fixed send-as identity for the login address, mirroring the
			// boot reconcile (spec §5): a fresh account can send without extra setup.
			// "already exists" is success; a real failure aborts the batch.
			if pc.identityAddr != "" {
				if _, ierr := tx.CreateIdentity(ctx, store.IdentityRow{
					AccountID: id, Kind: store.IdentityFixed, Address: pc.identityAddr, Enabled: true,
				}); ierr != nil && !errors.Is(ierr, store.ErrIdentityExists) {
					return ierr
				}
			}
			created[cid] = id
			obj, oerr := accountObjectByID(ctx, tx, id)
			if oerr != nil {
				return oerr
			}
			res.Created[cid] = obj
		}

		for id := range args.Update {
			aid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			pu := preparedUpdates[id]
			if pu.err != nil {
				res.NotUpdated[id] = *pu.err
				continue
			}
			// The Gmail sync hold (oauthmail/synchold.go): switching sync ON
			// for a held account is refused, in this transaction, against the
			// row as it is now. Switching it off, or any other edit, passes.
			if pu.patch.SyncEnabled != nil && *pu.patch.SyncEnabled {
				if cur, gerr := tx.GetAccount(ctx, aid); gerr == nil {
					if code, msg, held := oauthmail.SyncHold(cur.AuthKind, cur.ProviderProfile); held {
						res.NotUpdated[id] = SetError{Type: setInvalidProps,
							Description: "sync cannot be switched on yet: " + msg + " (" + code + ")",
							Properties:  []string{"syncEnabled"}}
						continue
					}
				}
			}
			// The Gmail writeback hold (oauthmail.WritebackHold, ADR-047 §3):
			// a Gmail OAuth account stays ingest-only until Gmail writeback
			// exists. Switching writeback OFF always passes.
			if pu.patch.WritebackEnabled != nil && *pu.patch.WritebackEnabled {
				// FAIL CLOSED (#52 review): an account whose hold cannot be
				// checked is not switched on.
				cur, gerr := tx.GetAccount(ctx, aid)
				if gerr != nil {
					if errors.Is(gerr, store.ErrAccountNotFound) {
						res.NotUpdated[id] = SetError{Type: setNotFound}
					} else {
						res.NotUpdated[id] = SetError{Type: setForbidden,
							Description: "writeback cannot be switched on: the account could not be checked",
							Properties:  []string{"writebackEnabled"}}
					}
					continue
				}
				if code, msg, held := oauthmail.WritebackHold(cur.AuthKind, cur.ProviderProfile); held {
					res.NotUpdated[id] = SetError{Type: setInvalidProps,
						Description: "writeback cannot be switched on yet: " + msg + " (" + code + ")",
						Properties:  []string{"writebackEnabled"}}
					continue
				}
			}
			found, uerr := tx.UpdateAccount(ctx, aid, pu.patch)
			if uerr != nil {
				if errors.Is(uerr, store.ErrAccountNameTaken) {
					res.NotUpdated[id] = SetError{Type: setInvalidProps,
						Description: "an account with that name already exists", Properties: []string{"name"}}
					continue
				}
				if errors.Is(uerr, store.ErrOAuthConnectionImmutable) {
					// The store's guarantee behind the earlier refusal in
					// prepareAccountUpdate (an account that became OAuth in
					// between, or a creation reference).
					res.NotUpdated[id] = *oauthConnectionImmutableErr(nil)
					continue
				}
				return uerr
			}
			if !found {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			// ADR-028 §4: switching writeback OFF suppresses the outstanding backlog
			// in THIS SAME transaction as the flag flip. Suppression at enqueue time
			// only governs intents created AFTER this moment — anything already
			// 'pending' from before the switch is untouched by EnqueueIntent's guard
			// and would fire on the very next writeback pass, so the account would
			// stay shadowed in name only while its backlog silently drained. An
			// enqueue guard and an apply guard are different guarantees (#521 q2),
			// and this closes the gap between them.
			if pu.patch.WritebackEnabled != nil && !*pu.patch.WritebackEnabled {
				n, serr := tx.SuppressAccountIntents(ctx, aid)
				if serr != nil {
					return serr
				}
				slog.InfoContext(ctx, "account/set: suppressed outstanding writeback intents on ingest-only switch",
					"account_id", aid.String(), "suppressed", n)
			}
			res.Updated[id] = nil
		}

		for _, id := range args.Destroy {
			aid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, reclaim, derr := tx.DeleteAccount(ctx, aid)
			if derr != nil {
				return derr
			}
			// The blob FILES of the messages that lived only in this account,
			// inside this transaction (#300) — see reclaimBlobFiles.
			if derr := a.reclaimBlobFiles(ctx, userID, reclaim); derr != nil {
				return derr
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectAccount)
		if err != nil {
			return err
		}
		res.NewState = encodeState(newState)
		return nil
	})
	if stateErr != nil {
		return nil, stateErr
	}
	if err != nil && err != errMethodRollback {
		return nil, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	// ADR-046 §9 obligation (b): a deleted account's grant went with it (ON
	// DELETE CASCADE), so no access token for it may outlive it in memory.
	// After the commit, so a rolled-back destroy forgets nothing it still needs
	// (and forgetting is harmless for a password account: there is nothing to
	// drop).
	if a.oauth != nil {
		for _, id := range res.Destroyed {
			if aid, ok := resolveSetID(id, created); ok {
				a.oauth.Forget(userID, aid)
			}
			if fn := revokers[id]; fn != nil {
				fn(ctx)
			}
		}
	}
	return mustMarshal(res), nil
}

// prepareAccountCreate does the out-of-transaction work for one create: parse,
// build the onboarding spec, seal the credentials, and validate by dialing IMAP.
// It returns a preparedCreate carrying either the ready row or the SetError.
func (a *API) prepareAccountCreate(ctx context.Context, userID ulid.ULID, raw json.RawMessage) preparedCreate {
	var c accountCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return preparedCreate{err: &SetError{Type: setInvalidProps, Description: err.Error()}}
	}
	spec, serr := accountSpecFromCreate(c)
	if serr != nil {
		return preparedCreate{err: serr}
	}
	if a.accountSecrets == nil {
		return preparedCreate{err: &SetError{Type: setNotImplemented,
			Description: "account management is not enabled on this server"}}
	}
	// ADR-028 §2, the same refusal the update path makes. A create that asks for
	// writebackEnabled:true while the instance flag holds would store a value
	// that never takes effect, and Account/get would then report writeback as
	// enabled on an account that cannot write — the "setting that means its own
	// opposite" defect ADR-020 already paid for once. Refuse both doors, not one.
	if c.WritebackEnabled != nil && *c.WritebackEnabled && a.store.WritebackDisabled() {
		return preparedCreate{err: &SetError{Type: setInvalidProps,
			Description: "writeback is disabled instance-wide by configuration " +
				"(MAILROOM_WRITEBACK_DISABLED) and cannot be enabled per-account",
			Properties: []string{"writebackEnabled"}}}
	}

	// Seal first (cheap, local), then validate (network). A seal failure is a
	// server fault, reported per-object so the batch survives; the description is
	// deliberately generic and never carries the secret.
	ct, scheme, err := a.accountSecrets.SealAccountCredentials(ctx, userID, credentialsFromSpec(spec))
	if err != nil {
		return preparedCreate{err: &SetError{Type: setForbidden, Description: "could not seal account credentials"}}
	}
	if _, err := onboarding.ValidateAccount(ctx, a.accountDialer, spec, a.accountValidateOpts()...); err != nil {
		return preparedCreate{err: &SetError{Type: setValidationFailed,
			Description: "could not validate the account against its IMAP server: " + err.Error()}}
	}

	syncEnabled := true
	if c.SyncEnabled != nil {
		syncEnabled = *c.SyncEnabled
	}
	// color is optional on create; when supplied it must be #RRGGBB (a JSON null
	// is indistinguishable from absent here and both mean "no color").
	var color *string
	if c.Color != nil {
		if !validHexColor(*c.Color) {
			return preparedCreate{err: invalidColorErr()}
		}
		color = c.Color
	}
	return preparedCreate{
		row: store.AccountRow{
			Name:             spec.Name,
			ProviderProfile:  spec.ProviderProfile,
			IMAPHost:         spec.IMAPHost,
			IMAPPort:         spec.IMAPPort,
			SMTPHost:         spec.SMTPHost,
			SMTPPort:         spec.SMTPPort,
			AuthKind:         "password",
			CredentialsCT:    ct,
			CredentialsDEK:   scheme,
			SyncEnabled:      syncEnabled,
			Color:            color,
			WritebackEnabled: c.WritebackEnabled,
		},
		identityAddr: defaultIdentityAddr(spec.Username),
		syncEnabled:  syncEnabled,
	}
}

// validHexColor reports whether s is a "#RRGGBB" hex color (the only accepted
// color form on the Account object). Case-insensitive on the hex digits.
func validHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// invalidColorErr is the shared per-object SetError for a malformed color — a
// clean notCreated/notUpdated outcome, never a 500.
func invalidColorErr() *SetError {
	return &SetError{Type: setInvalidProps,
		Description: "color must be null or a #RRGGBB hex string", Properties: []string{"color"}}
}

// prepareAccountUpdate does the out-of-transaction work for one update: parse
// the patch, classify it as a metadata-only edit (name/syncEnabled — no dial) or
// a connection change, and for a connection change re-seal + re-validate against
// IMAP. A connection change MUST re-supply the full endpoint block and the
// credentials (host/port are non-secret and the client already has them from
// Account/get; username/password are re-entered), so the server never needs the
// old plaintext secret.
func (a *API) prepareAccountUpdate(ctx context.Context, userID ulid.ULID, id string, rawPatch json.RawMessage) preparedUpdate {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawPatch, &raw); err != nil {
		return preparedUpdate{err: &SetError{Type: setInvalidPatch, Description: err.Error()}}
	}

	var patch store.AccountPatch
	connChange := false
	var connKeys []string
	for key := range raw {
		switch key {
		case "name", "syncEnabled", "color", "writebackEnabled", "notifyPush":
		case "providerProfile", "imapHost", "imapPort", "smtpHost", "smtpPort", "username", "password":
			connChange = true
			connKeys = append(connKeys, key)
		case "id":
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: "id is immutable", Properties: []string{"id"}}}
		case "authKind", "authState", "authProvider", "authAddress":
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: key + " is read-only: it changes by connecting, reconnecting or disconnecting the account",
				Properties:  []string{key}}}
		default:
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: "unsupported Account property in patch: " + key, Properties: []string{key}}}
		}
	}

	if v, ok := raw["name"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || s == "" {
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: "name must be a non-empty string", Properties: []string{"name"}}}
		}
		patch.Name = &s
	}
	if v, ok := raw["syncEnabled"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: "syncEnabled must be a boolean", Properties: []string{"syncEnabled"}}}
		}
		patch.SyncEnabled = &b
	}
	if v, ok := raw["writebackEnabled"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: "writebackEnabled must be a boolean", Properties: []string{"writebackEnabled"}}}
		}
		// ADR-028 §2: the instance-level MAILROOM_WRITEBACK_DISABLED flag ANDs into
		// every account's effective writeback, and the API must not let a per-account
		// setting quietly disagree with it. Silently accepting writebackEnabled:true
		// here would store a value that never takes effect while the instance flag
		// holds — exactly the "a setting that means its own opposite" defect
		// ADR-020 already named once. Refuse it outright rather than lie in Account/get.
		if b && a.store.WritebackDisabled() {
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: "writeback is disabled instance-wide by configuration " +
					"(MAILROOM_WRITEBACK_DISABLED) and cannot be enabled per-account",
				Properties: []string{"writebackEnabled"}}}
		}
		patch.WritebackEnabled = &b
	}
	if v, ok := raw["notifyPush"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return preparedUpdate{err: &SetError{Type: setInvalidProps,
				Description: "notifyPush must be a boolean", Properties: []string{"notifyPush"}}}
		}
		patch.NotifyPush = &b
	}
	if v, ok := raw["color"]; ok {
		// Present in the patch: apply it. null clears the color; a string must be
		// #RRGGBB. SetColor distinguishes this from a patch that omits color.
		patch.SetColor = true
		if !isJSONNull(v) {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return preparedUpdate{err: invalidColorErr()}
			}
			if !validHexColor(s) {
				return preparedUpdate{err: invalidColorErr()}
			}
			patch.Color = &s
		}
	}
	if !connChange {
		return preparedUpdate{patch: patch}
	}

	// ADR-046 §9 obligation (e): an OAuth account's connection is its
	// provider's and its grant's. Refused BEFORE anything is sealed or dialled,
	// so the user-supplied host never receives the password and the answer is
	// this clear error rather than a validation failure. (The store refuses it
	// again at the write, for an account that became OAuth in between.)
	if aid, err := ulid.Parse(id); err == nil {
		var kind string
		_ = a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
			acct, err := tx.GetAccount(ctx, aid)
			if err == nil {
				kind = acct.AuthKind
			}
			return nil // not found is the write path's setNotFound
		})
		if kind == "oauth2" {
			sort.Strings(connKeys)
			return preparedUpdate{err: oauthConnectionImmutableErr(connKeys)}
		}
	}

	// A connection change is validated and sealed exactly like a create.
	if a.accountSecrets == nil {
		return preparedUpdate{err: &SetError{Type: setNotImplemented,
			Description: "account management is not enabled on this server"}}
	}
	var c accountCreate
	if err := json.Unmarshal(rawPatch, &c); err != nil {
		return preparedUpdate{err: &SetError{Type: setInvalidPatch, Description: err.Error()}}
	}
	spec, serr := accountSpecFromCreate(c)
	if serr != nil {
		serr.Description = "changing connection settings requires re-supplying imapHost, smtpHost, " +
			"username and password: " + serr.Description
		return preparedUpdate{err: serr}
	}
	ct, scheme, err := a.accountSecrets.SealAccountCredentials(ctx, userID, credentialsFromSpec(spec))
	if err != nil {
		return preparedUpdate{err: &SetError{Type: setForbidden, Description: "could not seal account credentials"}}
	}
	if _, err := onboarding.ValidateAccount(ctx, a.accountDialer, spec, a.accountValidateOpts()...); err != nil {
		return preparedUpdate{err: &SetError{Type: setValidationFailed,
			Description: "could not validate the account against its IMAP server: " + err.Error()}}
	}
	patch.Connection = &store.AccountConnection{
		ProviderProfile: spec.ProviderProfile,
		IMAPHost:        spec.IMAPHost,
		IMAPPort:        spec.IMAPPort,
		SMTPHost:        spec.SMTPHost,
		SMTPPort:        spec.SMTPPort,
		CredentialsCT:   ct,
		CredentialsDEK:  scheme,
	}
	return preparedUpdate{patch: patch}
}

// oauthConnectionImmutableErr is the per-object refusal of a connection change
// on an OAuth account (ADR-046 §9 obligation (e)).
func oauthConnectionImmutableErr(props []string) *SetError {
	return &SetError{Type: setInvalidProps,
		Description: "this account signs in with its provider (OAuth): its servers and sign-in cannot be " +
			"edited. To use a different mailbox, disconnect it and connect the other one",
		Properties: props}
}

// accountSpecFromCreate builds an onboarding.AccountSpec from a parsed create/
// connection form, enforcing the required fields and defaulting the ports and
// transport. Real accounts always use implicit TLS (993/465).
func accountSpecFromCreate(c accountCreate) (onboarding.AccountSpec, *SetError) {
	missing := func(field string) *SetError {
		return &SetError{Type: setInvalidProps, Description: field + " is required", Properties: []string{field}}
	}
	switch {
	case c.Name == nil || strings.TrimSpace(*c.Name) == "":
		return onboarding.AccountSpec{}, missing("name")
	case c.IMAPHost == nil || strings.TrimSpace(*c.IMAPHost) == "":
		return onboarding.AccountSpec{}, missing("imapHost")
	case c.SMTPHost == nil || strings.TrimSpace(*c.SMTPHost) == "":
		return onboarding.AccountSpec{}, missing("smtpHost")
	case c.Username == nil || strings.TrimSpace(*c.Username) == "":
		return onboarding.AccountSpec{}, missing("username")
	case c.Password == nil || *c.Password == "":
		return onboarding.AccountSpec{}, missing("password")
	}
	spec := onboarding.AccountSpec{
		Name:     strings.TrimSpace(*c.Name),
		IMAPHost: strings.TrimSpace(*c.IMAPHost),
		SMTPHost: strings.TrimSpace(*c.SMTPHost),
		Username: strings.TrimSpace(*c.Username),
		Password: *c.Password,
		TLS:      onboarding.TLSImplicit,
	}
	if c.ProviderProfile != nil {
		spec.ProviderProfile = strings.TrimSpace(*c.ProviderProfile)
	}
	if c.IMAPPort != nil {
		spec.IMAPPort = *c.IMAPPort
	}
	if c.SMTPPort != nil {
		spec.SMTPPort = *c.SMTPPort
	}
	if err := spec.Validate(); err != nil {
		return onboarding.AccountSpec{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	return spec, nil
}

// credentialsFromSpec builds the sealed-secret payload: the IMAP login is the
// SMTP login too (the common one-login case), matching onboarding.AddAccount.
func credentialsFromSpec(spec onboarding.AccountSpec) onboarding.Credentials {
	return onboarding.Credentials{
		IMAPUsername: spec.Username,
		IMAPPassword: spec.Password,
		SMTPUsername: spec.Username,
		SMTPPassword: spec.Password,
	}
}

// defaultIdentityAddr returns the send-as address to auto-provision for a new
// account: the login when it is an email address, else "" (a plain mailbox login
// that is not itself an address gets no default identity).
func defaultIdentityAddr(username string) string {
	if strings.Contains(username, "@") {
		return strings.ToLower(username)
	}
	return ""
}

// accountObjectByID re-reads a just-written account so the created object carries
// the server-applied defaults (ports, provider profile). It never exposes the
// password.
func accountObjectByID(ctx context.Context, tx *store.UserTx, id ulid.ULID) (map[string]any, error) {
	accounts, err := tx.GetAccounts(ctx, []ulid.ULID{id})
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return map[string]any{"id": id.String()}, nil
	}
	return accountObject(accounts[0], nil, nil), nil
}
