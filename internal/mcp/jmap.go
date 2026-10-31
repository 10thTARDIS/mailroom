// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// jmap is a tool's view of the Backend: build calls, run them as ONE JMAP
// request, and turn method errors into tool errors.
type jmap struct{ b Backend }

// call is one method call to make.
type call struct {
	id     string
	method string
	args   any
}

// response is one method response, decoded.
type response struct {
	name string
	args map[string]any
	raw  json.RawMessage
}

// run makes the calls as one JMAP request and returns the responses by call
// id. A request that could not be made at all is a serverFail tool error.
func (j *jmap) run(ctx context.Context, calls ...call) (map[string]response, error) {
	invs := make([]Invocation, 0, len(calls))
	for _, c := range calls {
		raw, err := json.Marshal(c.args)
		if err != nil {
			return nil, err
		}
		invs = append(invs, Invocation{Name: c.method, Args: raw, CallID: c.id})
	}
	out, err := j.b.Call(ctx, invs)
	if err != nil {
		return nil, &toolError{Type: "forbidden", Description: err.Error()}
	}
	byID := make(map[string]response, len(out))
	for _, inv := range out {
		var args map[string]any
		if err := json.Unmarshal(inv.Args, &args); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", inv.Name, err)
		}
		byID[inv.CallID] = response{name: inv.Name, args: args, raw: inv.Args}
	}
	return byID, nil
}

// result is the arguments of call id's response, or the tool error its
// method error becomes (forbidden, overQuota, rateLimit, invalidArguments, …).
func result(rs map[string]response, id string) (map[string]any, error) {
	r, ok := rs[id]
	if !ok {
		return nil, errorf("serverFail", "the server returned no response to %s", id)
	}
	if r.name == "error" {
		return nil, &toolError{Type: str(r.args["type"]), Description: str(r.args["description"])}
	}
	return r.args, nil
}

// one makes a single method call and returns its arguments.
func (j *jmap) one(ctx context.Context, method string, args any) (map[string]any, error) {
	rs, err := j.run(ctx, call{id: "0", method: method, args: args})
	if err != nil {
		return nil, err
	}
	return result(rs, "0")
}

// setOutcome is a standard /set response for a bulk update or destroy: the ids
// that succeeded and the per-object SetErrors (RFC 8620 §5.3).
type setOutcome struct {
	done    []string
	failed  map[string]map[string]any
	pending []string // ids whose SetError is pendingApproval
	pendID  string
}

// readSet reads `updated`/`destroyed` and `notUpdated`/`notDestroyed`.
func readSet(res map[string]any, doneKey, failedKey string) setOutcome {
	o := setOutcome{failed: map[string]map[string]any{}}
	switch d := res[doneKey].(type) {
	case map[string]any:
		for id := range d {
			o.done = append(o.done, id)
		}
	case []any:
		for _, id := range d {
			o.done = append(o.done, str(id))
		}
	}
	sort.Strings(o.done)
	if f, ok := res[failedKey].(map[string]any); ok {
		for id, e := range f {
			se, _ := e.(map[string]any)
			if str(se["type"]) == "pendingApproval" {
				o.pending = append(o.pending, id)
				o.pendID = str(se["pendingActionId"])
				continue
			}
			o.failed[id] = map[string]any{"type": se["type"], "description": se["description"]}
		}
	}
	sort.Strings(o.pending)
	return o
}

// bulkResult turns a bulk /set outcome into a tool result: success when every
// object succeeded, else a tool error naming each failure (and what did
// succeed, so the model does not retry those).
func bulkResult(o setOutcome, verb string, extra map[string]any) (output, error) {
	data := map[string]any{"emailIds": nonNil(o.done)}
	for k, v := range extra {
		data[k] = v
	}
	if len(o.failed) > 0 {
		data["failed"] = o.failed
		typ := "partialFailure"
		if len(o.done) == 0 && len(o.failed) == 1 {
			for _, e := range o.failed {
				typ = str(e["type"])
			}
		}
		return output{}, &toolError{Type: typ,
			Description: fmt.Sprintf("%d of %d messages could not be %s", len(o.failed), len(o.failed)+len(o.done), verb),
			Detail:      data}
	}
	return output{data: data, summary: fmt.Sprintf("%d messages %s.", len(o.done), verb)}, nil
}

// pendingResult describes a call that a person must approve first. It is a
// normal result, not an error: the request was accepted and is waiting.
func (j *jmap) pendingResult(ctx context.Context, pendingID string, extra map[string]any) (output, error) {
	data := map[string]any{"status": "pending_approval", "pendingId": pendingID}
	for k, v := range extra {
		data[k] = v
	}
	// The expiry and the server's summary are on the PendingAction itself.
	// Reading it is best effort: the hold already happened either way.
	if res, err := j.one(ctx, "PendingAction/get", map[string]any{"ids": []string{pendingID}}); err == nil {
		if list := arr(res["list"]); len(list) == 1 {
			p := obj(list[0])
			data["expiresAt"] = p["expiresAt"]
			untrusted := map[string]any{"summary": p["summary"]}
			for _, k := range []string{"subject", "recipients"} {
				if v, ok := p[k]; ok {
					untrusted[k] = v
				}
			}
			data["untrusted_content"] = untrusted
		}
	}
	return output{data: data, hasUntrusted: true,
		summary: "waiting for the account owner's approval (pending " + pendingID + "); nothing has happened yet. " +
			"Check it with get_pending; unanswered requests are denied after 24 hours."}, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// pick copies the named keys of m that are present.
func pick(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// keywordList is a JMAP keyword set as a sorted list.
func keywordList(v any) []string {
	out := []string{}
	for k, on := range obj(v) {
		if b, _ := on.(bool); b {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// idList is a JMAP id set (mailboxIds) as a sorted list.
func idList(v any) []string { return keywordList(v) }
