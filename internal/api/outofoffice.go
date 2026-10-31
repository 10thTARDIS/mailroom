// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/outofoffice"
	"mailroom/internal/store"
)

// OutOfOffice/get, /set and /check (#50, urn:mailroom:outofoffice).
//
// ONE OBJECT PER MAIL ACCOUNT, and its id is the ACCOUNT's id. This is not
// RFC 8621's VacationResponse, which is a singleton per JMAP account: here a
// login holds several mail accounts and each provider runs its own reply, so
// the standard object's shape cannot express it.
//
// /set does its work on the PROVIDER first and saves only what the provider
// accepted: turning the reply on while another Sieve script is active is
// refused (notUpdated, with a sentence naming the script) and nothing is
// saved. Create and destroy are refused: the object exists for every account.
//
// /check asks the provider what it holds now and records it, which is how
// Settings shows drift.

// OutOfOfficeService is the seam to internal/outofoffice.
type OutOfOfficeService interface {
	List(ctx context.Context, userID ulid.ULID) ([]outofoffice.View, error)
	Get(ctx context.Context, userID, accountID ulid.ULID) (outofoffice.View, error)
	Set(ctx context.Context, userID, accountID ulid.ULID, in outofoffice.Input) (outofoffice.View, error)
	Check(ctx context.Context, userID, accountID ulid.ULID) (outofoffice.View, error)
}

// WithOutOfOffice installs the out-of-office service. Without it the methods
// answer serverFail with a plain description.
func WithOutOfOffice(s OutOfOfficeService) Option { return func(a *API) { a.outOfOffice = s } }

func outOfOfficeObject(v outofoffice.View) map[string]any {
	addrs := v.Addresses
	if addrs == nil {
		addrs = []string{}
	}
	o := map[string]any{
		"id":           v.AccountID.String(),
		"accountName":  v.AccountName,
		"isEnabled":    v.Enabled,
		"fromDate":     jmapDateOrNil(v.From),
		"toDate":       jmapDateOrNil(v.To),
		"subject":      v.Subject,
		"textBody":     v.Body,
		"replyDays":    v.ReplyDays,
		"fromAddress":  v.FromAddress,
		"contactsOnly": v.ContactsOnly,
		"addresses":    addrs,
		"isSupported":  v.Supported,
		// What the last look at the provider found, and when. One of off,
		// active, displaced, missing, modified, error.
		"serverState":  v.ServerState,
		"serverDetail": v.ServerDetail,
		"checkedAt":    jmapDateOrNil(v.CheckedAt),
		// The bound on a contacts-only list: how many the script names, of how
		// many. Null unless contacts-only is on.
		"contactsIncluded": v.ContactsIncluded,
		"contactsTotal":    v.ContactsTotal,
		"maxContacts":      outofoffice.MaxContacts,
	}
	if !v.Supported {
		o["unsupportedReason"] = v.Unsupported
	}
	return o
}

func (a *API) outOfOfficeUnavailable() *MethodError {
	return &MethodError{Type: errServerFail, Description: "out of office is not configured on this server"}
}

type outOfOfficeGetArgs struct {
	AccountID string   `json:"accountId"`
	IDs       []string `json:"ids"`
}

func (a *API) outOfOfficeGet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.outOfOfficeRead(ctx, userID, rawArgs, false)
}

func (a *API) outOfOfficeCheck(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	return a.outOfOfficeRead(ctx, userID, rawArgs, true)
}

// outOfOfficeRead serves /get (stored state) and /check (asks the provider
// first). /check requires ids: asking every provider is not a side effect a
// client gets by leaving ids out.
func (a *API) outOfOfficeRead(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage, check bool) (json.RawMessage, *MethodError) {
	var args outOfOfficeGetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if a.outOfOffice == nil {
		return nil, a.outOfOfficeUnavailable()
	}
	if check && args.IDs == nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: "OutOfOffice/check needs ids: it contacts each provider"}
	}
	list := []any{}
	notFound := []string{}
	if args.IDs == nil {
		views, err := a.outOfOffice.List(ctx, userID)
		if err != nil {
			return nil, &MethodError{Type: errServerFail, Description: err.Error()}
		}
		for _, v := range views {
			list = append(list, outOfOfficeObject(v))
		}
	} else {
		for _, raw := range args.IDs {
			id, err := ulid.Parse(raw)
			if err != nil {
				notFound = append(notFound, raw)
				continue
			}
			var v outofoffice.View
			if check {
				v, err = a.outOfOffice.Check(ctx, userID, id)
			} else {
				v, err = a.outOfOffice.Get(ctx, userID, id)
			}
			if errors.Is(err, store.ErrAccountNotFound) {
				notFound = append(notFound, raw)
				continue
			}
			if err != nil {
				return nil, &MethodError{Type: errServerFail, Description: err.Error()}
			}
			list = append(list, outOfOfficeObject(v))
		}
	}
	state, mErr := a.outOfOfficeState(ctx, userID)
	if mErr != nil {
		return nil, mErr
	}
	return mustMarshal(map[string]any{
		"accountId": userID.String(),
		"state":     state,
		"list":      list,
		"notFound":  notFound,
	}), nil
}

func (a *API) outOfOfficeState(ctx context.Context, userID ulid.ULID) (string, *MethodError) {
	var st int64
	if err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		st, e = tx.ChangeState(ctx, store.ObjectOutOfOffice)
		return e
	}); err != nil {
		return "", &MethodError{Type: errServerFail, Description: err.Error()}
	}
	return encodeState(st), nil
}

// outOfOfficePatch is the whole form; every field is required, like
// quietHours: a partial object would leave the server to guess.
type outOfOfficePatch struct {
	IsEnabled    *bool            `json:"isEnabled"`
	FromDate     *json.RawMessage `json:"fromDate"`
	ToDate       *json.RawMessage `json:"toDate"`
	Subject      *string          `json:"subject"`
	TextBody     *string          `json:"textBody"`
	ReplyDays    *int             `json:"replyDays"`
	FromAddress  *string          `json:"fromAddress"`
	ContactsOnly *bool            `json:"contactsOnly"`
}

func parseOptionalDate(raw *json.RawMessage) (*time.Time, bool) {
	if raw == nil || string(*raw) == "null" {
		return nil, true
	}
	var s string
	if err := json.Unmarshal(*raw, &s); err != nil {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, false
	}
	t = t.UTC()
	return &t, true
}

func parseOutOfOffice(raw json.RawMessage) (outofoffice.Input, *SetError) {
	bad := func(why string) *SetError { return &SetError{Type: setInvalidProps, Description: why} }
	var p outOfOfficePatch
	if err := json.Unmarshal(raw, &p); err != nil {
		return outofoffice.Input{}, bad("an OutOfOffice update is an object")
	}
	if p.IsEnabled == nil || p.Subject == nil || p.TextBody == nil || p.ReplyDays == nil ||
		p.FromAddress == nil || p.ContactsOnly == nil {
		return outofoffice.Input{}, bad("isEnabled, fromDate, toDate, subject, textBody, replyDays, fromAddress and contactsOnly are all required")
	}
	from, ok := parseOptionalDate(p.FromDate)
	if !ok {
		return outofoffice.Input{}, bad("fromDate must be a UTCDate or null")
	}
	to, ok := parseOptionalDate(p.ToDate)
	if !ok {
		return outofoffice.Input{}, bad("toDate must be a UTCDate or null")
	}
	return outofoffice.Input{
		Enabled: *p.IsEnabled, From: from, To: to, Subject: *p.Subject, Body: *p.TextBody,
		ReplyDays: *p.ReplyDays, FromAddress: *p.FromAddress, ContactsOnly: *p.ContactsOnly,
	}, nil
}

func (a *API) outOfOfficeSet(ctx context.Context, userID ulid.ULID, rawArgs json.RawMessage) (json.RawMessage, *MethodError) {
	var args emailSetArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &MethodError{Type: errInvalidArguments, Description: err.Error()}
	}
	if mErr := resolveAccount(userID, args.AccountID); mErr != nil {
		return nil, mErr
	}
	if a.outOfOffice == nil {
		return nil, a.outOfOfficeUnavailable()
	}
	oldState, mErr := a.outOfOfficeState(ctx, userID)
	if mErr != nil {
		return nil, mErr
	}
	if args.IfInState != nil && *args.IfInState != oldState {
		return nil, &MethodError{Type: errStateMismatch}
	}
	res := setResult{
		AccountID:    userID.String(),
		OldState:     oldState,
		Created:      map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]SetError{},
		NotUpdated:   map[string]SetError{},
		NotDestroyed: map[string]SetError{},
	}
	for cid := range args.Create {
		res.NotCreated[cid] = SetError{Type: setForbidden, Description: "every account has an out-of-office object; update it"}
	}
	for _, id := range args.Destroy {
		res.NotDestroyed[id] = SetError{Type: setForbidden, Description: "every account has an out-of-office object; turn it off instead"}
	}
	for raw, patch := range args.Update {
		id, err := ulid.Parse(raw)
		if err != nil {
			res.NotUpdated[raw] = SetError{Type: setNotFound}
			continue
		}
		in, serr := parseOutOfOffice(patch)
		if serr != nil {
			res.NotUpdated[raw] = *serr
			continue
		}
		v, err := a.outOfOffice.Set(ctx, userID, id, in)
		var foreign *outofoffice.ForeignActiveError
		switch {
		case err == nil:
			res.Updated[raw] = outOfOfficeObject(v)
		case errors.Is(err, store.ErrAccountNotFound):
			res.NotUpdated[raw] = SetError{Type: setNotFound}
		case errors.As(err, &foreign):
			res.NotUpdated[raw] = SetError{Type: setForbidden, Description: foreign.Error()}
		case errors.Is(err, outofoffice.ErrInvalid), errors.Is(err, outofoffice.ErrNoContacts):
			res.NotUpdated[raw] = SetError{Type: setInvalidProps, Description: err.Error()}
		case errors.Is(err, outofoffice.ErrTooLarge):
			res.NotUpdated[raw] = SetError{Type: setOverQuota, Description: err.Error()}
		default:
			// The provider could not be reached or said no: the user must see
			// it, and nothing was saved.
			res.NotUpdated[raw] = SetError{Type: setForbidden, Description: "The mail server did not accept the change, so nothing was saved: " + err.Error()}
		}
	}
	newState, mErr := a.outOfOfficeState(ctx, userID)
	if mErr != nil {
		return nil, mErr
	}
	res.NewState = newState
	return mustMarshal(res), nil
}
