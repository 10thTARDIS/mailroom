// SPDX-License-Identifier: AGPL-3.0-only

package nudge

import (
	"errors"
	"strings"

	"mailroom/internal/kind"
	"mailroom/internal/mime"
)

// BodySource says where Asks read a message's body from.
type BodySource int

const (
	BodyNone BodySource = iota // no body text at all
	BodyText                   // text/plain
	BodyHTML                   // the HTML-only fallback (HTMLText)
)

// ParseContent fills m's parsed fields from its raw RFC 5322 bytes, through
// the shipped MIME parser and the kind projection (FactsFromHeader). It is the
// ONE parse both the report and the live nudge use, so a message cannot look
// different to the two of them.
func ParseContent(raw []byte, m *Message) (BodySource, error) {
	msg, err := mime.ParseBytes(raw)
	if err != nil {
		return BodyNone, err
	}
	if msg == nil {
		return BodyNone, errors.New("nudge: unparseable message")
	}
	h := msg.Header
	addrs := func(as []mime.Address) []string {
		out := make([]string, 0, len(as))
		for _, a := range as {
			if a.Address != "" {
				out = append(out, strings.ToLower(a.Address))
			}
		}
		return out
	}
	m.To, m.Cc, m.Bcc = addrs(h.To()), addrs(h.Cc()), addrs(h.Bcc())
	m.Facts = FactsFromHeader(msg)
	src := BodyNone
	body := msg.Text
	switch {
	case strings.TrimSpace(body) != "":
		src = BodyText
	case strings.TrimSpace(msg.HTML) != "":
		body = HTMLText(msg.HTML)
		src = BodyHTML
	}
	m.Question, m.Request = Asks(h.Subject(), body)
	if m.InReplyTo == "" {
		if irt := h.InReplyTo(); len(irt) > 0 {
			m.InReplyTo = irt[0]
		}
	}
	m.Parsed = true
	return src, nil
}

// FactsFromHeader projects a parsed message into the kind classifier's
// inputs. HasCorresponded is left false: it is per-user history, which the
// caller supplies as of the message (InboundFeatures).
func FactsFromHeader(msg *mime.Message) kind.Facts {
	h := msg.Header
	from := ""
	if addrs := h.From(); len(addrs) > 0 {
		from = strings.ToLower(addrs[0].Address)
	}
	domain := ""
	if at := strings.LastIndex(from, "@"); at > 0 {
		domain = from[at+1:]
	}
	f := kind.Facts{
		FromAddress: from,
		FromDomain:  domain,
		// h.Subject() DECODES RFC 2047 encoded-words; h.Get("Subject") would
		// hand the subject lexicons base64 (the bug the admin copy of this
		// projection once had).
		Subject:             h.Subject(),
		ListID:              h.Get("List-Id"),
		ListUnsubscribe:     h.Get("List-Unsubscribe"),
		ListUnsubscribePost: h.Get("List-Unsubscribe-Post"),
		AutoSubmitted:       h.Get("Auto-Submitted"),
		Precedence:          h.Get("Precedence"),
		FeedbackID:          firstNonEmpty(h.Get("Feedback-ID"), h.Get("X-Feedback-ID")),
		ReturnPath:          h.Get("Return-Path"),
		RecipientCount:      len(h.To()) + len(h.Cc()),
		IsReply:             h.Get("In-Reply-To") != "",
	}
	f.DKIMDomains = kind.DKIMDomains(h.Values("DKIM-Signature"))
	return f
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
