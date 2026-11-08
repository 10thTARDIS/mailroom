// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"strings"
	"sync"

	"mailroom/internal/mime"
	"mailroom/internal/store"
)

// Email/get header properties (RFC 8621 §4.1.2, ADR-052): `header:Name[:asForm][:all]`
// and `headers`, answered from the stored message's header block.
//
// ⚠ THE VALUES ARE NEVER LOGGED, STORED OR INDEXED. They are computed per
// request from the decrypted bytes and returned; the only thing a failure logs
// is the message id (loadBodies).

const (
	// maxHeaderBlockBytes bounds how much of one message's header block is
	// parsed. Real header blocks are a few KiB; a hostile one is not allowed to
	// make a request pay for megabytes. A longer block is parsed up to its last
	// whole field and reported through `headersTruncated`.
	maxHeaderBlockBytes = 128 << 10

	// maxHeaderPropertyIDs bounds Email/get with header properties by ids: each
	// id costs one blob read, decrypt and integrity check. More is
	// requestTooLarge, so a client pages instead of one call becoming a hang.
	maxHeaderPropertyIDs = 200

	// maxHeaderProperties bounds the distinct header properties of one request.
	maxHeaderProperties = 64
)

// The parsed forms of RFC 8621 §4.1.2.
const (
	formRaw      = "asRaw"
	formText     = "asText"
	formAddr     = "asAddresses"
	formGrouped  = "asGroupedAddresses"
	formMsgIDs   = "asMessageIds"
	formDate     = "asDate"
	formURLs     = "asURLs"
	headersProp  = "headers"
	headerPrefix = "header:"
)

// formHeaders lists the header fields each restricted form is defined for
// (lower case). Raw and Text are defined for every header.
var formHeaders = map[string]map[string]bool{
	formAddr:    addressHeaders,
	formGrouped: addressHeaders,
	formMsgIDs:  set("message-id", "in-reply-to", "references", "resent-message-id"),
	formDate:    set("date", "resent-date"),
	formURLs: set("list-help", "list-unsubscribe", "list-subscribe", "list-post", "list-owner",
		"list-archive"),
}

var addressHeaders = set("from", "sender", "reply-to", "to", "cc", "bcc", "resent-from", "resent-sender",
	"resent-reply-to", "resent-to", "resent-cc", "resent-bcc")

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// headerSpec is one parsed header property.
type headerSpec struct {
	prop  string // exactly as the client wrote it: the key of the answer
	lname string // lower-cased field name
	form  string
	all   bool
}

// parseHeaderProperty parses `header:Name[:asForm][:all]`. The error text names
// the property and what is wrong with it.
func parseHeaderProperty(p string) (headerSpec, string) {
	parts := strings.Split(p, ":")
	if len(parts) < 2 || parts[0] != "header" {
		return headerSpec{}, "not a header property"
	}
	name := parts[1]
	if name == "" {
		return headerSpec{}, "the header field name is empty"
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x21 || name[i] > 0x7e {
			return headerSpec{}, "the header field name has a character that cannot be in one"
		}
	}
	sp := headerSpec{prop: p, lname: strings.ToLower(name), form: formRaw}
	rest := parts[2:]
	if n := len(rest); n > 0 && rest[n-1] == "all" {
		sp.all = true
		rest = rest[:n-1]
	}
	switch len(rest) {
	case 0:
	case 1:
		switch rest[0] {
		case formRaw, formText, formAddr, formGrouped, formMsgIDs, formDate, formURLs:
			sp.form = rest[0]
		default:
			return headerSpec{}, "unknown form " + `"` + rest[0] + `"` +
				" (asRaw, asText, asAddresses, asGroupedAddresses, asMessageIds, asDate, asURLs)"
		}
	default:
		return headerSpec{}, "too many parts: expected header:Name[:asForm][:all]"
	}
	if allowed, restricted := formHeaders[sp.form]; restricted && !allowed[sp.lname] {
		return headerSpec{}, sp.form + " is not defined for the " + name + " header field"
	}
	return sp, ""
}

// emailPropertySpecs validates an Email/get `properties` list against the
// properties the server has and the header grammar, returning the header
// properties. Anything else is invalidArguments, never silently dropped.
func emailPropertySpecs(properties []string) ([]headerSpec, *MethodError) {
	var (
		specs   []headerSpec
		seen    = map[string]bool{}
		unknown []string
	)
	for _, p := range properties {
		switch {
		case p == headersProp:
		case strings.HasPrefix(p, headerPrefix):
			sp, why := parseHeaderProperty(p)
			if why != "" {
				return nil, &MethodError{Type: errInvalidArguments, Description: "property " + `"` + p + `"` + ": " + why}
			}
			if !seen[p] {
				seen[p] = true
				specs = append(specs, sp)
			}
		case !knownEmailProperties()[p]:
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "unknown Email properties: " + strings.Join(unknown, ", ")}
	}
	if len(specs) > maxHeaderProperties {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "at most 64 header properties per request"}
	}
	return specs, nil
}

// headerSpecsOf recovers the header properties from the validated property set.
func headerSpecsOf(props map[string]bool) []headerSpec {
	var specs []headerSpec
	for p := range props {
		if strings.HasPrefix(p, headerPrefix) {
			if sp, why := parseHeaderProperty(p); why == "" {
				specs = append(specs, sp)
			}
		}
	}
	return specs
}

// wantsHeaders reports whether the property set asks for any header value.
func wantsHeaders(props map[string]bool) bool {
	if props[headersProp] || props["headersTruncated"] {
		return true
	}
	for p := range props {
		if strings.HasPrefix(p, headerPrefix) {
			return true
		}
	}
	return false
}

// addHeaderProperties adds the requested header properties to obj from the
// message's fields. body is nil when the instance has no blob store.
func addHeaderProperties(obj map[string]any, props map[string]bool, body *emailBody) {
	if body == nil || !wantsHeaders(props) {
		return
	}
	if body.unreadable {
		// Same honesty as the body properties (#419): no values, and the
		// reason, even if the client did not name bodyUnavailable.
		obj["bodyUnavailable"] = true
		return
	}
	obj["headersTruncated"] = body.headersTruncated
	if props[headersProp] {
		list := make([]map[string]any, 0, len(body.fields))
		for _, f := range body.fields {
			list = append(list, map[string]any{"name": f.Name, "value": f.Raw})
		}
		obj[headersProp] = list
	}
	for _, sp := range headerSpecsOf(props) {
		obj[sp.prop] = headerValue(sp, body.fields)
	}
}

// headerValue answers one header property: the last matching field, or every
// one in order for `:all` (RFC 8621 §4.1.2). A missing field is null (`[]` for
// :all); a value that does not parse in the asked form is null.
func headerValue(sp headerSpec, fields []mime.Field) any {
	var raws []string
	for _, f := range fields {
		if strings.EqualFold(f.Name, headerNameOf(sp)) {
			raws = append(raws, f.Raw)
		}
	}
	if sp.all {
		out := make([]any, 0, len(raws))
		for _, r := range raws {
			out = append(out, parseForm(sp.form, r))
		}
		return out
	}
	if len(raws) == 0 {
		return nil
	}
	return parseForm(sp.form, raws[len(raws)-1])
}

func headerNameOf(sp headerSpec) string { return sp.lname }

// parseForm renders one raw field value in a form; nil when it does not parse.
func parseForm(form, raw string) any {
	switch form {
	case formRaw:
		return raw
	case formText:
		return mime.FieldText(raw)
	case formAddr:
		addrs, ok := mime.FieldAddresses(raw)
		if !ok {
			return nil
		}
		out := make([]map[string]any, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, map[string]any{"name": nilIfEmpty(a.Name), "email": a.Email})
		}
		return out
	case formGrouped:
		groups, ok := mime.FieldGroupedAddresses(raw)
		if !ok {
			return nil
		}
		out := make([]map[string]any, 0, len(groups))
		for _, g := range groups {
			addrs := make([]map[string]any, 0, len(g.Addresses))
			for _, a := range g.Addresses {
				addrs = append(addrs, map[string]any{"name": nilIfEmpty(a.Name), "email": a.Email})
			}
			var name any
			if g.Named {
				name = g.Name
			}
			out = append(out, map[string]any{"name": name, "addresses": addrs})
		}
		return out
	case formMsgIDs:
		if ids, ok := mime.FieldMessageIDs(raw); ok {
			return ids
		}
		return nil
	case formURLs:
		if urls, ok := mime.FieldURLs(raw); ok {
			return urls
		}
		return nil
	case formDate:
		if d, ok := mime.FieldDate(raw); ok {
			return d
		}
		return nil
	}
	return nil
}

// knownEmailProperties is built once: every key the Email object renders, plus
// the ones Email/get adds around it (and the headersTruncated extension). A
// property missing here would turn a working one into invalidArguments, so a
// test pins it against every key emailGet can produce.
var knownEmailProperties = sync.OnceValue(func() map[string]bool {
	m := map[string]bool{}
	for k := range emailObject(store.EmailView{}, nil, &emailBody{}) {
		m[k] = true
	}
	for _, k := range emailGetExtraProperties {
		m[k] = true
	}
	return m
})

// emailGetExtraProperties are the properties emailGet adds to the object
// emailObject renders: each is resolved in one batch and attached by name.
var emailGetExtraProperties = []string{
	"categoryTab", "area", "areaProvenance", "drawerArchivedAt", "deskArchivedAt",
	"lineSide", "attachmentScan", "senderWarnings", "calendarInvite", "headersTruncated",
}
