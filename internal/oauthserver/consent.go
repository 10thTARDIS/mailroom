// SPDX-License-Identifier: AGPL-3.0-only

package oauthserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// The consent screen (ADR-050 section 5): server-rendered, one inline style
// pinned by CSP hash, no script, framing refused. It is the API key form for an
// app: what it may read, what it may change, and sending and deleting with the
// ask-first choice beside each, in the words Settings uses.
//
// form-action is deliberately NOT in the policy: Chromium applies it to the
// redirect that follows the POST, which goes to the client's redirect URI, so
// 'self' would break every approval there. With no script and every value
// escaped by html/template there is no injection to exploit it through.

// ScopeWords is each scope in plain words, the primary text of the consent
// screen (the raw name is shown beside it, secondary).
//
// TWIN: these are web/src/app/i18n.ts's "apiKeys.scope.*" entries, which the
// key dialog shows. Change one, change the other: a test in internal/api
// compares this table with the closed scope set (ADR-042) and with those
// entries, so neither can drift.
var ScopeWords = map[string]string{
	"mail:read":      "Read mail",
	"search:read":    "Search mail",
	"labels:read":    "See labels",
	"contacts:read":  "See contacts",
	"mail:move":      "Move, mark and label mail",
	"labels:write":   "Create and edit labels",
	"mail:draft":     "Write drafts",
	"contacts:write": "Edit contacts",
	"mail:send":      "Send mail",
	"mail:delete":    "Delete mail (to Trash)",
}

// Wording shared with the key dialog (i18n.ts "apiKeys.create.*"), copied
// exactly.
const (
	wordsAskFirstFor  = "Ask me before it can: "                                                                                // apiKeys.create.askFirstFor
	wordsAskFirstHint = "Each request waits for you to approve or deny it here. Unanswered requests are denied after 24 hours." // apiKeys.create.askFirstHint
)

// scopeWords is a scope in words; the raw name for one the table lacks, so a
// new scope is shown rather than hidden.
func scopeWords(scope string) string {
	if w, ok := ScopeWords[scope]; ok {
		return w
	}
	return scope
}

// The page's palette is CSS custom properties, light first and dark under
// prefers-color-scheme. consent_test.go checks the contrast of every pair.
const pageCSS = `:root{color-scheme:light dark;--bg:#ffffff;--fg:#1a1a1a;--muted:#4b5563;--line:#6b7280;--btn:#1d4ed8;--btn-fg:#ffffff;--warn-bg:#fffbeb;--warn-line:#92400e;--err-bg:#fef2f2;--err-line:#b91c1c;--focus:#1d4ed8}
@media (prefers-color-scheme:dark){:root{--bg:#111827;--fg:#f3f4f6;--muted:#d1d5db;--line:#9ca3af;--btn:#93c5fd;--btn-fg:#0b1220;--warn-bg:#2a1f05;--warn-line:#fbbf24;--err-bg:#3b0d0d;--err-line:#fca5a5;--focus:#93c5fd}}
body{margin:0;background:var(--bg);color:var(--fg);font:1rem/1.5 system-ui,sans-serif}
main{max-width:40rem;margin:0 auto;padding:1rem}
h1{font-size:1.5rem;line-height:1.25}
fieldset{border:1px solid var(--line);border-radius:.5rem;margin:1rem 0;padding:.5rem 1rem .75rem;min-width:0}
legend{font-weight:600;padding:0 .25rem}
label{display:block;padding:.3rem 0}
.ask{margin-left:1.75rem}
input[type=checkbox],input[type=radio]{width:1.25rem;height:1.25rem;margin:0 .5rem 0 0;vertical-align:-.2rem}
input[type=number],input[type=text]{display:block;box-sizing:border-box;width:100%;max-width:20rem;min-height:2.75rem;margin-top:.25rem;padding:.4rem .6rem;font:inherit;color:var(--fg);background:var(--bg);border:1px solid var(--line);border-radius:.375rem}
code{font-size:.875em;color:var(--muted);overflow-wrap:anywhere}
.hint{margin:.25rem 0;color:var(--muted);font-size:.875rem}
.warn,.err{margin:1rem 0;padding:.5rem 1rem;border:2px solid;border-radius:.5rem}
.warn{background:var(--warn-bg);border-color:var(--warn-line)}
.err{background:var(--err-bg);border-color:var(--err-line)}
.new{font-weight:700}
.sub{margin-left:1.75rem}
.actions{display:flex;flex-wrap:wrap;gap:.75rem;margin:1.5rem 0}
button{min-height:2.75rem;padding:.5rem 1.25rem;font:inherit;cursor:pointer;border:2px solid var(--btn);border-radius:.375rem}
button.primary{background:var(--btn);color:var(--btn-fg)}
button.secondary{background:transparent;color:var(--fg);border-color:var(--line)}
:focus-visible{outline:3px solid var(--focus);outline-offset:2px}
@media (max-width:30rem){main{padding:.75rem}.actions button{flex:1 1 100%}.ask,.sub{margin-left:0}}`

func sha256Source(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

var pageCSP = strings.Join([]string{
	"default-src 'none'",
	"style-src " + sha256Source(pageCSS),
	"base-uri 'none'",
	"frame-ancestors 'none'",
}, "; ")

// scopeRow is one scope on the page. Destructive scopes carry the ask-first
// choice, shown right beside them.
type scopeRow struct {
	Name        string
	Words       string
	Checked     bool
	New         bool
	Destructive bool
	AskChecked  bool
	AskLabel    string
}

// scopeGroup is one fieldset of scopes: read, change, or sending and deleting.
type scopeGroup struct {
	Title       string
	Hint        string
	Destructive bool
	Rows        []scopeRow
	ShowAck     bool // the typed confirmation, in the destructive group only
}

type accountRow struct {
	ID      string
	Name    string
	Checked bool
}

type consentView struct {
	CSS             template.CSS
	RequestID       string
	ClientName      string
	Verified        bool   // identified by the host of its client_id
	ClientHost      string // that host
	Registered      string // "manual" for the user's own registration
	RedirectHost    string
	LoopbackWarning bool
	Groups          []scopeGroup
	Unavailable     []string
	Accounts        []accountRow
	Restricted      bool
	Rate            int
	Budget          int
	Suspend         bool
	Reauth          bool
	Error           string
	MaxRate         int
	MaxBudget       int
	AskHint         string
}

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="referrer" content="no-referrer"><title>Approve an app - Mailroom</title><style>{{.CSS}}</style></head>
<body><main>
<h1>Approve {{.ClientName}}?</h1>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
{{if .Verified}}<p><strong>{{.ClientName}}</strong> from <strong>{{.ClientHost}}</strong> wants to use your mailbox.</p>
{{else if eq .Registered "manual"}}<p><strong>{{.ClientName}}</strong> is an app you registered yourself.</p>
{{else}}<p><strong>{{.ClientName}}</strong> wants to use your mailbox. <em>Name given by the app, not verified.</em></p>{{end}}
<p>After you decide, you are sent back to <strong>{{.RedirectHost}}</strong>.</p>
{{if .LoopbackWarning}}<p class="warn" role="note">This app receives the approval on this computer. Approve only if you started this from an app you trust.</p>{{end}}
{{if .Reauth}}<p>This app is already connected. The settings below are its current ones; scopes marked <span class="new">new</span> are newly requested.</p>{{end}}
<form method="post" action="/oauth/consent">
<input type="hidden" name="request" value="{{.RequestID}}">
{{range $g := .Groups}}<fieldset><legend>{{$g.Title}}</legend>
<p class="hint">{{$g.Hint}}</p>
{{range $g.Rows}}<label><input type="checkbox" name="scope" value="{{.Name}}"{{if .Checked}} checked{{end}}> {{.Words}} <code>{{.Name}}</code>{{if .New}} <span class="new">(new)</span>{{end}}</label>
{{if .Destructive}}<label class="ask"><input type="checkbox" name="confirm" value="{{.Name}}"{{if .AskChecked}} checked{{end}}> {{.AskLabel}}</label>
{{end}}{{end}}
{{if $g.Destructive}}<p class="hint">{{$.AskHint}}</p>{{end}}
{{if $g.ShowAck}}<p class="hint" id="ack-help">Only needed if you allow both sending and deleting. An app with both can mail anyone as you and then remove the trace.</p>
<label for="ack">To allow both, type: <code>send and delete</code></label>
<input type="text" id="ack" name="ack" autocomplete="off" spellcheck="false" aria-describedby="ack-help">
{{end}}</fieldset>
{{end}}
{{if .Unavailable}}<p class="hint">Asked for, but not available here: {{range $i, $s := .Unavailable}}{{if $i}}, {{end}}<code>{{$s}}</code>{{end}}</p>{{end}}
<fieldset><legend>Which accounts</legend>
<label><input type="radio" name="accounts_mode" value="all"{{if not .Restricted}} checked{{end}}> Every account</label>
<label><input type="radio" name="accounts_mode" value="some"{{if .Restricted}} checked{{end}}> Only these accounts</label>
{{range .Accounts}}<label class="sub"><input type="checkbox" name="account" value="{{.ID}}"{{if .Checked}} checked{{end}}> {{.Name}}</label>{{end}}
</fieldset>
<fieldset><legend>Limits</legend>
<label for="rate">Calls per minute</label>
<input type="number" id="rate" name="rate" min="1" max="{{.MaxRate}}" value="{{.Rate}}" aria-describedby="rate-help">
<p class="hint" id="rate-help">1 to {{.MaxRate}}.</p>
<label for="budget">Calls per 24 hours</label>
<input type="number" id="budget" name="budget" min="1" max="{{.MaxBudget}}" value="{{.Budget}}" aria-describedby="budget-help">
<p class="hint" id="budget-help">1 to {{.MaxBudget}}.</p>
<label><input type="checkbox" name="suspend" value="1"{{if .Suspend}} checked{{end}}> Suspend the app automatically if it misbehaves</label>
</fieldset>
<div class="actions"><button type="submit" class="primary" name="action" value="approve">Approve</button>
<button type="submit" class="secondary" name="action" value="deny">Deny</button></div>
</form></main></body></html>`))

var errorTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>This request cannot continue - Mailroom</title><style>{{.CSS}}</style></head><body><main><h1>This request cannot continue</h1><p class="err" role="alert">{{.Msg}}</p></main></body></html>`))

func setPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", pageCSP)
}

func errorPage(w http.ResponseWriter, status int, msg string) {
	var buf bytes.Buffer
	if err := errorTmpl.Execute(&buf, map[string]any{"CSS": template.CSS(pageCSS), "Msg": msg}); err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	setPageHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func renderConsent(w http.ResponseWriter, status int, v consentView) {
	v.CSS = template.CSS(pageCSS)
	var buf bytes.Buffer
	if err := consentTmpl.Execute(&buf, v); err != nil {
		errorPage(w, http.StatusInternalServerError, "The page could not be shown.")
		return
	}
	setPageHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// offered is the requested scopes that are in the closed set, in the closed
// set's order, and the requested ones that are not.
func (s *Server) offered(requested []string) (known, unknown []string) {
	for _, sc := range s.policy.Scopes {
		if slices.Contains(requested, sc) {
			known = append(known, sc)
		}
	}
	for _, sc := range requested {
		if !slices.Contains(s.policy.Scopes, sc) && !slices.Contains(unknown, sc) {
			unknown = append(unknown, sc)
		}
	}
	return known, unknown
}

// ceiling is every scope the consent page offers, and so the most a decision may
// grant: the request's scopes that are in the closed set, plus (on
// re-authorization) the scopes the grant already holds. consentFor renders it
// and parseDecision enforces it, from this one function so they cannot drift.
func (s *Server) ceiling(requested, grantScopes []string) []string {
	known, _ := s.offered(requested)
	var out []string
	for _, sc := range s.policy.Scopes {
		if slices.Contains(known, sc) || slices.Contains(grantScopes, sc) {
			out = append(out, sc)
		}
	}
	return out
}

func (s *Server) buildConsent(ctx context.Context, tx *store.UserTx, req store.NewOAuthRequest, rawID, errMsg string) (consentView, error) {
	return s.consentFor(ctx, tx, req.ClientID, req.ClientName, req.ClientKind, req.RedirectHost, req.AllLoopback, req.Scopes, rawID, errMsg)
}

func (s *Server) consentFor(ctx context.Context, tx *store.UserTx, clientID, name, kind, redirectHost string, loopback bool, requested []string, rawID, errMsg string) (consentView, error) {
	known, unknown := s.offered(requested)
	v := consentView{RequestID: rawID, ClientName: name, RedirectHost: redirectHost, LoopbackWarning: loopback,
		Unavailable: unknown, Rate: s.policy.DefaultRate, Budget: s.policy.DefaultBudget, Suspend: s.policy.DefaultSuspend,
		Error: errMsg, MaxRate: s.policy.MaxRate, MaxBudget: s.policy.MaxBudget}
	if kind == store.OAuthClientCIMD {
		v.Verified, v.ClientHost = true, hostOf(clientID)
	}
	if kind == store.OAuthClientManual {
		v.Registered = "manual"
	}
	grant, hasGrant, err := tx.LockOAuthGrantForClient(ctx, clientID)
	if err != nil {
		return v, err
	}
	accounts, err := tx.ListAccounts(ctx)
	if err != nil {
		return v, err
	}
	scopes := known
	if hasGrant {
		v.Reauth = true
		scopes = s.ceiling(requested, grant.Scopes)
		v.Rate, v.Budget, v.Suspend = grant.RatePerMin, grant.DailyBudget, grant.SuspendOnAnomaly
		v.Restricted = grant.AccountIDs != nil
	}
	v.Groups, v.AskHint = s.groupsFor(scopes, grant, hasGrant)
	for _, a := range accounts {
		row := accountRow{ID: a.ID.String(), Name: a.Name}
		if hasGrant && grant.AccountIDs != nil {
			for _, id := range grant.AccountIDs {
				if id == a.ID {
					row.Checked = true
				}
			}
		}
		v.Accounts = append(v.Accounts, row)
	}
	return v, nil
}

// groupsFor arranges the offered scopes into the page's groups: read, change,
// and sending and deleting, each scope in words with its raw name beside it.
func (s *Server) groupsFor(scopes []string, grant store.APIKey, hasGrant bool) ([]scopeGroup, string) {
	groups := []*scopeGroup{
		{Title: "What it may read", Hint: "Looking only; nothing is changed."},
		{Title: "What it may change", Hint: "Changes your mail, labels and contacts, but never sends or deletes."},
		{Title: "Sending and deleting", Destructive: true,
			Hint: "These have the most effect, so each can wait for your approval before it happens."},
	}
	offeredDestructive := 0
	for _, sc := range scopes {
		row := scopeRow{Name: sc, Words: scopeWords(sc), Checked: true}
		if hasGrant && !slices.Contains(grant.Scopes, sc) {
			row.New = true
		}
		g := groups[1]
		switch {
		case slices.Contains(s.policy.Destructive, sc):
			g = groups[2]
			// Destructive scopes start as "ask me first", and keep the grant's
			// own choice on re-authorization unless newly requested.
			row.Destructive, row.AskChecked = true, true
			row.AskLabel = wordsAskFirstFor + scopeWords(sc)
			if hasGrant && slices.Contains(grant.Scopes, sc) {
				row.AskChecked = slices.Contains(grant.ConfirmScopes, sc)
			}
			offeredDestructive++
		case strings.HasSuffix(sc, ":read"):
			g = groups[0]
		}
		g.Rows = append(g.Rows, row)
	}
	// The typed confirmation is needed, and so shown, only when the page offers
	// every destructive scope, which is what parseDecision requires of "both".
	groups[2].ShowAck = len(s.policy.Destructive) > 0 && offeredDestructive == len(s.policy.Destructive)
	var out []scopeGroup
	for _, g := range groups {
		if len(g.Rows) > 0 {
			out = append(out, *g)
		}
	}
	return out, wordsAskFirstHint
}

var errRefused = errors.New("oauthserver: decision refused")

// Consent is POST /oauth/consent: the user's decision. It must be mounted
// behind the session gate; it carries its own auth.CSRFGuard, so a cross-site
// POST is refused (403) wherever it is mounted. The request id is single use and
// is the caller's own (RLS): a forged, replayed, expired or other user's
// decision is refused identically.
func (s *Server) Consent() http.Handler {
	return auth.CSRFGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			errorPage(w, http.StatusBadRequest, "The form could not be read.")
			return
		}
		rawID := r.PostForm.Get("request")
		action := r.PostForm.Get("action")
		if rawID == "" || (action != "approve" && action != "deny") {
			errorPage(w, http.StatusBadRequest, "This approval is no longer valid. Start again from the app.")
			return
		}
		hash := hashOf(rawID)

		var (
			req      store.OAuthRequest
			redirect string
			refusal  string
			gone     bool
		)
		err := s.st.UserTx(r.Context(), userID, func(tx *store.UserTx) error {
			var found bool
			var err error
			req, found, err = tx.GetOAuthRequest(r.Context(), hash)
			if err != nil {
				return err
			}
			if !found || req.DecidedAt != nil {
				gone = true
				return errRefused
			}
			if action == "deny" {
				if decided, err := tx.DecideOAuthRequest(r.Context(), req.ID); err != nil || !decided {
					gone = !decided
					if err == nil {
						err = errRefused
					}
					return err
				}
				redirect = "deny"
				return nil
			}
			var grantScopes []string
			if g, ok, err := tx.LockOAuthGrantForClient(r.Context(), req.ClientID); err != nil {
				return err
			} else if ok {
				grantScopes = g.Scopes
			}
			in, msg := s.parseDecision(r, req, s.ceiling(req.Scopes, grantScopes))
			if msg != "" {
				refusal = msg
				return errRefused
			}
			if decided, err := tx.DecideOAuthRequest(r.Context(), req.ID); err != nil || !decided {
				gone = !decided
				if err == nil {
					err = errRefused
				}
				return err
			}
			// The same writer ApiKey/set uses: validation, caps, audit.
			key, msg, err := s.approve(r.Context(), tx, in)
			if err != nil {
				return err
			}
			if msg != "" {
				refusal = msg
				return errRefused
			}
			code, codeHash, err := randomSecret("mrcode_")
			if err != nil {
				return err
			}
			if err := tx.InsertOAuthCode(r.Context(), store.NewOAuthCode{ID: store.NewULID(), Hash: codeHash,
				APIKeyID: key.ID, ClientID: req.ClientID, RedirectURI: req.RedirectURI,
				CodeChallenge: req.CodeChallenge, Resource: req.Resource, Scopes: key.Scopes, TTL: CodeTTL}); err != nil {
				return err
			}
			redirect = code
			return nil
		})
		switch {
		case err == nil:
		case errors.Is(err, errRefused) && gone:
			errorPage(w, http.StatusBadRequest, "This approval has expired or was already used. Start again from the app.")
			return
		case errors.Is(err, errRefused):
			// The request is still undecided (the transaction rolled back):
			// show the page again with the reason.
			var v consentView
			verr := s.st.UserTx(r.Context(), userID, func(tx *store.UserTx) error {
				var e error
				v, e = s.consentFor(r.Context(), tx, req.ClientID, req.ClientName, req.ClientKind, req.RedirectHost,
					req.AllLoopback, req.Scopes, rawID, refusal)
				return e
			})
			if verr != nil {
				errorPage(w, http.StatusServiceUnavailable, "Something went wrong on our side. Try again in a moment.")
				return
			}
			renderConsent(w, http.StatusUnprocessableEntity, v)
			return
		default:
			s.log.ErrorContext(r.Context(), "oauth: consent", "err", err)
			errorPage(w, http.StatusServiceUnavailable, "Something went wrong on our side. Try again in a moment.")
			return
		}
		if redirect == "deny" {
			s.redirectError(w, r, req.RedirectURI, req.State, "access_denied", "the user denied the request")
			return
		}
		s.redirectBack(w, r, req.RedirectURI, req.State, map[string]string{"code": redirect})
	}), "", s.log)
}

// parseDecision turns the posted form into a grant approval, enforcing the
// ceiling: a grant can hold only scopes the page offered (the request's, in the
// closed set), however the form was crafted.
func (s *Server) parseDecision(r *http.Request, req store.OAuthRequest, offered []string) (GrantApproval, string) {
	f := r.PostForm
	in := GrantApproval{ClientID: req.ClientID, ClientName: req.ClientName, ClientKind: req.ClientKind,
		RedirectHost: req.RedirectHost}
	for _, sc := range f["scope"] {
		if !slices.Contains(offered, sc) {
			return in, "That scope was not part of the request: " + sc + "."
		}
		if !slices.Contains(in.Scopes, sc) {
			in.Scopes = append(in.Scopes, sc)
		}
	}
	if len(in.Scopes) == 0 {
		return in, "Choose at least one thing the app may do."
	}
	in.ConfirmScopes = []string{}
	for _, sc := range f["confirm"] {
		if slices.Contains(s.policy.Destructive, sc) && slices.Contains(in.Scopes, sc) && !slices.Contains(in.ConfirmScopes, sc) {
			in.ConfirmScopes = append(in.ConfirmScopes, sc)
		}
	}
	if f.Get("accounts_mode") == "some" {
		in.AccountIDs = []string{}
		for _, id := range f["account"] {
			if _, err := ulid.Parse(id); err != nil {
				return in, "Choose the accounts from the list."
			}
			in.AccountIDs = append(in.AccountIDs, id)
		}
		if len(in.AccountIDs) == 0 {
			return in, "Choose at least one account, or allow all of them."
		}
	}
	var err error
	if in.RatePerMinute, err = strconv.Atoi(strings.TrimSpace(f.Get("rate"))); err != nil {
		return in, "Calls per minute must be a number."
	}
	if in.DailyBudget, err = strconv.Atoi(strings.TrimSpace(f.Get("budget"))); err != nil {
		return in, "Calls per day must be a number."
	}
	in.SuspendOnAnomaly = f.Get("suspend") == "1"
	hasBoth := true
	for _, sc := range s.policy.Destructive {
		hasBoth = hasBoth && slices.Contains(in.Scopes, sc)
	}
	if hasBoth && len(s.policy.Destructive) > 0 {
		if strings.ToLower(strings.Join(strings.Fields(f.Get("ack")), " ")) != "send and delete" {
			return in, "To allow both sending and deleting, type: send and delete"
		}
		in.AcknowledgeSendAndDelete = true
	}
	return in, ""
}
