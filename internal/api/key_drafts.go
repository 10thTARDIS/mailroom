// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// AN API KEY'S OWN DRAFTS (#872, ADR-045). Three things in Email/set that only
// a key does, all confined to the drafts the key itself wrote
// (app.api_key_drafts):
//
//  1. REPEATS COLLAPSE. A key's create that is identical to a draft the same
//     key saved in the last KeyDraftRepeatWindow, which is still an unsent
//     draft in Drafts, writes nothing and answers with that draft, marked
//     alreadySaved: true. Seen in the field: a model that could not send saved
//     the same draft five times in 85 seconds while trying to.
//  2. REPLACE. A create carrying replacesEmailId is a new version of that
//     draft: checked first, created, and the old version moved to Trash in the
//     same transaction — a failure of either leaves the old draft as it was.
//     The reply threading of the old version is kept.
//  3. DISCARD. discardDrafts moves the key's own unsent drafts to Trash.
//
// Replace and discard need mail:draft, not mail:delete, and are never held for
// approval: they touch only what the key wrote and never sent. They are still
// SOFT — the version goes to Trash, never destroyed — because spec §13a allows
// no agent path to a hard delete, ever (ADR-042 §3); see ADR-045 §3 for why
// that rule holds here too. Both refuse a draft with a submission or with a
// send waiting for (or just given) a person's approval: the approval pins the
// draft, and approving a send of a draft the key had meanwhile replaced would
// send the old words.

// KeyDraftRepeatWindow is how far back a key's identical draft is collapsed
// into rather than written again.
const KeyDraftRepeatWindow = 10 * time.Minute

// draftCanonical is the normalised content a key's create is fingerprinted
// on: what the draft says and to whom, never when it was asked. Addresses are
// lowercased and sorted (the same people in another order are the same
// draft); text has its line endings unified and trailing whitespace dropped.
// The From is as given — an omitted From (the default identity) and the
// default address spelled out are two fingerprints, which costs at most one
// extra draft.
func draftCanonical(c emailCreate) []byte {
	// Every field is a separate JSON value — an address is [email, name], an
	// attachment a five-element array — so no choice of field contents can
	// make two different drafts encode alike. (A separator byte between
	// fields could: "a@b" + "n\x00m" and "a@b\x00n" + "m" once joined the same.)
	addrs := func(in []jmapEmailAddr) [][2]string {
		out := make([][2]string, 0, len(in))
		for _, a := range in {
			out = append(out, [2]string{strings.ToLower(strings.TrimSpace(a.Email)), strings.TrimSpace(a.Name)})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i][0] != out[j][0] {
				return out[i][0] < out[j][0]
			}
			return out[i][1] < out[j][1]
		})
		return out
	}
	text := func(s string) string {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		return strings.TrimRight(s, " \t\r\n")
	}
	atts := make([][5]string, 0, len(c.Attachments))
	for _, at := range c.Attachments {
		atts = append(atts, [5]string{at.BlobID, at.Type, at.Name, at.Disposition, at.CID})
	}
	b, _ := json.Marshal(struct {
		V           int         `json:"v"`
		From        [][2]string `json:"from"`
		To          [][2]string `json:"to"`
		Cc          [][2]string `json:"cc"`
		Bcc         [][2]string `json:"bcc"`
		Subject     string      `json:"subject"`
		InReplyTo   string      `json:"inReplyTo"`
		References  []string    `json:"references"`
		Text        string      `json:"text"`
		HTML        string      `json:"html"`
		Attachments [][5]string `json:"attachments"`
	}{
		V: 2, From: addrs(c.From), To: addrs(c.To), Cc: addrs(c.Cc), Bcc: addrs(c.Bcc),
		Subject: strings.TrimSpace(c.Subject), InReplyTo: firstMsgID(c.InReplyTo), References: trimMsgIDs(c.References),
		Text: text(bodyText(c.BodyValues, c.TextBody)), HTML: text(bodyText(c.BodyValues, c.HTMLBody)),
		Attachments: atts,
	})
	return b
}

// keyDraftRefusal is why a key may not replace or discard a message, in words
// an agent can act on, or nil when it may. verb is "changed" or "discarded".
func keyDraftRefusal(st store.KeyDraftStatus, verb string) *SetError {
	switch {
	case !st.Exists:
		return &SetError{Type: setNotFound, Description: "no such draft"}
	case !st.Own:
		return &SetError{Type: setForbidden, Description: "this draft was not written by this API key; a key can " +
			"change or discard only drafts it wrote itself, never the account owner's or another key's"}
	case st.Submitted:
		return &SetError{Type: setForbidden, Description: "this draft has already been submitted for sending, so it " +
			"can no longer be " + verb}
	case st.SendPending:
		return &SetError{Type: setForbidden, Description: "a send of this draft is waiting for the account owner's " +
			"approval; it cannot be " + verb + " until they approve or deny it"}
	case !st.Live():
		return &SetError{Type: setForbidden, Description: "this message is no longer an unsent draft in Drafts (it " +
			"was replaced, discarded or moved), so it cannot be " + verb}
	}
	return nil
}

// keyDraftTarget parses id and reads its status for the key, applying the
// key's account restriction (a message outside it is notFound, as everywhere).
func keyDraftTarget(ctx context.Context, tx *store.UserTx, p *keyPrincipal, id, verb string) (ulid.ULID, store.KeyDraftStatus, *SetError, error) {
	mid, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, store.KeyDraftStatus{}, &SetError{Type: setNotFound, Description: "no such draft"}, nil
	}
	if visible, verr := keyVisibleMessages(ctx, tx, []ulid.ULID{mid}); verr != nil {
		return ulid.ULID{}, store.KeyDraftStatus{}, nil, verr
	} else if visible != nil && !visible[mid] {
		return ulid.ULID{}, store.KeyDraftStatus{}, &SetError{Type: setNotFound, Description: "no such draft"}, nil
	}
	st, err := tx.APIKeyDraftStatus(ctx, p.key.ID, mid)
	if err != nil {
		return ulid.ULID{}, store.KeyDraftStatus{}, nil, err
	}
	return mid, st, keyDraftRefusal(st, verb), nil
}

// errReplacedDraftGone aborts a replacement whose old version could not be
// moved to Trash after the new one was written: the whole Email/set rolls
// back, so the old draft stays and no new one exists.
var errReplacedDraftGone = errors.New("the draft being replaced could not be moved to Trash; nothing was changed")

// keyCreateDraft is one create by an API key: a repeat collapsed into the
// draft it repeats, a replacement of the key's own draft, or a new draft —
// recorded in app.api_key_drafts with its fingerprint either way. collapse is
// false in a call that also destroys or discards, where the draft a repeat
// would be collapsed into might be the one going away.
func (a *API) keyCreateDraft(ctx context.Context, tx *store.UserTx, userID ulid.ULID, p *keyPrincipal, raw json.RawMessage, collapse bool) (map[string]any, ulid.ULID, *SetError, error) {
	var c emailCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}, nil
	}
	// The caller (emailSet) holds the key's draft lock from the top of its
	// transaction to the commit: a racing identical create waits for this one
	// and then finds its draft.

	var replaced *ulid.ULID
	if c.ReplacesEmailID != "" {
		old, st, serr, err := keyDraftTarget(ctx, tx, p, c.ReplacesEmailID, "changed")
		if err != nil {
			return nil, ulid.ULID{}, nil, err
		}
		if serr != nil {
			serr.Properties = []string{"replacesEmailId"}
			return nil, ulid.ULID{}, serr, nil
		}
		replaced = &old
		collapse = false
		// The new version answers the same message the old one did, unless
		// the create says otherwise.
		if len(c.InReplyTo) == 0 && len(c.References) == 0 && (st.InReplyTo != "" || len(st.References) > 0) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}, nil
			}
			if st.InReplyTo != "" {
				c.InReplyTo = []string{st.InReplyTo}
				m["inReplyTo"], _ = json.Marshal(c.InReplyTo)
			}
			if len(st.References) > 0 {
				c.References = st.References
				m["references"], _ = json.Marshal(c.References)
			}
			raw, _ = json.Marshal(m)
		}
	}

	fp, err := a.composer.DraftFingerprint(ctx, userID, draftCanonical(c))
	if err != nil {
		return nil, ulid.ULID{}, nil, err
	}
	if collapse {
		id, found, err := tx.RecentAPIKeyDraft(ctx, p.key.ID, fp, KeyDraftRepeatWindow)
		if err != nil {
			return nil, ulid.ULID{}, nil, err
		}
		if found {
			obj := map[string]any{"id": id.String()}
			if views, err := tx.GetEmails(ctx, []ulid.ULID{id}); err == nil && len(views) == 1 {
				obj = emailObject(views[0], nil, nil)
			}
			obj["alreadySaved"] = true
			return obj, id, nil, nil
		}
	}

	obj, id, serr := a.createDraft(ctx, tx, userID, raw)
	if serr != nil {
		return nil, ulid.ULID{}, serr, nil
	}
	// A key's drafts are remembered: it may send, replace and discard only
	// those (#49, #872).
	if err := tx.RecordAPIKeyDraft(ctx, p.key.ID, id, fp); err != nil {
		return nil, ulid.ULID{}, nil, err
	}
	if replaced != nil {
		found, _, err := tx.ApplyEmailUpdate(ctx, *replaced,
			store.EmailUpdate{Trash: true, LabelSource: store.AgentLabelSource()})
		if err != nil {
			return nil, ulid.ULID{}, nil, err
		}
		if !found {
			return nil, ulid.ULID{}, nil, errReplacedDraftGone
		}
		obj["replacedEmailId"] = replaced.String()
	}
	return obj, id, nil, nil
}

// keyDiscardDraft moves one of the key's own unsent drafts to Trash. The
// caller (emailSet) holds the key's draft lock.
func keyDiscardDraft(ctx context.Context, tx *store.UserTx, p *keyPrincipal, id string) (*SetError, error) {
	mid, _, serr, err := keyDraftTarget(ctx, tx, p, id, "discarded")
	if err != nil || serr != nil {
		return serr, err
	}
	found, _, err := tx.ApplyEmailUpdate(ctx, mid, store.EmailUpdate{Trash: true, LabelSource: store.AgentLabelSource()})
	if err != nil {
		return nil, err
	}
	if !found {
		return &SetError{Type: setNotFound, Description: "no such draft"}, nil
	}
	return nil, nil
}
