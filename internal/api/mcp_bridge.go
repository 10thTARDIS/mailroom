// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"

	"mailroom/internal/auth"
	"mailroom/internal/mcp"
)

// THE MCP ENDPOINT'S SIDE OF THE API (#49, ADR-044).
//
// internal/mcp turns each tool call into a JMAP request; this file runs that
// request through dispatch — the same function POST /jmap uses — with the
// request's context, which carries the user and the key's enforced policy
// (keyPolicyGate). So an MCP tool call meets exactly the gate a JMAP call
// meets: scopes, the account restriction, rate and budget, confirm-mode holds,
// the audit and the anomaly rules (agent_gate.go). Nothing here decides
// whether a call is allowed.
//
// KEYS ONLY. /mcp refuses a browser session (403): MCP is a programmatic
// surface for API keys, and a cookie-authenticated tool call would be a
// cross-site request away from acting with the browser's whole authority.

// MCPPath is where the MCP endpoint is served (inside Handler, so nosniff,
// the rate limiter and keyPolicyGate wrap it).
const MCPPath = "/mcp"

// errMCPNeedsKey: a tool call whose context carries no enforced key policy.
// The HTTP handler refuses such a request before the SDK sees it; this is
// the second, independent refusal, so a future mount that skips the first
// still cannot run a call as a session.
var errMCPNeedsKey = errors.New("the MCP endpoint accepts only API keys")

// mcpUsing is every capability this server advertises: a tool's call is
// gated by the key policy, not by the `using` list.
var mcpUsing = func() []string {
	var out []string
	for urn := range capabilityObjects() {
		out = append(out, urn)
	}
	sort.Strings(out)
	return out
}()

// WithVersion sets the server version the MCP endpoint reports in initialize.
func WithVersion(v string) Option {
	return func(a *API) { a.version = v }
}

// mcpBridge implements mcp.Backend over dispatch.
type mcpBridge struct{ a *API }

// keyContext reports whether ctx is an API key's request with its policy
// loaded, and whose.
func keyContext(ctx context.Context) (*keyPrincipal, bool) {
	if _, isKey := auth.APIKeyIDFromContext(ctx); !isKey {
		return nil, false
	}
	p := principalFrom(ctx)
	return p, p != nil
}

// Call runs the invocations as one JMAP request through dispatch.
func (b mcpBridge) Call(ctx context.Context, calls []mcp.Invocation) ([]mcp.Invocation, error) {
	userID, ok := contextUserID(ctx)
	if !ok {
		return nil, errMCPNeedsKey
	}
	if _, isKey := keyContext(ctx); !isKey {
		return nil, errMCPNeedsKey
	}
	if len(calls) > maxCallsInRequest {
		return nil, errors.New("too many method calls")
	}
	req := Request{Using: mcpUsing, MethodCalls: make([]Invocation, 0, len(calls))}
	for _, c := range calls {
		req.MethodCalls = append(req.MethodCalls, Invocation{Name: c.Name, Args: c.Args, CallID: c.CallID})
	}
	resp := b.a.dispatch(ctx, userID, req)
	out := make([]mcp.Invocation, 0, len(resp.MethodResponses))
	for _, r := range resp.MethodResponses {
		out = append(out, mcp.Invocation{Name: r.Name, Args: r.Args, CallID: r.CallID})
	}
	return out, nil
}

// KeyMayCall reports whether the key in ctx may make every use, from the SAME
// table runKeyCall enforces (keyPolicies, and emailUpdateFieldScopes for an
// Email/set update). It mirrors runKeyCall's steps 2–4 without admission: a
// line that is reachable, the account restriction, and the scopes. It only
// filters tools/list; it is never consulted to allow a call.
func (b mcpBridge) KeyMayCall(ctx context.Context, uses ...mcp.Use) bool {
	return len(b.KeyLacks(ctx, uses...)) == 0
}

// KeyLacks is what stops the key in ctx from making every use (KeyMayCall's
// reasons): the scopes it lacks, sorted, or mcp.LacksRestriction /
// mcp.LacksUnavailable when no scope would help. Fails closed: a context with
// no key lacks everything. It words the answer to a hidden tool called
// anyway (#872); it is never consulted to allow a call.
func (b mcpBridge) KeyLacks(ctx context.Context, uses ...mcp.Use) []string {
	p, isKey := keyContext(ctx)
	userID, ok := contextUserID(ctx)
	if !isKey || !ok {
		return []string{mcp.LacksUnavailable}
	}
	missing := map[string]bool{}
	for _, u := range uses {
		pol, known := keyPolicies[u.Method]
		if !known || !pol.keyReachable() {
			return []string{mcp.LacksUnavailable}
		}
		if p.restricted() && !pol.accountRestricted {
			return []string{mcp.LacksRestriction}
		}
		var scopes []string
		switch {
		case len(u.EmailUpdate) > 0:
			// An Email/set update needs the scope of each EmailUpdate field
			// its patch sets; a field the table does not classify is refused,
			// as emailUpdateScopes refuses it.
			if u.Method != "Email/set" {
				return []string{mcp.LacksUnavailable}
			}
			for _, f := range u.EmailUpdate {
				s, classified := emailUpdateFieldScopes[f]
				if !classified {
					return []string{mcp.LacksUnavailable}
				}
				scopes = append(scopes, s)
			}
		case pol.needs != nil:
			// The method's own needs function on representative arguments. An
			// Email/set update is classified by field (above), never here: its
			// needs function would parse the patch against the database.
			if u.Method == "Email/set" && hasUpdate(u.Args) {
				return []string{mcp.LacksUnavailable}
			}
			args := u.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			n, mErr := pol.needs(ctx, b.a, userID, p, args)
			if mErr != nil {
				return []string{mcp.LacksUnavailable}
			}
			scopes = n.scopes
		default:
			scopes = pol.scopes
		}
		for _, s := range missingScopes(p.key.Scopes, scopes) {
			missing[s] = true
		}
	}
	out := make([]string, 0, len(missing))
	for s := range missing {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SendMode is how the key in ctx sends: not at all without mail:send (and
// with no key: fail closed), after a person's approval when mail:send is in
// its confirm scopes, else directly. It words tool descriptions and results
// only (#872): the gate decides every send.
func (b mcpBridge) SendMode(ctx context.Context) mcp.SendMode {
	p, isKey := keyContext(ctx)
	if !isKey || !slices.Contains(p.key.Scopes, scopeMailSend) {
		return mcp.SendNone
	}
	if p.confirms(scopeMailSend) {
		return mcp.SendWithApproval
	}
	return mcp.SendDirect
}

func hasUpdate(args json.RawMessage) bool {
	var s struct {
		Update map[string]json.RawMessage `json:"update"`
	}
	_ = json.Unmarshal(args, &s)
	return len(s.Update) > 0
}

// mcpHandler is the /mcp endpoint: keys only, then the MCP transport.
func (a *API) mcpHandler() http.Handler {
	inner := mcp.NewHandler(mcpBridge{a}, mcp.Options{Version: a.version, MaxRequestBytes: maxRequestBytes})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, isKey := keyContext(r.Context()); !isKey {
			http.Error(w, "forbidden: "+MCPPath+" accepts only an API key in Authorization: Bearer",
				http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// MCPChallenge answers an MCP request that carries no credential at all —
// no Authorization header and no session cookie — with the bearer gate's 401
// and its WWW-Authenticate: Bearer challenge, which is how an MCP client
// learns it needs a key. Anything else goes to next (the auth chain), so a
// bad key still gets the bearer gate's uniform 401 and a browser session
// reaches mcpHandler's 403. cmd/mailroom mounts it outside the auth chain on
// MCPPath only.
func MCPChallenge(next http.Handler, sessionCookie string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth := r.Header["Authorization"]
		_, cookieErr := r.Cookie(sessionCookie)
		if !hasAuth && cookieErr != nil {
			refuseKey(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
