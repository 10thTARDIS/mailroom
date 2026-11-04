// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	"mailroom/internal/auth"
	"mailroom/internal/mcp"
	"mailroom/internal/store"
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
	needed, why := b.usesNeed(ctx, uses...)
	if why != "" {
		return []string{why}
	}
	p, _ := keyContext(ctx) // non-nil: usesNeed fails closed without a key
	out := missingScopes(p.key.Scopes, needed)
	sort.Strings(out)
	return out
}

// usesNeed is every scope the uses need, sorted and without repeats, computed
// from the same tables the gate reads (keyPolicies, emailUpdateFieldScopes),
// never from request text. why is "" or, when no scope would make the uses
// possible (a method closed to keys, an account restriction the method cannot
// keep to, an unclassified field), mcp.LacksRestriction / mcp.LacksUnavailable
// and needed is nil. Fails closed: a context with no key is unavailable.
func (b mcpBridge) usesNeed(ctx context.Context, uses ...mcp.Use) (needed []string, why string) {
	p, isKey := keyContext(ctx)
	userID, ok := contextUserID(ctx)
	if !isKey || !ok {
		return nil, mcp.LacksUnavailable
	}
	set := map[string]bool{}
	for _, u := range uses {
		pol, known := keyPolicies[u.Method]
		if !known || !pol.keyReachable() {
			return nil, mcp.LacksUnavailable
		}
		if p.restricted() && !pol.accountRestricted {
			return nil, mcp.LacksRestriction
		}
		var scopes []string
		switch {
		case len(u.EmailUpdate) > 0:
			// An Email/set update needs the scope of each EmailUpdate field
			// its patch sets; a field the table does not classify is refused,
			// as emailUpdateScopes refuses it.
			if u.Method != "Email/set" {
				return nil, mcp.LacksUnavailable
			}
			for _, f := range u.EmailUpdate {
				s, classified := emailUpdateFieldScopes[f]
				if !classified {
					return nil, mcp.LacksUnavailable
				}
				scopes = append(scopes, s)
			}
		case pol.needs != nil:
			// The method's own needs function on representative arguments. An
			// Email/set update is classified by field (above), never here: its
			// needs function would parse the patch against the database.
			if u.Method == "Email/set" && hasUpdate(u.Args) {
				return nil, mcp.LacksUnavailable
			}
			args := u.Args
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			n, mErr := pol.needs(ctx, b.a, userID, p, args)
			if mErr != nil {
				return nil, mcp.LacksUnavailable
			}
			scopes = n.scopes
		default:
			scopes = pol.scopes
		}
		for _, s := range scopes {
			set[s] = true
		}
	}
	needed = make([]string, 0, len(set))
	for s := range set {
		needed = append(needed, s)
	}
	sort.Strings(needed)
	return needed, ""
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
		if p, _ := keyContext(r.Context()); p != nil && p.key.Kind == store.KeyKindOAuth && a.mcpMetadata != "" {
			a.serveWithStepUp(inner, w, r, p)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// SetMCPResourceMetadata turns on the OAuth step-up challenge (ADR-050
// section 8): the protected-resource metadata URL its WWW-Authenticate names.
// Call it before serving, when the OAuth authorization server is on.
func (a *API) SetMCPResourceMetadata(url string) { a.mcpMetadata = url }

// bufferedResponse holds a response until the step-up check has seen whether
// it stands. /mcp answers in JSON mode (one write per exchange), so buffering
// holds nothing back that a client could have used earlier.
type bufferedResponse struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.h }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

func (b *bufferedResponse) flushTo(w http.ResponseWriter) {
	for k, v := range b.h {
		w.Header()[k] = v
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}

// serveWithStepUp serves an OAuth-authenticated /mcp request. A tools/call
// that the key gate refused SOLELY for a scope the grant lacks is answered 403
// with the RFC 6750 section 3.1 insufficient_scope challenge, so a client that
// supports step-up can send the user back to consent (ADR-050 section 8). The
// call ran through the dispatcher like any other, so its refusal is audited and
// charged exactly as for a key; only the answer differs.
//
// The challenge names EVERY scope the tool's declared uses need (the MCP
// authorization specification asks for the whole operation, not the missing
// piece), computed by usesNeed from the same tables KeyMayCall reads, never
// from the request text; the request supplies only the tool NAME, a lookup key
// into the catalog. Anything else that stops a call (an account restriction,
// rate, budget, a hold, a suspension, bad arguments) keeps today's tool error,
// and so does a request this cannot read.
func (a *API) serveWithStepUp(inner http.Handler, w http.ResponseWriter, r *http.Request, p *keyPrincipal) {
	if r.Method != http.MethodPost {
		inner.ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Method != "tools/call" {
		inner.ServeHTTP(w, r)
		return
	}
	rec := &bufferedResponse{h: http.Header{}}
	inner.ServeHTTP(rec, r)
	if p.scopeRefused.Load() {
		if scopes := a.stepUpScopes(r.Context(), p, msg.Params.Name); len(scopes) > 0 {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+strings.Join(scopes, " ")+
				`", resource_metadata="`+a.mcpMetadata+`"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "forbidden: this tool needs more permission than the connection has", http.StatusForbidden)
			return
		}
	}
	rec.flushTo(w)
}

// stepUpScopes is the scopes a refused tool call asks the client to request, or
// nil when no step-up applies: an unknown tool, a tool no scope could open, or
// a grant that already holds everything the tool needs (so the refusal was not
// one a new scope would cure).
func (a *API) stepUpScopes(ctx context.Context, p *keyPrincipal, tool string) []string {
	for _, t := range mcp.Catalog() {
		if t.Name != tool {
			continue
		}
		needed, why := mcpBridge{a}.usesNeed(ctx, t.Uses...)
		if why != "" || len(missingScopes(p.key.Scopes, needed)) == 0 {
			return nil
		}
		return needed
	}
	return nil
}

// MCPChallenge answers an MCP request that carries no credential at all —
// no Authorization header and no session cookie — with the bearer gate's 401
// and its WWW-Authenticate: Bearer challenge, which is how an MCP client
// learns it needs a key. Anything else goes to next (the auth chain), so a
// bad key still gets the bearer gate's uniform 401 and a browser session
// reaches mcpHandler's 403. cmd/mailroom mounts it outside the auth chain on
// MCPPath only.
func MCPChallenge(next http.Handler, sessionCookie string) http.Handler {
	return MCPChallengeWith(next, sessionCookie, "")
}

// MCPChallengeWith is MCPChallenge with the WWW-Authenticate value to send: the
// OAuth resource-metadata challenge when the authorization server is on
// (ADR-050 section 2). Empty means the plain `Bearer realm="mailroom"`.
func MCPChallengeWith(next http.Handler, sessionCookie, challenge string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth := r.Header["Authorization"]
		_, cookieErr := r.Cookie(sessionCookie)
		if !hasAuth && cookieErr != nil {
			if challenge != "" {
				w.Header().Set("WWW-Authenticate", challenge)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			refuseKey(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
