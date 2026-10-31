// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file implements the RFC 8620 §3 request/response envelope for POST /jmap:
// decoding {using, methodCalls}, dispatching each method call (with result-
// reference resolution), and assembling {methodResponses, sessionState}.

// maxCallsInRequest bounds methodCalls per request (advertised in the session
// core capability). A generous cap that still refuses a pathological batch.
const maxCallsInRequest = 64

// Request is the JMAP request object (RFC 8620 §3.3).
type Request struct {
	Using       []string          `json:"using"`
	MethodCalls []Invocation      `json:"methodCalls"`
	CreatedIDs  map[string]string `json:"createdIds,omitempty"`
}

// Response is the JMAP response object (RFC 8620 §3.4). createdIds is echoed
// only when the client sent one (RFC 8620 §3.3): a client that tracks creation
// ids across requests gets the map back, one that does not is not burdened with
// it.
type Response struct {
	MethodResponses []Invocation      `json:"methodResponses"`
	SessionState    string            `json:"sessionState"`
	CreatedIDs      map[string]string `json:"createdIds,omitempty"`
}

// Invocation is a JMAP method call/response triple [name, arguments, callId]
// (RFC 8620 §3.2). It marshals to and from a heterogeneous JSON array.
type Invocation struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// UnmarshalJSON decodes the [name, args, callId] array form.
func (inv *Invocation) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 3 {
		return fmt.Errorf("api: invocation must be a 3-element array, got %d", len(raw))
	}
	if err := json.Unmarshal(raw[0], &inv.Name); err != nil {
		return fmt.Errorf("api: invocation name: %w", err)
	}
	inv.Args = raw[1]
	if err := json.Unmarshal(raw[2], &inv.CallID); err != nil {
		return fmt.Errorf("api: invocation callId: %w", err)
	}
	return nil
}

// MarshalJSON encodes back to the [name, args, callId] array form.
func (inv Invocation) MarshalJSON() ([]byte, error) {
	args := inv.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return json.Marshal([]any{inv.Name, args, inv.CallID})
}

// methodFn handles one JMAP method call. It returns the response arguments, or
// a method-level error to emit in place of a normal response.
//
// ⚠ IT DOES NOT RECEIVE THE `using` SET, AND THAT IS THE REASON MAILROOM'S
// CAPABILITY GATING IS PER-METHOD AND NOTHING ELSE (Forgejo #310). A handler
// cannot consult the client's declared capabilities because it is never handed
// them, so per-PROPERTY gating is structurally impossible here rather than weakly
// enforced — emailGet, for instance, builds its property set entirely from the
// client's own `properties` argument. The gate is mayCall, applied once, below.
// capability.go's comment on mayCall states in full what that does and does not
// buy; read it before writing a comment that claims a capability withholds
// anything.
type methodFn func(ctx context.Context, userID ulid.ULID, args json.RawMessage) (json.RawMessage, *MethodError)

// MethodTiming is how long one method call of a request took (#665).
type MethodTiming struct {
	// Name is the method name only when the server declares it and the client
	// may call it; anything else is the literal "unknown". The name is
	// client-supplied and becomes a metric label, so it must stay a bounded set.
	Name     string
	Duration time.Duration
	// Failed is true when the call answered with a method-level error.
	Failed bool
}

// RequestTiming is one POST /jmap request's timing: the whole dispatch loop and
// each call in the order it ran. It carries no arguments and no mail content.
type RequestTiming struct {
	UserID  ulid.ULID
	Total   time.Duration
	Methods []MethodTiming
}

// WithRequestTiming installs an observer called once per request, after the
// last method call, on the request's own goroutine. It is read-only: it cannot
// change a response. Without it dispatch does not even read the clock.
func WithRequestTiming(fn func(RequestTiming)) Option {
	return func(a *API) { a.onTiming = fn }
}

// dispatch runs every method call in order and returns the assembled response.
//
// Before each call, two kinds of back-reference are resolved against everything
// that happened EARLIER IN THE SAME REQUEST — which is what makes a batch a
// single logical operation rather than a list of independent calls:
//
//   - RESULT references (RFC 8620 §3.7): a "#arg" whose value names a prior
//     call's response and a path into it (`"#ids": {resultOf, name, path}`).
//   - CREATION-ID references (RFC 8620 §5.3): a "#creationId" used ANYWHERE an
//     Id is expected, naming an object created earlier in this request. This is
//     what makes RFC 8621 §7.5's canonical compose-and-send work as one request:
//     Email/set creates the draft as "draft", and the EmailSubmission/set in the
//     same batch submits `emailId: "#draft"` — nested inside `create`, which no
//     result reference can reach.
//
// A method whose CAPABILITY the client did not declare in `using` is answered
// exactly as an unknown method (RFC 8620 §3.3).
func (a *API) dispatch(ctx context.Context, userID ulid.ULID, req Request) Response {
	using := usingSet(req.Using)

	// Creation ids seeded by the client (RFC 8620 §3.3 createdIds) plus every id
	// this request creates, in order. It is the map "#draft" resolves against.
	createdIDs := make(map[string]string, len(req.CreatedIDs)+len(req.MethodCalls))
	for cid, id := range req.CreatedIDs {
		createdIDs[cid] = id
	}

	var timing *RequestTiming
	var loopStart time.Time
	if a.onTiming != nil {
		timing = &RequestTiming{UserID: userID, Methods: make([]MethodTiming, 0, len(req.MethodCalls))}
		loopStart = time.Now()
	}

	responses := make([]Invocation, 0, len(req.MethodCalls))
	for _, call := range req.MethodCalls {
		args, refErr := resolveReferences(call.Args, responses)
		if refErr != nil {
			if timing != nil {
				timing.Methods = append(timing.Methods, MethodTiming{Name: a.timingName(call.Name, using), Failed: true})
			}
			responses = append(responses, errorInvocation(call.CallID, refErr))
			continue
		}
		args = resolveCreationIDs(call.Name, args, createdIDs)

		fn, ok := a.methods[call.Name]
		if !ok || !mayCall(using, call.Name) {
			if timing != nil {
				timing.Methods = append(timing.Methods, MethodTiming{Name: "unknown", Failed: true})
			}
			responses = append(responses, errorInvocation(call.CallID,
				&MethodError{Type: errUnknownMethod}))
			continue
		}
		var callStart time.Time
		if timing != nil {
			callStart = time.Now()
		}
		result, mErr := a.invoke(ctx, userID, call.Name, args, fn)
		if timing != nil {
			timing.Methods = append(timing.Methods, MethodTiming{
				Name: call.Name, Duration: time.Since(callStart), Failed: mErr != nil})
		}
		if mErr != nil {
			responses = append(responses, errorInvocation(call.CallID, mErr))
			continue
		}
		recordCreatedIDs(result, createdIDs)
		responses = append(responses, Invocation{Name: call.Name, Args: result, CallID: call.CallID})
	}

	if timing != nil {
		timing.Total = time.Since(loopStart)
		a.onTiming(*timing)
	}

	resp := Response{MethodResponses: responses, SessionState: a.sessionState}
	if req.CreatedIDs != nil {
		resp.CreatedIDs = createdIDs
	}
	return resp
}

// timingName is the label a call is recorded under: its own name when the
// client may call it, "unknown" otherwise (see MethodTiming.Name).
func (a *API) timingName(name string, using map[string]bool) string {
	if _, ok := a.methods[name]; ok && mayCall(using, name) {
		return name
	}
	return "unknown"
}

// recordCreatedIDs harvests the creation ids a /set response just minted
// (`created: {"draft": {"id": "01J..."}}`) so a later call in the same request
// can refer to them as "#draft". It is intentionally generic over every /set:
// the response shape is the same for all of them (RFC 8620 §5.3).
func recordCreatedIDs(result json.RawMessage, into map[string]string) {
	var res struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		return
	}
	for cid, obj := range res.Created {
		if obj.ID != "" {
			into[cid] = obj.ID
		}
	}
}

// errorInvocation builds the ["error", {type,...}, callId] response for a
// method-level failure.
func errorInvocation(callID string, mErr *MethodError) Invocation {
	return Invocation{Name: "error", Args: mErr.json(), CallID: callID}
}

// resultReference is the value of a "#"-prefixed argument (RFC 8620 §3.7).
type resultReference struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// resolveReferences replaces every "#name" argument with the value produced by
// evaluating its result reference against a prior response, returning the
// rewritten arguments. A reference to a missing/mismatched call, or a path that
// does not resolve, yields an invalidResultReference method error.
//
// It recurses into NESTED argument positions, not just the top level. RFC 8620
// §3.7 describes the top-level form, but a client has every reason to write one
// inside a create — `create: {"sub": {"#emailId": {resultOf: ...}}}` — and a
// server that silently ignores it would hand the method a literal "#emailId"
// property instead of the id, which is worse than an error.
//
// A "#"-prefixed key is treated as a reference ONLY when its value is an object
// carrying a `resultOf` member. That test is load bearing: "#draft" is also the
// syntax for a CREATION-ID key (`update: {"#draft": {...patch}}`, RFC 8620
// §5.3), whose value is likewise an object — and rewriting one of those as a
// broken result reference would fail every compose-then-edit batch.
func resolveReferences(args json.RawMessage, prior []Invocation) (json.RawMessage, *MethodError) {
	if len(args) == 0 || !strings.Contains(string(args), "#") {
		return args, nil
	}
	out, _, mErr := resolveRefsIn(args, prior)
	if mErr != nil {
		return nil, mErr
	}
	return out, nil
}

// resolveRefsIn walks one JSON value, resolving result references wherever they
// appear. It re-encodes only the containers it actually changed, so untouched
// arguments keep their exact original bytes.
func resolveRefsIn(node json.RawMessage, prior []Invocation) (json.RawMessage, bool, *MethodError) {
	switch {
	case looksLikeObject(node):
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(node, &obj); err != nil {
			return node, false, nil // malformed: the method reports invalidArguments
		}
		changed := false
		for key, val := range obj {
			if strings.HasPrefix(key, "#") && isResultReference(val) {
				resolved, mErr := evalReference(val, key, prior)
				if mErr != nil {
					return nil, false, mErr
				}
				delete(obj, key)
				obj[strings.TrimPrefix(key, "#")] = resolved
				changed = true
				continue
			}
			sub, subChanged, mErr := resolveRefsIn(val, prior)
			if mErr != nil {
				return nil, false, mErr
			}
			if subChanged {
				obj[key] = sub
				changed = true
			}
		}
		if !changed {
			return node, false, nil
		}
		b, err := json.Marshal(obj)
		if err != nil {
			return nil, false, &MethodError{Type: errInvalidResultReference, Description: err.Error()}
		}
		return b, true, nil

	case looksLikeArray(node):
		var arr []json.RawMessage
		if err := json.Unmarshal(node, &arr); err != nil {
			return node, false, nil
		}
		changed := false
		for i, el := range arr {
			sub, subChanged, mErr := resolveRefsIn(el, prior)
			if mErr != nil {
				return nil, false, mErr
			}
			if subChanged {
				arr[i] = sub
				changed = true
			}
		}
		if !changed {
			return node, false, nil
		}
		b, err := json.Marshal(arr)
		if err != nil {
			return nil, false, &MethodError{Type: errInvalidResultReference, Description: err.Error()}
		}
		return b, true, nil
	}
	return node, false, nil
}

// isResultReference reports whether a "#key"'s value is an RFC 8620 §3.7
// ResultReference rather than, say, a creation-id-keyed patch object.
func isResultReference(v json.RawMessage) bool {
	if !looksLikeObject(v) {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil {
		return false
	}
	_, ok := obj["resultOf"]
	return ok
}

// evalReference resolves one ResultReference against the responses so far.
func evalReference(val json.RawMessage, key string, prior []Invocation) (json.RawMessage, *MethodError) {
	var ref resultReference
	if err := json.Unmarshal(val, &ref); err != nil {
		return nil, &MethodError{Type: errInvalidResultReference,
			Description: fmt.Sprintf("malformed result reference for %q", key)}
	}
	src := findResponse(prior, ref.ResultOf)
	if src == nil || src.Name != ref.Name {
		return nil, &MethodError{Type: errInvalidResultReference,
			Description: fmt.Sprintf("no prior %q response with callId %q", ref.Name, ref.ResultOf)}
	}
	resolved, err := evalPointer(src.Args, ref.Path)
	if err != nil {
		return nil, &MethodError{Type: errInvalidResultReference, Description: err.Error()}
	}
	return resolved, nil
}

// --- Creation-id references (RFC 8620 §5.3) ----------------------------------

// creationIDPaths names, per method, every argument position that holds an Id
// and may therefore carry a "#creationId" reference to an object created
// earlier in the same request.
//
// It is a WHITELIST of positions, not a blanket walk over every string, on
// purpose: a subject of "#draft", a body containing "#draft", or a search query
// for "#draft" must survive a request that also creates something called
// "draft". Rewriting Ids by position cannot corrupt user text; rewriting by
// value could.
//
// Path tokens:
//
//	literal — an object member
//	"*"     — every element of an array / every value of an object
//	"#"     — every KEY of an object (the `update`/`destroy` id maps)
//
// Intra-call references (a /set that destroys the object it created in the same
// call) are NOT listed: they are resolved inside the method against its own
// `created` map (resolveSetID), which is the only place they exist yet.
var creationIDPaths = map[string][]string{
	"Email/get":               {"ids/*"},
	"Email/set":               {"update/#", "destroy/*", "create/*/mailboxIds/#"},
	"Mailbox/get":             {"ids/*"},
	"Mailbox/set":             {"update/#", "destroy/*", "create/*/parentId"},
	"Thread/get":              {"ids/*"},
	"Thread/set":              {"update/#"},
	"Label/get":               {"ids/*"},
	"Label/set":               {"update/#", "destroy/*"},
	"Identity/get":            {"ids/*"},
	"Identity/set":            {"update/#", "destroy/*"},
	"EmailSubmission/get":     {"ids/*"},
	"EmailSubmission/set":     {"create/*/emailId", "update/#", "destroy/*"},
	"SearchSnippet/get":       {"emailIds/*"},
	"EmailSubmission/changes": nil,
}

// resolveCreationIDs rewrites "#creationId" references in the Id positions of
// this method's arguments. An unknown creation id is left ALONE — the method
// then reports it as the invalid id it is (notFound / invalidProperties), which
// tells the client far more than a generic request failure would.
func resolveCreationIDs(method string, args json.RawMessage, ids map[string]string) json.RawMessage {
	paths := creationIDPaths[method]
	if len(paths) == 0 || len(ids) == 0 || len(args) == 0 || !strings.Contains(string(args), "#") {
		return args
	}
	out := args
	for _, p := range paths {
		if rewritten, changed := rewriteIDPath(out, strings.Split(p, "/"), ids); changed {
			out = rewritten
		}
	}
	return out
}

// rewriteIDPath walks one path into the arguments and substitutes creation ids
// at its leaves. A node whose shape does not match the path is left untouched
// (the method will reject it on its own terms).
func rewriteIDPath(node json.RawMessage, tokens []string, ids map[string]string) (json.RawMessage, bool) {
	if len(tokens) == 0 {
		return substituteID(node, ids)
	}
	switch tokens[0] {
	case "#":
		// Rename the object's KEYS: {"#draft": patch} -> {"01J...": patch}.
		var obj map[string]json.RawMessage
		if !looksLikeObject(node) || json.Unmarshal(node, &obj) != nil {
			return node, false
		}
		changed := false
		for key, val := range obj {
			id, ok := creationID(key, ids)
			if !ok {
				continue
			}
			delete(obj, key)
			obj[id] = val
			changed = true
		}
		return remarshal(node, obj, changed)

	case "*":
		if looksLikeArray(node) {
			var arr []json.RawMessage
			if json.Unmarshal(node, &arr) != nil {
				return node, false
			}
			changed := false
			for i, el := range arr {
				if sub, ok := rewriteIDPath(el, tokens[1:], ids); ok {
					arr[i], changed = sub, true
				}
			}
			if !changed {
				return node, false
			}
			b, err := json.Marshal(arr)
			if err != nil {
				return node, false
			}
			return b, true
		}
		var obj map[string]json.RawMessage
		if !looksLikeObject(node) || json.Unmarshal(node, &obj) != nil {
			return node, false
		}
		changed := false
		for key, val := range obj {
			if sub, ok := rewriteIDPath(val, tokens[1:], ids); ok {
				obj[key], changed = sub, true
			}
		}
		return remarshal(node, obj, changed)

	default:
		var obj map[string]json.RawMessage
		if !looksLikeObject(node) || json.Unmarshal(node, &obj) != nil {
			return node, false
		}
		val, ok := obj[tokens[0]]
		if !ok {
			return node, false
		}
		sub, changed := rewriteIDPath(val, tokens[1:], ids)
		if !changed {
			return node, false
		}
		obj[tokens[0]] = sub
		return remarshal(node, obj, true)
	}
}

// substituteID replaces a JSON string that is a creation-id reference with the
// id it names.
func substituteID(node json.RawMessage, ids map[string]string) (json.RawMessage, bool) {
	var s string
	if json.Unmarshal(node, &s) != nil {
		return node, false
	}
	id, ok := creationID(s, ids)
	if !ok {
		return node, false
	}
	b, err := json.Marshal(id)
	if err != nil {
		return node, false
	}
	return b, true
}

// creationID resolves a "#creationId" token against the request's creation-id
// map. A string that is not a "#" reference, or names an id this request has not
// created, resolves to nothing.
func creationID(s string, ids map[string]string) (string, bool) {
	if !strings.HasPrefix(s, "#") {
		return "", false
	}
	id, ok := ids[strings.TrimPrefix(s, "#")]
	return id, ok
}

// remarshal re-encodes a rewritten object, falling back to the original bytes if
// nothing changed (or if the re-encode somehow fails).
func remarshal(original json.RawMessage, obj map[string]json.RawMessage, changed bool) (json.RawMessage, bool) {
	if !changed {
		return original, false
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return original, false
	}
	return b, true
}

// findResponse returns the prior response with the given callId (the first
// match; JMAP forbids duplicate callIds referenced this way), or nil. An
// "error" response is never a valid reference source.
func findResponse(prior []Invocation, callID string) *Invocation {
	for i := range prior {
		if prior[i].CallID == callID && prior[i].Name != "error" {
			return &prior[i]
		}
	}
	return nil
}
