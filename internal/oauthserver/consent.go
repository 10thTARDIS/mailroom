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
// pinned by CSP hash, no script, framing refused. This is the minimal page of
// increment 1; the full screen is increment 2.
//
// form-action is deliberately NOT in the policy: Chromium applies it to the
// redirect that follows the POST, which goes to the client's redirect URI, so
// 'self' would break every approval there. With no script and every value
// escaped by html/template there is no injection to exploit it through.

const pageCSS = `body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem}
fieldset{margin:1rem 0}label{display:block}.warn{border:2px solid #b45309;padding:.5rem 1rem}
.err{border:2px solid #b91c1c;padding:.5rem 1rem}.new{font-weight:bold}small{opacity:.8}`

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

type scopeRow struct {
	Name    string
	Checked bool
	New     bool
}

type confirmRow struct {
	Scope   string
	Checked bool
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
	Scopes          []scopeRow
	Unavailable     []string
	Confirms        []confirmRow
	Accounts        []accountRow
	Restricted      bool
	Rate            int
	Budget          int
	Suspend         bool
	Reauth          bool
	Error           string
	MaxRate         int
	MaxBudget       int
}

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
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
<fieldset><legend>What it may do</legend>
{{range .Scopes}}<label><input type="checkbox" name="scope" value="{{.Name}}"{{if .Checked}} checked{{end}}> {{.Name}}{{if .New}} <span class="new">(new)</span>{{end}}</label>{{end}}
{{if .Unavailable}}<p><small>Not available: {{range $i, $s := .Unavailable}}{{if $i}}, {{end}}{{$s}}{{end}}</small></p>{{end}}
</fieldset>
{{if .Confirms}}<fieldset><legend>Ask me first</legend>
{{range .Confirms}}<label><input type="checkbox" name="confirm" value="{{.Scope}}"{{if .Checked}} checked{{end}}> Hold {{.Scope}} actions until I approve them</label>{{end}}
</fieldset>{{end}}
<fieldset><legend>Accounts</legend>
<label><input type="radio" name="accounts_mode" value="all"{{if not .Restricted}} checked{{end}}> All my accounts</label>
<label><input type="radio" name="accounts_mode" value="some"{{if .Restricted}} checked{{end}}> Only these:</label>
{{range .Accounts}}<label><input type="checkbox" name="account" value="{{.ID}}"{{if .Checked}} checked{{end}}> {{.Name}}</label>{{end}}
</fieldset>
<fieldset><legend>Limits</legend>
<label>Calls per minute (1 to {{.MaxRate}}) <input type="number" name="rate" min="1" max="{{.MaxRate}}" value="{{.Rate}}"></label>
<label>Calls per day (1 to {{.MaxBudget}}) <input type="number" name="budget" min="1" max="{{.MaxBudget}}" value="{{.Budget}}"></label>
<label><input type="checkbox" name="suspend" value="1"{{if .Suspend}} checked{{end}}> Suspend it automatically if it behaves unusually</label>
</fieldset>
<p>If you allow both sending and deleting, type <code>send and delete</code> to confirm that an app with both can mail anyone as you and then remove the trace.</p>
<label>Confirmation <input type="text" name="ack" autocomplete="off"></label>
<p><button type="submit" name="action" value="approve">Approve</button> <button type="submit" name="action" value="deny">Deny</button></p>
</form></main></body></html>`))

var errorTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Mailroom</title><style>{{.CSS}}</style></head><body><main><h1>This request cannot continue</h1><p class="err" role="alert">{{.Msg}}</p></main></body></html>`))

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
		scopes = nil
		for _, sc := range s.policy.Scopes {
			if slices.Contains(known, sc) || slices.Contains(grant.Scopes, sc) {
				scopes = append(scopes, sc)
			}
		}
		v.Rate, v.Budget, v.Suspend = grant.RatePerMin, grant.DailyBudget, grant.SuspendOnAnomaly
		v.Restricted = grant.AccountIDs != nil
	}
	for _, sc := range scopes {
		row := scopeRow{Name: sc, Checked: true}
		if hasGrant && !slices.Contains(grant.Scopes, sc) {
			row.New = true
		}
		v.Scopes = append(v.Scopes, row)
		if slices.Contains(s.policy.Destructive, sc) {
			// Destructive scopes start as "ask me first", and keep the grant's
			// own choice on re-authorization unless newly requested.
			checked := true
			if hasGrant && slices.Contains(grant.Scopes, sc) {
				checked = slices.Contains(grant.ConfirmScopes, sc)
			}
			v.Confirms = append(v.Confirms, confirmRow{Scope: sc, Checked: checked})
		}
	}
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

var errRefused = errors.New("oauthserver: decision refused")

// Consent is POST /oauth/consent: the user's decision. It must be mounted
// behind the session gate and auth.CSRFGuard. The request id is single use and
// is the caller's own (RLS): a forged, replayed, expired or other user's
// decision is refused identically.
func (s *Server) Consent() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			in, msg := s.parseDecision(r, req)
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
	})
}

// parseDecision turns the posted form into a grant approval, enforcing the
// ceiling: a grant can hold only scopes the page offered (the request's, in the
// closed set), however the form was crafted.
func (s *Server) parseDecision(r *http.Request, req store.OAuthRequest) (GrantApproval, string) {
	known, _ := s.offered(req.Scopes)
	f := r.PostForm
	in := GrantApproval{ClientID: req.ClientID, ClientName: req.ClientName, ClientKind: req.ClientKind,
		RedirectHost: req.RedirectHost}
	for _, sc := range f["scope"] {
		if !slices.Contains(known, sc) {
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
