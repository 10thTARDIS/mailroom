// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// Identity methods (RFC 8621 §6). An Identity is a VERIFIED SEND-AS RIGHT: it is
// what the server validates a message's From address against before sending
// anything (spec §5), so this surface is a security boundary, not a preferences
// page.
//
// Two things follow from that, and they are the whole design of Identity/set:
//
//   - A client MAY NOT invent a send-as right. Creating an identity for an
//     address at a domain the user has not verified would let any client that can
//     call /jmap send as anyone — it would make the server-side From validation
//     ornamental. So create is allowed only for an address that ALREADY falls
//     under one of the user's verified domain-wildcard identities (the catchall
//     case, spec §5: the domain is the verified thing, and pinning a concrete
//     address under it — to hang a display name or signature on — adds no
//     authority). Anything else is `forbidden`.
//   - The address of an existing identity is IMMUTABLE. Re-pointing an identity
//     at a new address would launder authorization the same way.
//
// A wildcard identity renders as RFC 8621's own wildcard form — email
// "*@example.com", which the RFC defines as "any local part at this domain" — so
// a stock JMAP client understands the catchall model with no extension at all.

// identityObject renders a store.Identity as an RFC 8621 Identity object.
func identityObject(id store.Identity, props map[string]bool) map[string]any {
	// READ-ONLY, AND NEVER KEY MATERIAL (#492). The id of the key bound as this
	// identity's default_key_id, and only while that key can still sign — the
	// store decides that (store.Identity.SigningKeyID), so this projection and
	// the send path's own choice of key cannot disagree.
	//
	// ⚠ IT IS HALF OF "WILL SIGN", NOT ALL OF IT. Signing also needs the crypto
	// process, which is an INSTANCE fact and is advertised separately as
	// urn:mailroom:pgp {signing}. A client that reads this alone would promise a
	// signature on a server that cannot make one (docs/openapi.yaml says so
	// beside UserKey/get).
	var signingKey any
	if id.SigningKeyID != nil {
		signingKey = id.SigningKeyID.String()
	}
	full := map[string]any{
		"id":            id.ID.String(),
		"name":          id.DisplayName,
		"email":         id.Email(),
		"replyTo":       nil,
		"bcc":           nil,
		"textSignature": "",
		"htmlSignature": id.SignatureHTML,
		"mayDelete":     true,
		// Mailroom extension (urn:mailroom:compose): which account's SMTP this
		// identity submits through, and whether it is a catchall wildcard. Both
		// are already implied by `email`, but a client that wants to group
		// identities by account should not have to parse an address to do it.
		"accountId":  id.AccountID.String(),
		"isWildcard": id.Kind == store.IdentityWildcard,
		"enabled":    id.Enabled,
		// Mailroom extension (urn:mailroom:pgp).
		"signingKeyId": signingKey,
	}
	if props == nil {
		return full
	}
	out := map[string]any{"id": id.ID.String()}
	for k, v := range full {
		if props[k] {
			out[k] = v
		}
	}
	return out
}

// ---- Identity/get ---------------------------------------------------------

func (a *API) identityGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties []string  `json:"properties"`
	}
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
		byID      = make(map[ulid.ULID]store.Identity)
		requested []requestedID
		notFound  = []string{}
	)
	mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var list []store.Identity
		var err error
		if args.IDs == nil {
			list, err = tx.ListIdentities(ctx)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
			for _, v := range list {
				requested = append(requested, requestedID{raw: v.ID.String(), id: v.ID, ok: true})
			}
		} else {
			var valid []ulid.ULID
			valid, notFound, requested = parseIDs(*args.IDs)
			list, err = tx.GetIdentities(ctx, valid)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		for _, v := range list {
			// An account-restricted API key sees only its accounts' identities
			// (#49, ADR-042 §5).
			if !keyAllowsAccount(ctx, v.AccountID) {
				continue
			}
			byID[v.ID] = v
		}
		if keyAccounts(ctx) != nil && args.IDs == nil {
			kept := requested[:0]
			for _, r := range requested {
				if _, ok := byID[r.id]; ok {
					kept = append(kept, r)
				}
			}
			requested = kept
		}
		state, err = tx.ChangeState(ctx, store.ObjectIdentity)
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
		iv, ok := byID[req.id]
		if !ok {
			notFound = append(notFound, req.raw)
			continue
		}
		list = append(list, identityObject(iv, props))
	}
	return mustMarshal(getResult{
		AccountID: userID.String(),
		State:     encodeState(state),
		List:      list,
		NotFound:  notFound,
	}), nil
}

// ---- Identity/changes -----------------------------------------------------

func (a *API) identityChanges(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.handleChanges(ctx, userID, rawArgs, store.ObjectIdentity)
}

// ---- Identity/set ---------------------------------------------------------

type identityCreate struct {
	Email         *string `json:"email"`
	Name          *string `json:"name"`
	HTMLSignature *string `json:"htmlSignature"`
	TextSignature *string `json:"textSignature"`
}

func (a *API) identitySet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs // the shared {accountId, ifInState, create, update, destroy} shape
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}

	res := newSetResult(userID)
	var stateErr *MethodError
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		oldState, err := tx.ChangeState(ctx, store.ObjectIdentity)
		if err != nil {
			return err
		}
		res.OldState = encodeState(oldState)
		if args.IfInState != nil && *args.IfInState != encodeState(oldState) {
			stateErr = &MethodError{Type: errStateMismatch}
			return errMethodRollback
		}

		created := map[string]ulid.ULID{}
		for cid, raw := range args.Create {
			obj, id, serr := createIdentity(ctx, tx, raw)
			if serr != nil {
				res.NotCreated[cid] = *serr
				continue
			}
			created[cid] = id
			res.Created[cid] = obj
		}

		for id, patch := range args.Update {
			iid, ok := resolveSetID(id, created)
			if !ok {
				res.NotUpdated[id] = SetError{Type: setNotFound}
				continue
			}
			if serr := updateIdentity(ctx, tx, iid, patch); serr != nil {
				res.NotUpdated[id] = *serr
				continue
			}
			res.Updated[id] = nil
		}

		for _, id := range args.Destroy {
			iid, ok := resolveSetID(id, created)
			if !ok {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			found, derr := tx.DeleteIdentity(ctx, iid)
			if derr != nil {
				// An identity that has already sent mail is referenced by a
				// submission and cannot be deleted (the audit trail of what was
				// sent as whom is not erasable). The FK violation ran under a
				// savepoint, so the rest of the batch is unharmed.
				res.NotDestroyed[id] = SetError{Type: setForbidden,
					Description: "this identity has submissions and cannot be deleted; disable it instead"}
				continue
			}
			if !found {
				res.NotDestroyed[id] = SetError{Type: setNotFound}
				continue
			}
			res.Destroyed = append(res.Destroyed, id)
		}

		newState, err := tx.ChangeState(ctx, store.ObjectIdentity)
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
	return mustMarshal(res), nil
}

// createIdentity pins a concrete send-as address under an ALREADY-VERIFIED
// wildcard domain. It refuses anything else: see the file header. This is the
// one place a client can add an identity, and it can only ever add authority it
// already had.
func createIdentity(ctx context.Context, tx *store.UserTx, raw json.RawMessage) (map[string]any, ulid.ULID, *SetError) {
	var c identityCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}
	if c.Email == nil || strings.TrimSpace(*c.Email) == "" {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "email is required", Properties: []string{"email"}}
	}
	addr := store.NormalizeAddress(*c.Email)
	if strings.HasPrefix(addr, "*@") {
		return nil, ulid.ULID{}, &SetError{Type: setForbidden,
			Description: "a wildcard (catchall) identity attests that a whole domain is verified; it is provisioned by the operator, not created over the API",
			Properties:  []string{"email"}}
	}

	// THE CHECK: the address must already be one this user may send as. In
	// practice that means a wildcard identity covers its domain — so this
	// creates a named identity for an alias the catchall already authorizes,
	// and grants nothing new.
	si, err := tx.ResolveSendIdentity(ctx, addr)
	if err != nil {
		if errors.Is(err, store.ErrFromNotVerified) {
			return nil, ulid.ULID{}, &SetError{Type: setForbidden,
				Description: "this address is not covered by any verified identity or domain for your account, so the server will not let you send as it",
				Properties:  []string{"email"}}
		}
		return nil, ulid.ULID{}, &SetError{Type: setForbidden, Description: err.Error()}
	}
	// ViaSubaddress matters here: the identity that authorized this address is
	// the one for its BASE ("me@d" covering "me+shop@d"), so there is no row for
	// this address and naming it is a create, not a duplicate. It still grants
	// nothing new — the base already allowed it — which is the standard this
	// check exists to hold.
	if si.Kind == store.IdentityFixed && !si.ViaSubaddress {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
			Description: "an identity for this address already exists", Properties: []string{"email"}}
	}

	row := store.IdentityRow{
		AccountID: si.AccountID,
		Kind:      store.IdentityFixed,
		Address:   addr,
		Enabled:   true,
	}
	if c.Name != nil {
		row.DisplayName = *c.Name
	}
	if c.HTMLSignature != nil {
		row.SignatureHTML = *c.HTMLSignature
	}
	id, err := tx.CreateIdentity(ctx, row)
	if err != nil {
		if errors.Is(err, store.ErrIdentityExists) {
			return nil, ulid.ULID{}, &SetError{Type: setInvalidProps,
				Description: "an identity for this address already exists", Properties: []string{"email"}}
		}
		return nil, ulid.ULID{}, &SetError{Type: setForbidden, Description: err.Error()}
	}
	views, err := tx.GetIdentities(ctx, []ulid.ULID{id})
	if err != nil || len(views) == 0 {
		return map[string]any{"id": id.String()}, id, nil
	}
	return identityObject(views[0], nil), id, nil
}

// updateIdentity applies the mutable subset of an Identity patch: the display
// name, the signature, and whether the identity is enabled. `email` is rejected
// as immutable — changing it is how a client would try to launder a send-as
// right it does not have.
func updateIdentity(ctx context.Context, tx *store.UserTx, id ulid.ULID, patch json.RawMessage) *SetError {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(patch, &raw); err != nil {
		return &SetError{Type: setInvalidPatch, Description: err.Error()}
	}
	var p store.IdentityPatch
	for key, val := range raw {
		switch key {
		case "name":
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "name must be a string", Properties: []string{"name"}}
			}
			p.DisplayName = &s
		case "htmlSignature":
			var s string
			if err := json.Unmarshal(val, &s); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "htmlSignature must be a string", Properties: []string{"htmlSignature"}}
			}
			p.SignatureHTML = &s
		case "enabled":
			var b bool
			if err := json.Unmarshal(val, &b); err != nil {
				return &SetError{Type: setInvalidProps,
					Description: "enabled must be a boolean", Properties: []string{"enabled"}}
			}
			p.Enabled = &b
		case "id", "email", "accountId", "isWildcard", "mayDelete":
			return &SetError{Type: setInvalidProps,
				Description: key + " is immutable (an identity's address IS its verification)",
				Properties:  []string{key}}
		case "signingKeyId":
			// SERVER-OWNED, and named here only so the refusal says why (#492).
			// The default branch below would already refuse it as an unknown
			// patch key; what it would not say is that binding a key to an
			// identity happens through the crypto process (mailroom-admin
			// pgp-bind), which is the only party that can hold a private half at
			// all (ADR-024). A client that could patch this would be choosing
			// which key signs its mail.
			return &SetError{Type: setInvalidProps,
				Description: "signingKeyId is read-only: a signing key is bound to an identity by the crypto process, never over JMAP",
				Properties:  []string{key}}
		default:
			return &SetError{Type: setInvalidProps,
				Description: "unsupported Identity property in patch: " + key, Properties: []string{key}}
		}
	}
	found, err := tx.UpdateIdentity(ctx, id, p)
	if err != nil {
		return &SetError{Type: setForbidden, Description: err.Error()}
	}
	if !found {
		return &SetError{Type: setNotFound}
	}
	return nil
}

// newSetResult builds an empty /set response (every map non-nil, as RFC 8620
// §5.3 requires).
func newSetResult(userID ulid.ULID) setResult {
	return setResult{
		AccountID:    userID.String(),
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}
}
