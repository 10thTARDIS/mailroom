// SPDX-License-Identifier: AGPL-3.0-only

package api

import "encoding/json"

// JMAP error surfaces (RFC 8620). Three distinct shapes:
//
//   - requestError — a request-level failure (§3.6.1), returned as the whole
//     HTTP response body with a non-200 status.
//   - MethodError — a method-level error (§3.6.2), emitted in place of a normal
//     method response as ["error", {type, ...}, callId].
//   - SetError — a per-object error inside a /set response (§5.3), keyed by the
//     object's creation/update/destroy id.

// Standard method-level error types (RFC 8620 §3.6.2 + method sections).
const (
	errUnknownMethod          = "unknownMethod"
	errInvalidArguments       = "invalidArguments"
	errInvalidResultReference = "invalidResultReference"
	errAccountNotFound        = "accountNotFound"
	errCannotCalculateChanges = "cannotCalculateChanges"
	errStateMismatch          = "stateMismatch"
	errServerFail             = "serverFail"
	errRequestTooLarge        = "requestTooLarge"
	// errUnsupportedSort is RFC 8620 §5.5's refusal of a sort the method does
	// not implement (EmailSubmission/query has one fixed order, #653).
	errUnsupportedSort = "unsupportedSort"
	// errForbidden is RFC 8620 §3.6.2's method-level "forbidden": the call is
	// valid but this caller may not make it. API keys get it for every method
	// or scope their policy refuses (#49, ADR-042 §3).
	errForbidden = "forbidden"
	// errRateLimit / errOverQuota are an API key's per-minute rate and daily
	// budget (ADR-042 §5). Mailroom method-level types: a client that does not
	// know them treats them as a generic failure, which is the right reading.
	errRateLimit = "rateLimit"
	errOverQuota = "overQuota"
)

// Standard per-object SetError types (RFC 8620 §5.3).
const (
	setNotFound       = "notFound"
	setForbidden      = "forbidden"
	setInvalidProps   = "invalidProperties"
	setInvalidPatch   = "invalidPatch"
	setStateMismatch  = "stateMismatch"
	setWillDestroy    = "willDestroy"
	setOverQuota      = "overQuota"
	setNotImplemented = "notImplemented"
)

// setAccountNeedsReconnect is Mailroom's per-object refusal of a send on an
// account whose OAuth grant needs its user to sign in again (#52, ADR-046 §5).
// A stable type a client can key on; one that does not know it shows the
// description, which says what to do.
const setAccountNeedsReconnect = "accountNeedsReconnect"

// setAccountCannotSend is Mailroom's per-object refusal of a send on a
// RECEIVE-ONLY account: its provider refuses to take mail from it as a policy
// (Microsoft 5.7.139, SMTP AUTH disabled; ADR-046 §12). Stable for a client
// to key on; the description is store.SendBlockedReason, which says what to
// do. Reconnecting does not necessarily fix it, so it is not
// accountNeedsReconnect.
const setAccountCannotSend = "accountCannotSend"

// MethodError is a JMAP method-level error object.
type MethodError struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

func (e *MethodError) json() json.RawMessage {
	b, _ := json.Marshal(e)
	return b
}

// SetError is a JMAP per-object error inside a /set response.
type SetError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
	// PendingActionID is set on a "pendingApproval" SetError: the call was
	// held for a person, and this is the PendingAction to poll (#49, ADR-042 §7).
	PendingActionID string `json:"pendingActionId,omitempty"`
}

// requestError is a JMAP request-level error (application/problem+json).
type requestError struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// URN prefix for JMAP request-level error types.
const jmapErrURN = "urn:ietf:params:jmap:error:"

func newRequestError(kind string, status int, detail string) requestError {
	return requestError{Type: jmapErrURN + kind, Status: status, Detail: detail}
}
