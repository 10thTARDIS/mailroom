// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/notify"
	"mailroom/internal/store"
)

// THE ENFORCEMENT OF AN API KEY'S POLICY (#49, spec §13a, ADR-042).
//
// The bearer gate (internal/auth) says whose request it is. Everything a key
// may DO is decided here, and in this order, for every method call:
//
//  1. the policy is in the context, loaded fresh for this request under the
//     user's own UserTx — a request with a key id and no policy is refused
//     (keyPolicyGate and invoke both check, so neither alone is the control);
//  2. the method has a key-reachable line in the scope table;
//  3. a restricted key only reaches methods that honour the restriction;
//  4. the call's contents need no scope the key lacks;
//  5. admission, under the key's row lock: still active, within rate and
//     budget, not tripping an anomaly rule — and recorded in the audit before
//     it runs;
//  6. a destructive call in confirm mode is stored for a person instead of run.
//
// Every refusal is audited. Nothing here ever sees the token.

// keyPrincipal is an API key's policy as enforced for one request.
type keyPrincipal struct {
	key      store.APIKey
	scopes   map[string]bool
	confirm  map[string]bool
	clientIP string
	// approval is set only while PendingAction/set executes a request a
	// person approved: it skips the hold (that is what was approved) and the
	// anomaly refusals (a person looked at it), and nothing else.
	approval *keyApproval
	// suspended is set when a call of THIS request suspended the key (#49):
	// every later call in the request is refused as suspended at once.
	// Later requests never get this far — the bearer gate refuses a suspended
	// key with its uniform 401 (ADR-042 §2).
	suspended atomic.Bool
}

type keyApproval struct{ pendingID ulid.ULID }

func newPrincipal(k store.APIKey, clientIP string) *keyPrincipal {
	p := &keyPrincipal{key: k, scopes: map[string]bool{}, confirm: map[string]bool{}, clientIP: clientIP}
	for _, s := range k.Scopes {
		p.scopes[s] = true
	}
	for _, s := range k.ConfirmScopes {
		p.confirm[s] = true
	}
	return p
}

func (p *keyPrincipal) confirms(scope string) bool { return p != nil && p.confirm[scope] }

// keySuspendedError is the refusal of a call by a key that is suspended. It
// names the suspension and how it ends, because the caller holds the key and
// learns nothing from it it could not learn from the 401 its next request
// gets (#49).
func keySuspendedError(reason string) *MethodError {
	desc := "this API key is suspended"
	if reason != "" {
		desc += " (" + safeText(reason, 120) + ")"
	}
	return forbidden(desc + "; it can make no calls until the mailbox owner reactivates it in Settings, API keys")
}

// restricted reports whether the key is confined to some of the user's accounts.
func (p *keyPrincipal) restricted() bool { return p != nil && p.key.AccountIDs != nil }

type principalContextKey struct{}

func withPrincipal(ctx context.Context, p *keyPrincipal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// principalFrom is the enforced policy of the key making this request, or nil
// for a session request.
func principalFrom(ctx context.Context) *keyPrincipal {
	p, _ := ctx.Value(principalContextKey{}).(*keyPrincipal)
	return p
}

// keyPaths are the only routes an API key reaches. Downloads, uploads, the
// proxy and the event stream are refused to keys in this release: a key's
// account restriction is enforced in the method paths, not in those.
func keyPathAllowed(r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/jmap":
		return true
	case r.Method == http.MethodGet && (r.URL.Path == "/jmap/session" || r.URL.Path == "/.well-known/jmap"):
		return true
	case r.URL.Path == MCPPath: // every method: the MCP transport answers GET/DELETE with 405 (ADR-044)
		return true
	}
	return false
}

// keyPolicyGate loads the policy of a key-authenticated request into its
// context, or refuses the request. A session request passes untouched.
func (a *API) keyPolicyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyID, isKey := auth.APIKeyIDFromContext(r.Context())
		if !isKey {
			next.ServeHTTP(w, r)
			return
		}
		if !keyPathAllowed(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok || a.store == nil {
			refuseKey(w)
			return
		}
		var (
			k     store.APIKey
			found bool
		)
		if err := a.store.UserTx(r.Context(), userID, func(tx *store.UserTx) error {
			var err error
			k, found, err = tx.GetAPIKey(r.Context(), keyID)
			return err
		}); err != nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		// Revoked or suspended between the gate's lookup and now: the same
		// answer the gate gives.
		if !found || !k.Active() {
			refuseKey(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), newPrincipal(k, requestAddr(r)))))
	})
}

func refuseKey(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mailroom"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func requestAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// invoke runs one method call: directly for a session, through runKeyCall for
// a key. A context that names a key but carries no policy is refused here too,
// so a route that some day skips keyPolicyGate still cannot run a key's call
// unchecked.
func (a *API) invoke(ctx context.Context, userID ulid.ULID, name string, args json.RawMessage, fn methodFn) (json.RawMessage, *MethodError) {
	p := principalFrom(ctx)
	if _, isKey := auth.APIKeyIDFromContext(ctx); isKey && p == nil {
		return nil, &MethodError{Type: errForbidden, Description: "this request carries no enforced API key policy"}
	}
	if p == nil {
		return fn(ctx, userID, args)
	}
	return a.runKeyCall(ctx, userID, p, name, args, fn)
}

func forbidden(desc string) *MethodError { return &MethodError{Type: errForbidden, Description: desc} }

// runKeyCall is steps 2–6 for one call.
func (a *API) runKeyCall(ctx context.Context, userID ulid.ULID, p *keyPrincipal, name string, args json.RawMessage, fn methodFn) (json.RawMessage, *MethodError) {
	if p.suspended.Load() {
		return nil, keySuspendedError("suspended by an anomaly rule earlier in this request")
	}
	pol, known := keyPolicies[name]
	if !known || !pol.keyReachable() {
		why := whyNotOpened
		if known {
			why = pol.why
		}
		reason := store.RefusalMethod
		if known && pol.sessionOnly {
			reason = store.RefusalSessionOnly
		}
		a.auditRefusal(ctx, userID, p, name, store.KeyOutcomeDenied, map[string]any{"reason": reason})
		return nil, forbidden(name + " is not available to API keys: " + why)
	}
	if p.restricted() && !pol.accountRestricted {
		a.auditRefusal(ctx, userID, p, name, store.KeyOutcomeDenied, map[string]any{"reason": store.RefusalRestriction})
		return nil, forbidden("this key is restricted to some of your accounts, and " + name +
			" cannot keep to that restriction")
	}
	var needs keyNeeds
	if pol.needs != nil {
		var mErr *MethodError
		if needs, mErr = pol.needs(ctx, a, userID, p, args); mErr != nil {
			a.auditRefusal(ctx, userID, p, name, store.KeyOutcomeDenied,
				map[string]any{"reason": store.RefusalArguments, "error": mErr.Type})
			return nil, mErr
		}
	} else {
		needs = pol.staticNeeds(name, args)
	}
	if missing := missingScopes(p.key.Scopes, needs.scopes); len(missing) > 0 {
		a.auditRefusal(ctx, userID, p, name, store.KeyOutcomeDenied,
			map[string]any{"reason": store.RefusalScope, "missing": missing})
		return nil, forbidden("this key does not have the scope " + strings.Join(missing, ", ") + " that " + name +
			" needs here")
	}

	adm, mErr := a.admitKeyCall(ctx, userID, p, name, needs, args)
	if mErr != nil {
		return nil, mErr
	}
	if adm.response != nil {
		return adm.response, nil
	}
	result, mErr := fn(ctx, userID, args)
	if mErr != nil {
		_ = a.store.UserTx(context.WithoutCancel(ctx), userID, func(tx *store.UserTx) error {
			return tx.SetAPIKeyEventError(ctx, adm.eventID, mErr.Type)
		})
		return result, mErr
	}
	a.keyReadTopUp(ctx, userID, p, name, needs.cost, result)
	return result, nil
}

// keyReadTopUp charges a read for what it actually returned beyond what was
// charged up front (ids: null, no limit). The call that crosses the budget
// completes; the next one is refused. Recorded as its own audit row.
func (a *API) keyReadTopUp(ctx context.Context, userID ulid.ULID, p *keyPrincipal, name string, charged int, result json.RawMessage) {
	if isWriteMethod(name) {
		return
	}
	extra := readAnswerCost(name, result) - charged
	if extra <= 0 {
		return
	}
	if err := a.store.UserTx(context.WithoutCancel(ctx), userID, func(tx *store.UserTx) error {
		_, err := tx.InsertAPIKeyEvent(ctx, store.APIKeyEvent{KeyID: p.key.ID, Method: name,
			Outcome: store.KeyOutcomeOK, Cost: extra, ClientIP: p.clientIP,
			Summary: map[string]any{"topUp": true}})
		return err
	}); err != nil {
		slog.WarnContext(ctx, "could not charge an API key read", "key_id", p.key.ID.String(), "err", err)
	}
}

func missingScopes(have, need []string) []string {
	var out []string
	for _, s := range need {
		if !slices.Contains(have, s) {
			out = append(out, s)
		}
	}
	return out
}

// auditRefusal records a call refused before admission. Best effort: an
// audit write that fails must not turn a refusal into a success, and does not.
func (a *API) auditRefusal(ctx context.Context, userID ulid.ULID, p *keyPrincipal, method, outcome string, summary map[string]any) {
	if a.store == nil {
		return
	}
	reason, _ := summary["reason"].(string)
	// A refusal costs 1 against the key's per-minute rate (#49 review), so a
	// key looping on refused calls throttles its own later calls; how it is
	// written (in full, or counted past a cap) is writeRefusal's.
	var alert *agentAlert
	err := a.store.UserTx(context.WithoutCancel(ctx), userID, func(tx *store.UserTx) error {
		// The key's row lock, as admitKeyCall takes: concurrent refusals
		// cannot both read the count under the cap and both write in full.
		k, found, err := tx.LockAPIKey(ctx, p.key.ID)
		if err != nil || !found {
			return err
		}
		usage, err := tx.APIKeyUsageAt(ctx, k.ID, time.Now(), MassDeleteWindow, MassSendWindow)
		if err != nil {
			return err
		}
		al, suspended, err := writeRefusal(ctx, tx, k, usage, store.APIKeyEvent{KeyID: k.ID, Method: method,
			Outcome: outcome, Summary: summary, ClientIP: p.clientIP, PendingID: p.pendingID()}, reason)
		alert = al
		if suspended {
			p.suspended.Store(true)
		}
		return err
	})
	if err != nil {
		slog.WarnContext(ctx, "could not audit a refused API key call", "key_id", p.key.ID.String(),
			"method", method, "err", err)
		return
	}
	if alert != nil {
		a.sendAlert(userID, *alert)
	}
}

// CriticalRefusalCapFactor sets a key's per-minute cap of security-meaningful
// refusals written in full (store.CriticalRefusalReasons) as a multiple of
// its rate. Past it they are counted, and the anomaly is raised.
const CriticalRefusalCapFactor = 5

// CriticalRefusalAlertCooldown is how often one key's critical-refusal anomaly
// may be recorded and pushed.
const CriticalRefusalAlertCooldown = time.Hour

// refusalMethodName is the method a suppressed-refusal row is keyed by: the
// name only when it is one this server has (keyPolicies holds every
// dispatcher method, TestEveryMethodHasAKeyPolicy), never client text, so a
// key cannot mint a row per invented name.
func refusalMethodName(method string) string {
	if _, known := keyPolicies[method]; known {
		return method
	}
	return "unknown"
}

// writeRefusal records one refused call by the locked key k at cost 1 (it
// spends the key's per-minute rate). It is written in full to api_key_events
// while this minute's written refusals of its class are under their cap
// (routine: k.RatePerMin; security-meaningful: CriticalRefusalCapFactor x
// that); past the cap it is COUNTED in this minute's
// api_key_suppressed_refusals row for (outcome, known method or "unknown",
// reason) — bounded, and complete in aggregate. The first critical refusal
// past its cap raises the critical_refusals anomaly (recorded, and an alert
// returned for after the commit) at most once per key per
// CriticalRefusalAlertCooldown — unless the key has suspendOnAnomaly (the
// default), when that refusal SUSPENDS it (#49) and is recorded and
// pushed every time, as a suspending mass delete is: it can only recur after
// a person reactivated the key. suspended reports that this call suspended it.
func writeRefusal(ctx context.Context, tx *store.UserTx, k store.APIKey, usage store.APIKeyUsage, event store.APIKeyEvent, reason string) (alert *agentAlert, suspended bool, err error) {
	event.Cost = 1
	event.Reason = reason
	event.Summary = mergeSummary(event.Summary, map[string]any{"reason": reason})
	critical := store.IsCriticalRefusal(reason)
	written, limit := usage.RoutineRefusalsLastMinute, k.RatePerMin
	if critical {
		written, limit = usage.CriticalRefusalsLastMinute, CriticalRefusalCapFactor*k.RatePerMin
	}
	if written < limit {
		_, err := tx.InsertAPIKeyEvent(ctx, event)
		return nil, false, err
	}
	if err := tx.RecordSuppressedRefusal(ctx, k.ID, event.Outcome, refusalMethodName(event.Method), reason); err != nil {
		return nil, false, err
	}
	if !critical {
		return nil, false, nil
	}
	suspended, err = suspendForAnomaly(ctx, tx, k, store.AnomalyCriticalRefusals,
		fmt.Sprintf("more than %d security-meaningful refused attempts in a minute", limit))
	if err != nil {
		return nil, false, err
	}
	if !suspended {
		seen, err := tx.APIKeyAnomalySince(ctx, k.ID, store.AnomalyCriticalRefusals, time.Now().Add(-CriticalRefusalAlertCooldown))
		if err != nil || seen {
			return nil, false, err
		}
	}
	if _, err := tx.InsertAPIKeyEvent(ctx, store.APIKeyEvent{KeyID: k.ID, Method: refusalMethodName(event.Method),
		Outcome: store.KeyOutcomeAnomaly, Anomaly: store.AnomalyCriticalRefusals, ClientIP: event.ClientIP,
		Summary: map[string]any{"reason": reason, "perMinuteCap": limit, "suspended": suspended}}); err != nil {
		return nil, false, err
	}
	return anomalyAlert(k, "keeps being refused", fmt.Sprintf("It made more than %d refused attempts in a minute that matter for security "+
		"(outside its accounts, approvals, or actions only you can take).", limit), suspended), suspended, nil
}

// suspendForAnomaly suspends the key for an anomaly rule when its
// suspendOnAnomaly is on (the default; #49 applies it to every rule but
// off hours), writing the audit row (store.SuspendAPIKey). It reports whether
// THIS call suspended it: false for a key with the switch off, and for one
// already suspended or revoked, whatever the caller's copy of it says.
func suspendForAnomaly(ctx context.Context, tx *store.UserTx, k store.APIKey, rule, reason string) (bool, error) {
	if !k.SuspendOnAnomaly {
		return false, nil
	}
	return tx.SuspendAPIKey(ctx, k.ID, rule, reason)
}

// anomalyAlert is the urgent push for an anomaly rule. When the rule
// suspended the key the title says so and the body says how it ends (#49);
// otherwise the title is the rule's own.
func anomalyAlert(k store.APIKey, what, body string, suspended bool) *agentAlert {
	title := "API key “" + safeText(k.Name, 60) + "” " + what
	if suspended {
		title = "API key “" + safeText(k.Name, 60) + "” suspended"
		body += " It has been suspended and can make no calls until you reactivate it in Settings, API keys."
	}
	return &agentAlert{title: title, body: body, url: keyAlertURL(k.ID), urgent: true}
}

func (p *keyPrincipal) pendingID() *ulid.ULID {
	if p == nil || p.approval == nil {
		return nil
	}
	id := p.approval.pendingID
	return &id
}

// Anomaly rules (ADR-042 §6). Named so the thresholds are a reviewed decision.
const (
	// MassDeleteThreshold soft-deletes by one key within MassDeleteWindow trip
	// the rule: the call that would cross it is refused, an anomaly is
	// recorded and pushed, and the key is suspended when SuspendOnAnomaly is on
	// (the default). The bound is therefore on the damage, not on a report.
	MassDeleteThreshold = 50
	MassDeleteWindow    = 10 * time.Minute
	// MassSendThreshold sends within MassSendWindow: refused, recorded, pushed,
	// and the key suspended when SuspendOnAnomaly is on (#49). Sends cannot
	// be taken back, so the call is refused rather than run.
	MassSendThreshold = 10
	MassSendWindow    = 10 * time.Minute
	// MassAnomalyAlertCooldown: a mass rule tripped by a key that is not
	// suspended for it (suspendOnAnomaly off) is refused on every call but
	// recorded and pushed at most once per key and rule in this long (#852).
	MassAnomalyAlertCooldown = time.Hour
	// Off hours: the user's quiet hours when they are on; otherwise 00:00 to
	// 06:00 in their quiet-hours time zone (UTC if none). A key that CHANGES
	// something then is recorded and pushed — once per OffHoursCooldown — and
	// not refused: an overnight assistant is legitimate, an unnoticed one is
	// not.
	OffHoursDefaultEndMinute = 6 * 60
	OffHoursCooldown         = 6 * time.Hour
	// PendingApprovalWindow is how long a held call waits for a person.
	PendingApprovalWindow = 24 * time.Hour
	// MaxPendingPerKey is how many held calls one key may have waiting for a
	// person at once; past it a new one is refused (#852).
	MaxPendingPerKey = 10
)

type admission struct {
	eventID ulid.ULID
	// response, when set, is the call's whole answer (a held call): the
	// method does not run.
	response json.RawMessage
}

// admitKeyCall is step 5 (and 6, the hold) in ONE transaction under the key's
// row lock, so the limit check and the audit row that spends it cannot be
// split by a concurrent call.
func (a *API) admitKeyCall(ctx context.Context, userID ulid.ULID, p *keyPrincipal, method string, needs keyNeeds, args json.RawMessage) (admission, *MethodError) {
	now := a.clock()
	var (
		out     admission
		refusal *MethodError
		alerts  []agentAlert
	)
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		k, found, err := tx.LockAPIKey(ctx, p.key.ID)
		if err != nil {
			return err
		}
		switch {
		case !found:
			refusal = forbidden("this key has been revoked or suspended")
			return nil
		case k.RevokedAt != nil:
			refusal = forbidden("this key has been revoked")
			return nil
		case k.SuspendedAt != nil:
			refusal = keySuspendedError(k.SuspendedReason)
			return nil
		}
		// The scopes are checked again against the LOCKED row: a person who
		// narrowed the key a moment ago is not overruled by the copy this
		// request loaded.
		if missing := missingScopes(k.Scopes, needs.scopes); len(missing) > 0 {
			refusal = forbidden("this key no longer has the scope " + strings.Join(missing, ", "))
			return nil
		}
		// An approved run costs nothing: its held call was charged when it was
		// held (the pending row's cost), and charging again would make a
		// confirm-mode key pay twice and could rate-limit the very run a person
		// just approved (#49 review).
		cost := needs.cost
		if p.approval != nil {
			cost = 0
		}
		event := store.APIKeyEvent{KeyID: k.ID, Method: method, Cost: cost, Summary: needs.summary,
			ClientIP: p.clientIP, PendingID: p.pendingID()}
		var usage store.APIKeyUsage
		// A refusal is written at cost 1, which the per-minute rate counts, and
		// not at all once this minute's rate is spent (see auditRefusal).
		refuse := func(outcome, reason string, mErr *MethodError) error {
			refusal = mErr
			event.Outcome = outcome
			al, suspended, err := writeRefusal(ctx, tx, k, usage, event, reason)
			if al != nil {
				alerts = append(alerts, *al)
			}
			if suspended {
				p.suspended.Store(true)
			}
			return err
		}

		// The windows are measured on the clock that stamps the events (the
		// database's now(), i.e. the real time), never on the API's test-seam
		// clock: mixing the two lets a skewed clock open every window wide.
		usage, err = tx.APIKeyUsageAt(ctx, k.ID, time.Now(), MassDeleteWindow, MassSendWindow)
		if err != nil {
			return err
		}
		// An approved run is not held to the per-minute rate (#852): its call
		// was admitted, and paid for, when it was held, and the key's own
		// calls since (its refusals included) must not fail an approval a
		// person gave. The daily budget still applies.
		if p.approval == nil && usage.CostLastMinute+cost > k.RatePerMin {
			return refuse(store.KeyOutcomeRateLimited, store.RefusalRate, &MethodError{Type: errRateLimit,
				Description: fmt.Sprintf("this key's rate of %d per minute is spent; try again in a minute", k.RatePerMin)})
		}
		if usage.CostLast24h+cost > k.DailyBudget {
			return refuse(store.KeyOutcomeOverBudget, store.RefusalBudget, &MethodError{Type: errOverQuota,
				Description: fmt.Sprintf("this key's budget of %d per 24 hours is spent", k.DailyBudget)})
		}

		if p.approval == nil && needs.hold == "" {
			// Both mass rules refuse every call that would cross them. A key
			// with suspendOnAnomaly (the default; #49 for mass send too) is
			// SUSPENDED by that call, which is recorded and pushed every time: it
			// can only trip again after a person reactivated it, and then they
			// need to hear of it again. One with the switch off keeps calling, so
			// its record and push are once per key and rule per
			// MassAnomalyAlertCooldown (#852).
			massRule := func(anomaly, refusalReason, what, verb string, inWindow, thisCall, threshold int,
				window time.Duration) error {
				minutes := int(window.Minutes())
				suspended, err := suspendForAnomaly(ctx, tx, k, anomaly,
					fmt.Sprintf("mass %s: more than %d in %d minutes", what, threshold, minutes))
				if err != nil {
					return err
				}
				report := suspended
				if !report {
					seen, err := tx.APIKeyAnomalySince(ctx, k.ID, anomaly, time.Now().Add(-MassAnomalyAlertCooldown))
					if err != nil {
						return err
					}
					report = !seen
				}
				if report {
					if _, err := tx.InsertAPIKeyEvent(ctx, store.APIKeyEvent{KeyID: k.ID, Method: method,
						Outcome: store.KeyOutcomeAnomaly, Anomaly: anomaly, ClientIP: p.clientIP,
						Summary: map[string]any{what + "sInWindow": inWindow, "thisCall": thisCall,
							"threshold": threshold, "windowMinutes": minutes, "suspended": suspended}}); err != nil {
						return err
					}
					stopped := "stopped"
					if anomaly == store.AnomalyMassSend {
						stopped = "held back"
					}
					alerts = append(alerts, *anomalyAlert(k, stopped,
						fmt.Sprintf("It tried to %s more than %d messages in %d minutes.", verb, threshold, minutes), suspended))
				}
				desc := fmt.Sprintf("this call would %s more than %d messages within %d minutes, so it was refused",
					verb, threshold, minutes)
				if suspended {
					p.suspended.Store(true)
					desc += " and this key was suspended; the mailbox owner can reactivate it in Settings, API keys"
				}
				return refuse(store.KeyOutcomeDenied, refusalReason, forbidden(desc))
			}
			if needs.deletes > 0 && usage.DeletesInWindow+needs.deletes > MassDeleteThreshold {
				return massRule(store.AnomalyMassDelete, store.RefusalMassDelete, "delete", "delete",
					usage.DeletesInWindow, needs.deletes, MassDeleteThreshold, MassDeleteWindow)
			}
			if needs.sends > 0 && usage.SendsInWindow+needs.sends > MassSendThreshold {
				return massRule(store.AnomalyMassSend, store.RefusalMassSend, "send", "send",
					usage.SendsInWindow, needs.sends, MassSendThreshold, MassSendWindow)
			}
		}

		switch {
		case needs.hold != "":
			// A key's outstanding requests are capped (#852): each one is a
			// notification and a decision for a person, and an agent that
			// keeps asking without waiting is refused rather than queued.
			waiting, err := a.outstandingPending(ctx, tx, k.ID)
			if err != nil {
				return err
			}
			if waiting >= MaxPendingPerKey {
				return refuse(store.KeyOutcomeDenied, store.RefusalPendingCap, &MethodError{Type: errOverQuota,
					Description: fmt.Sprintf("this key already has %d requests waiting for your approval; "+
						"it can ask again once one of them is approved, denied or expired", MaxPendingPerKey)})
			}
			resp, summary, serr := a.holdForApproval(ctx, tx, userID, p, k, method, needs.hold, args, now)
			if serr != nil {
				return serr
			}
			if resp.pendingID == nil {
				// The request could not be held (no such draft, an invalid
				// send): answered with that error, nothing stored, nothing run.
				// Audited as a refusal for its arguments, so it has a reason
				// and the per-minute caps on refusals apply (#852).
				if err := refuse(store.KeyOutcomeDenied, store.RefusalArguments, nil); err != nil {
					return err
				}
				out.response = resp.body
				return nil
			}
			event.Outcome = store.KeyOutcomePending
			event.PendingID = resp.pendingID
			if out.eventID, err = tx.InsertAPIKeyEvent(ctx, event); err != nil {
				return err
			}
			out.response = resp.body
			alerts = append(alerts, agentAlert{title: "Approval needed",
				body: "“" + safeText(k.Name, 60) + "” asks: " + summary, url: apiKeysAlertURL})
		default:
			event.Outcome = store.KeyOutcomeOK
			if p.approval != nil {
				event.Outcome = store.KeyOutcomeApproved
				// Admitted to run, in the transaction that records it: the stuck-
				// approval sweep tells "never ran" from "ran" by this.
				ran, merr := tx.MarkAPIKeyPendingRan(ctx, p.approval.pendingID)
				if merr != nil {
					return merr
				}
				if !ran {
					// Not approved any more, or already run: one approval is
					// one run, so this admission is refused (#49 review).
					return refuse(store.KeyOutcomeDenied, store.RefusalApproval,
						forbidden("this approval has already been used or is no longer approved"))
				}
			}
			event.Deletes, event.Sends = needs.deletes, needs.sends
			if out.eventID, err = tx.InsertAPIKeyEvent(ctx, event); err != nil {
				return err
			}
		}

		if needs.cost > 0 && isWriteMethod(method) {
			al, err := a.offHoursCheck(ctx, tx, k, method, p.clientIP, now)
			if err != nil {
				return err
			}
			if al != nil {
				alerts = append(alerts, *al)
			}
		}
		return tx.TouchAPIKeyUsed(ctx, k.ID)
	})
	if err != nil {
		return admission{}, &MethodError{Type: errServerFail, Description: err.Error()}
	}
	// After the commit: an alert about something that rolled back would be a
	// false alarm.
	for _, al := range alerts {
		a.sendAlert(userID, al)
	}
	if refusal != nil {
		return admission{}, refusal
	}
	return out, nil
}

// outstandingPending counts the key's held calls still waiting for a person,
// after expiring the overdue ones (so a request nobody answered stops
// counting when its window ends, not when something next reads it). Runs
// under the key's row lock, so two concurrent holds cannot both see room.
func (a *API) outstandingPending(ctx context.Context, tx *store.UserTx, keyID ulid.ULID) (int, error) {
	if err := a.expirePending(ctx, tx); err != nil {
		return 0, err
	}
	list, err := tx.ListAPIKeyPending(ctx, store.PendingFilter{KeyID: &keyID,
		States: []string{store.PendingStatePending}}, MaxPendingPerKey)
	return len(list), err
}

func mergeSummary(base, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func isWriteMethod(name string) bool { return strings.HasSuffix(name, "/set") }

// offHoursCheck records (once per cooldown) a key changing something while the
// user is normally away.
func (a *API) offHoursCheck(ctx context.Context, tx *store.UserTx, k store.APIKey, method, ip string, now time.Time) (*agentAlert, error) {
	prefs, err := tx.GetDeskPreferences(ctx)
	if err != nil {
		return nil, err
	}
	if !isOffHours(prefs.QuietHours, now) {
		return nil, nil
	}
	seen, err := tx.APIKeyAnomalySince(ctx, k.ID, store.AnomalyOffHours, now.Add(-OffHoursCooldown))
	if err != nil || seen {
		return nil, err
	}
	if _, err := tx.InsertAPIKeyEvent(ctx, store.APIKeyEvent{KeyID: k.ID, Method: method,
		Outcome: store.KeyOutcomeAnomaly, Anomaly: store.AnomalyOffHours, ClientIP: ip,
		Summary: map[string]any{"quietHours": prefs.QuietHours.Enabled}}); err != nil {
		return nil, err
	}
	return &agentAlert{title: "API key “" + safeText(k.Name, 60) + "” is active",
		body: "It is changing your mail during your off hours.", url: keyAlertURL(k.ID), urgent: true}, nil
}

// isOffHours is the off-hours rule's clock (see OffHoursDefaultEndMinute).
func isOffHours(q store.QuietHours, now time.Time) bool {
	if q.Enabled {
		return quietHoursOf(q).Active(now)
	}
	loc := time.UTC
	if q.Timezone != "" {
		if l, err := time.LoadLocation(q.Timezone); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	return local.Hour()*60+local.Minute() < OffHoursDefaultEndMinute
}

func quietHoursOf(q store.QuietHours) notify.QuietHours {
	return notify.QuietHours{Enabled: q.Enabled, Start: q.Start, End: q.End,
		Days: uint8(q.Days & int(notify.AllDays)), Zone: q.Timezone, AllowVIP: q.AllowVIP}
}

// ---- alerts ----------------------------------------------------------------

// AlertNotifier delivers an agent alert to the user's devices through the
// existing push path (syncrunner.StorePushNotifier). urgent alerts — the
// anomaly rules — are security alerts and are delivered during quiet hours
// too; an approval request respects them (the banner is waiting either way).
type AlertNotifier interface {
	NotifyAgentAlert(ctx context.Context, userID ulid.ULID, title, body, url string, urgent bool) error
}

// WithAlertNotifier wires agent alerts to push. Without it they are recorded
// in the audit and not pushed.
func WithAlertNotifier(n AlertNotifier) Option { return func(a *API) { a.alerts = n } }

// Where an alert's notification opens. The web app routes by HASH: the server
// serves the app at "/" and answers "/settings/api-keys" with 404, so a path
// link opened a not-found page (#49, found building the keys UI). The API
// keys row lists the pending approvals, so an approval opens it; an alert
// about one key (an anomaly, off hours) opens that key's detail, where it is
// reactivated.
const apiKeysAlertURL = "/#/settings/api-keys"

func keyAlertURL(id ulid.ULID) string { return apiKeysAlertURL + "/" + id.String() }

type agentAlert struct {
	title, body, url string
	urgent           bool
}

// sendAlert pushes asynchronously: a push service that is slow must not hold
// up the agent's call or the approval.
func (a *API) sendAlert(userID ulid.ULID, al agentAlert) {
	if a.alerts == nil {
		return
	}
	url := al.url
	if url == "" {
		url = apiKeysAlertURL
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.baseCtx, 30*time.Second)
		defer cancel()
		if err := a.alerts.NotifyAgentAlert(ctx, userID, al.title, al.body, url, al.urgent); err != nil {
			slog.Warn("agent alert push failed", "user_id", userID.String(), "err", err)
		}
	}()
}

// safeText trims s for a notification or summary: control and format
// characters (bidi overrides included) removed, at most n runes.
func safeText(s string, n int) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if count == n {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String())
}

// clock is the API's notion of now (a test seam for the off-hours rule).
func (a *API) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}
