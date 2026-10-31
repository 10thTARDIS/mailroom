// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/compose"
	"mailroom/internal/store"
)

// Email/set CREATE — composing a draft (spec §5, RFC 8621 §4.6).
//
// The client sends the JMAP Email shape it would send any conforming server:
// from/to/cc/bcc, subject, inReplyTo/references, and body parts through
// bodyValues + textBody/htmlBody. Mailroom BUILDS the RFC 5322 bytes itself —
// the client never supplies them — which is what lets the server guarantee the
// three things spec §5 requires of every message it originates:
//
//   - the From address is one this user has VERIFIED (compose.Composer validates
//     it against the identity store, and there is no path around that check);
//   - the Message-ID is generated under the SENDING IDENTITY'S domain;
//   - In-Reply-To/References/Date are correct.
//
// mailboxIds and keywords in the create are accepted but not honored: a draft
// goes in the account's Drafts mailbox with $draft, and letting a client file a
// brand-new message anywhere it likes (Sent, say) would let it fabricate history.
//
// A draft is EDITED by creating the new version and destroying the old one in
// the same Email/set call (JMAP Emails are immutable apart from keywords and
// mailboxIds, RFC 8621 §4.6). That is not a workaround — it is what makes the
// draft conflict rule fall out for free: see store.RetireDraft.

// emailCreate is the subset of the JMAP Email create shape a draft needs.
type emailCreate struct {
	From        []jmapEmailAddr          `json:"from"`
	To          []jmapEmailAddr          `json:"to"`
	Cc          []jmapEmailAddr          `json:"cc"`
	Bcc         []jmapEmailAddr          `json:"bcc"`
	Subject     string                   `json:"subject"`
	InReplyTo   []string                 `json:"inReplyTo"`
	References  []string                 `json:"references"`
	BodyValues  map[string]jmapBodyValue `json:"bodyValues"`
	TextBody    []jmapBodyPart           `json:"textBody"`
	HTMLBody    []jmapBodyPart           `json:"htmlBody"`
	Attachments []jmapAttachment         `json:"attachments"`
	Keywords    map[string]bool          `json:"keywords"`
	MailboxIDs  map[string]bool          `json:"mailboxIds"`
	// ReplacesEmailID is a Mailroom extension for API keys (#872, ADR-045):
	// this create is a new version of the key's own draft with that id, which
	// moves to Trash in the same transaction (keyCreateDraft). Refused to a
	// session, which destroys the old version itself.
	ReplacesEmailID string                     `json:"replacesEmailId"`
	Extra           map[string]json.RawMessage `json:"-"`
}

// replacesDraft reports whether a create names replacesEmailId.
func replacesDraft(raw json.RawMessage) bool {
	var c struct {
		ReplacesEmailID *string `json:"replacesEmailId"`
	}
	return json.Unmarshal(raw, &c) == nil && c.ReplacesEmailID != nil
}

// jmapAttachment is an RFC 8621 EmailBodyPart used as an attachment in a create:
// a blobId (uploaded through /jmap/upload, or a part of a message the user
// already has — that is how a forward re-attaches without re-uploading), plus
// the metadata to put on the part.
type jmapAttachment struct {
	BlobID      string `json:"blobId"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Disposition string `json:"disposition"`
	CID         string `json:"cid"`
}

type jmapEmailAddr struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type jmapBodyValue struct {
	Value string `json:"value"`
}

type jmapBodyPart struct {
	PartID string `json:"partId"`
	Type   string `json:"type"`
	// Value is a Mailroom convenience: a client may inline the text on the body
	// part instead of going through bodyValues. RFC 8621 uses bodyValues, and
	// both are accepted.
	Value string `json:"value"`
}

// createDraft builds and files one draft.
func (a *API) createDraft(ctx context.Context, tx *store.UserTx, userID ulid.ULID, raw json.RawMessage) (map[string]any, ulid.ULID, *SetError) {
	var c emailCreate
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ulid.ULID{}, &SetError{Type: setInvalidProps, Description: err.Error()}
	}

	req := compose.DraftRequest{
		Subject:    c.Subject,
		To:         toComposeAddrs(c.To),
		Cc:         toComposeAddrs(c.Cc),
		Bcc:        toComposeAddrs(c.Bcc),
		InReplyTo:  firstMsgID(c.InReplyTo),
		References: trimMsgIDs(c.References),
		Text:       bodyText(c.BodyValues, c.TextBody),
		// The HTML body is user-authored and becomes the text/html part of the
		// bytes we transmit and file (compose.Build). Sanitize it HERE, at the
		// API trust boundary, with the send-path policy: a rich-text editor is a
		// convenience, never a security control, and the client's bytes are not
		// trusted (SanitizeOutboundHTML). Empty in, empty out — a plain-only send
		// is unaffected and stays text/plain.
		HTML: SanitizeOutboundHTML(bodyText(c.BodyValues, c.HTMLBody)),
	}
	if len(c.From) > 0 {
		req.From = compose.Address{Name: c.From[0].Name, Email: c.From[0].Email}
	}
	// A draft with no recipients is accepted (#601). See compose.Build for why
	// the rule belongs to submission rather than to drafting; EmailSubmission/set
	// still refuses to send one, which is the check that matters.

	// An account-restricted API key drafts only From its accounts and attaches
	// nothing: an attachment is a blob id, and a blob id carries no account,
	// so it could carry another account's mail into this one (#49, ADR-042 §5).
	if keyAccounts(ctx) != nil {
		if len(c.Attachments) > 0 {
			return nil, ulid.ULID{}, &SetError{Type: setForbidden,
				Description: "this API key is restricted to some accounts and cannot attach files",
				Properties:  []string{"attachments"}}
		}
		acct, err := a.composer.DraftAccount(ctx, tx, req.From.Email)
		if err != nil {
			return nil, ulid.ULID{}, draftSetError(err)
		}
		if !keyAllowsAccount(ctx, acct) {
			return nil, ulid.ULID{}, &SetError{Type: setForbidden,
				Description: "this API key cannot write drafts from that account", Properties: []string{"from"}}
		}
	}

	attachments, serr := a.resolveAttachments(ctx, userID, c.Attachments)
	if serr != nil {
		return nil, ulid.ULID{}, serr
	}
	req.Attachments = attachments

	res, err := a.composer.SaveDraft(ctx, tx, userID, req)
	if err != nil {
		return nil, ulid.ULID{}, draftSetError(err)
	}

	views, err := tx.GetEmails(ctx, []ulid.ULID{res.EmailID})
	if err != nil || len(views) == 0 {
		return map[string]any{"id": res.EmailID.String()}, res.EmailID, nil
	}
	// nil properties = every property of the object the server just created (RFC
	// 8620 §5.3); nil body because the client wrote this message's body — it does
	// not need the server to read it back out of the blob to tell it what it just
	// said, and doing so would mean a blob read inside the /set transaction.
	return emailObject(views[0], nil, nil), res.EmailID, nil
}

// setTooLarge is RFC 8621 §4.6's per-object error for a message whose
// attachments exceed maxSizeAttachmentsPerEmail (advertised in the session).
const setTooLarge = "tooLarge"

// resolveAttachments turns the blobIds of an Email/set create into the bytes the
// message will carry.
//
// EVERY blob goes through the SAME ownership gate the download endpoint uses
// (blobOwnedByUser: Postgres under RLS for a message blob, the per-user blob
// namespace for an upload that is not attached to anything yet). A client
// therefore cannot attach a blob it does not own — which matters, because doing
// so would let it EXFILTRATE another user's attachment by mailing it to itself,
// a far quieter attack than downloading it.
func (a *API) resolveAttachments(ctx context.Context, userID ulid.ULID, in []jmapAttachment) ([]compose.Attachment, *SetError) {
	if len(in) == 0 {
		return nil, nil
	}
	if a.blobs == nil {
		return nil, &SetError{Type: setNotImplemented,
			Description: "this server is not configured to store blobs, so it cannot attach anything"}
	}

	out := make([]compose.Attachment, 0, len(in))
	var total int64
	for _, at := range in {
		ref, err := parseBlobID(at.BlobID)
		if err != nil {
			return nil, &SetError{Type: setInvalidProps,
				Description: "attachment blobId " + at.BlobID + " is not a blob id",
				Properties:  []string{"attachments"}}
		}
		// ⚠ A PART OF A DECRYPTED MESSAGE CANNOT BE ATTACHED (#47, ADR-037).
		// Its plaintext exists only for the request that decrypted it;
		// attaching it would store that plaintext in a draft blob, which is the
		// one thing decrypt on open promises never happens.
		if ref.decrypted {
			return nil, &SetError{Type: setInvalidProps,
				Description: "attachment " + at.BlobID + " is inside an encrypted message; its decrypted " +
					"content is never stored, so it cannot be attached",
				Properties: []string{"attachments"}}
		}
		owned, _, _, messageID, err := a.blobOwnedByUser(ctx, userID, ref.hash)
		if err != nil {
			return nil, &SetError{Type: setInvalidProps, Description: err.Error()}
		}
		// ⚠ A QUARANTINED PART CANNOT BE ATTACHED TO A DRAFT (ADR-027 §8, §11).
		// This is the same gate as the download route and it belongs here for a
		// stronger reason: downloading quarantined bytes is a decision the user
		// makes about their own machine, while attaching them puts them on the
		// wire to someone else. §11 names "a user forwards malware" as a live
		// hole in the design; leaving this path open would keep it open with the
		// verdict already in hand.
		//
		// A release lifts it, exactly as it lifts the download — the user has
		// said these bytes are fine, and this endpoint is not the place to
		// second-guess that.
		//
		// ⚠ AND A BARE BLOBID IS GATED TOO (#413). This used to read
		// `if owned && ref.part != ""`, so the SUFFIXED form — one part — was
		// checked and the BARE form — the whole RFC 5322 message, quarantined
		// part and all — was attached unchecked. That is the forward-as-`.eml`
		// path, and it contradicted the rationale two paragraphs up: the gate is
		// here precisely because attaching puts bytes on the wire to someone
		// else, and forwarding the whole message puts MORE of them there, not
		// fewer. It was also weaker than the raw DOWNLOAD of the same bytes,
		// which at least sets X-Mailroom-Attachment-Scan; this path said nothing.
		//
		// ⚠ The whole-message check is deliberately NOT the download route's
		// policy. §11 lets a user download their own mail intact — a quarantined
		// part inside a .eml is inert until something extracts it — but that
		// argument is about the user's own machine and evaporates the moment the
		// bytes are addressed to somebody else.
		if owned {
			blocked, state, gerr := a.attachmentBlocked(ctx, userID, messageID, ref.part)
			if gerr != nil {
				return nil, &SetError{Type: setInvalidProps, Description: gerr.Error()}
			}
			if blocked {
				what := "attachment " + at.BlobID + " is quarantined (" + state + ")"
				if ref.part == "" {
					what = "message " + at.BlobID + " carries a quarantined attachment (" + state + ")"
				}
				return nil, &SetError{Type: setInvalidProps,
					Description: what + "; release it before attaching it to a message",
					Properties:  []string{"attachments"}}
			}
		}
		if !owned {
			// Same answer as the download endpoint gives: it does not exist. A
			// client learns nothing about blobs that are not its own.
			return nil, &SetError{Type: setNotFound,
				Description: "no such blob: " + at.BlobID, Properties: []string{"attachments"}}
		}

		data, partType, partName, err := a.blobBytes(ctx, userID, ref)
		if err != nil {
			return nil, &SetError{Type: setNotFound,
				Description: "no such blob: " + at.BlobID, Properties: []string{"attachments"}}
		}
		total += int64(len(data))
		if total > maxAttachmentBytes {
			return nil, &SetError{Type: setTooLarge,
				Description: "the attachments exceed the maximum message size",
				Properties:  []string{"attachments"}}
		}

		// The client's declared type/name win (it is authoring a message and knows
		// what it is attaching); the part's own metadata is the fallback for a
		// re-attached part. Neither can inject a header — compose sanitizes every
		// header value it writes — and neither is ever trusted on the way BACK out
		// (the download endpoint re-derives the type it serves from its allowlist).
		name := at.Name
		if name == "" {
			name = partName
		}
		mediaType := uploadType(at.Type)
		if at.Type == "" && partType != "" {
			mediaType = uploadType(partType)
		}
		out = append(out, compose.Attachment{
			Name:      name,
			Type:      mediaType,
			Data:      data,
			ContentID: at.CID,
			Inline:    at.Disposition == "inline",
		})
	}
	return out, nil
}

// maxAttachmentBytes is the session's advertised maxSizeAttachmentsPerEmail
// (session.go), enforced.
const maxAttachmentBytes = 50 << 20

// draftSetError maps a compose failure onto a per-object /set error. An
// unverified From is `forbiddenFrom` (RFC 8621 §7.5's type, reused here because
// it names exactly this condition) — the client learns it may not send as that
// address, and nothing else.
func draftSetError(err error) *SetError {
	var qe *compose.QuotaError
	switch {
	case errors.As(err, &qe):
		// Over the storage quota: the same setOverQuota per-object error the blob
		// upload endpoint returns, so Email/set create reports the draft in
		// notCreated with nothing stored (spec §10). used/limit come off the
		// sentinel — no second store read.
		se := overQuotaSetError(Quota{Kind: QuotaStorageBytes, Limit: qe.Limit, Used: qe.Used})
		return &se
	case errors.Is(err, store.ErrFromNotVerified):
		return &SetError{Type: setForbiddenFrom,
			Description: "you have no verified identity for that From address",
			Properties:  []string{"from"}}
	case errors.Is(err, compose.ErrNoDraftsMailbox):
		return &SetError{Type: setForbidden,
			Description: "this account has no Drafts mailbox (no RFC 6154 \\Drafts SPECIAL-USE folder was detected)"}
	case errors.Is(err, compose.ErrNoRecipients):
		return &SetError{Type: setInvalidProps,
			Description: "a draft needs at least one recipient", Properties: []string{"to"}}
	default:
		return &SetError{Type: setInvalidProps, Description: err.Error()}
	}
}

// toComposeAddrs converts JMAP EmailAddress objects to compose addresses.
func toComposeAddrs(in []jmapEmailAddr) []compose.Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]compose.Address, 0, len(in))
	for _, a := range in {
		if strings.TrimSpace(a.Email) == "" {
			continue
		}
		out = append(out, compose.Address{Name: a.Name, Email: strings.TrimSpace(a.Email)})
	}
	return out
}

// bodyText resolves a body part list to its text, via bodyValues (RFC 8621
// §4.1.4) or an inline value on the part.
func bodyText(values map[string]jmapBodyValue, parts []jmapBodyPart) string {
	var b strings.Builder
	for _, p := range parts {
		switch {
		case p.Value != "":
			b.WriteString(p.Value)
		case p.PartID != "":
			if v, ok := values[p.PartID]; ok {
				b.WriteString(v.Value)
			}
		}
	}
	return b.String()
}

// firstMsgID takes the first id of an inReplyTo list, stripped of brackets.
func firstMsgID(ids []string) string {
	for _, id := range ids {
		if s := strings.Trim(strings.TrimSpace(id), "<>"); s != "" {
			return s
		}
	}
	return ""
}

// trimMsgIDs strips angle brackets from a References list.
func trimMsgIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s := strings.Trim(strings.TrimSpace(id), "<>"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// msgIDList renders a single Message-ID as the JMAP List<String> RFC 8621 uses
// for messageId/inReplyTo (null when absent).
func msgIDList(id string) any {
	if id == "" {
		return nil
	}
	return []string{id}
}

// nonNilStrings renders a possibly-nil id list as a JSON array (never null).
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
