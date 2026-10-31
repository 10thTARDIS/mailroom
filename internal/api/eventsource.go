// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// EventSource push (RFC 8620 §7.3). The session has advertised eventSourceUrl
// since 0.5 and nothing served it, so every client — including our own — fell
// back to polling Foo/changes on a timer.
//
// This is cheap to do RIGHT here because the server already has the exact thing
// a push channel needs: a per-user, per-type, monotonic change sequence
// (app.change_seq, read by store.UserTx.ChangeState) whose value IS the JMAP
// state string. So a StateChange event is a comparison of two integers, and the
// client that receives one asks Foo/changes what actually moved — the same
// authoritative delta it would have asked for on a timer, just without the wait.
//
// WHAT THIS IS NOT: it is not a notification bus. The states are POLLED from
// Postgres on the server side (eventSourceInterval) rather than pushed out of
// the write path. That is a deliberate trade for 0.5 — it needs no LISTEN/NOTIFY,
// no in-process fan-out, and no new invariant to keep — and it still turns N
// clients × M types of HTTP polling into one cheap indexed read per connection.
// The write-path fan-out (a store-side NOTIFY on change_log append) is a later
// optimization this handler can adopt without any client noticing.
//
// LIFETIME. A long-lived connection needs a bounded story or it becomes a way to
// pin the process open:
//
//   - one goroutine per stream: the HANDLER'S OWN. Nothing is spawned.
//   - the number of concurrent streams is capped (maxEventSourceConns); over the
//     cap the server says 503 rather than accepting a connection it cannot serve.
//   - it dies on client disconnect (r.Context()) and on server shutdown
//     (API.baseCtx, cancelled by main on SIGINT/SIGTERM) — otherwise graceful
//     shutdown would sit and wait for every open stream's deadline.

const (
	// eventSourceInterval is how often a stream re-reads the change states. It is
	// the push latency a client sees; it is also one small indexed read per type
	// per connection, so it is not free.
	eventSourceInterval = 2 * time.Second

	// maxEventSourceConns bounds concurrent streams across the whole server.
	maxEventSourceConns = 32

	// minPingInterval clamps a client's requested ping. RFC 8620 §7.3 lets the
	// server impose a minimum; a client asking to be pinged every 100ms is asking
	// for a busy loop.
	minPingInterval = 5 * time.Second
)

// eventSourceTypes are the JMAP types a client may subscribe to. They are the
// types the change log actually sequences (store.ObjectType), which is the same
// set Foo/changes serves.
var eventSourceTypes = []store.ObjectType{
	store.ObjectEmail,
	store.ObjectMailbox,
	store.ObjectThread,
	store.ObjectLabel,
	store.ObjectRule,
	store.ObjectIdentity,
	store.ObjectEmailSubmission,
	store.ObjectContact,
	// Area and DeskPreferences (#375, urn:mailroom:desk) each have their own
	// /changes method and append change_log rows like every other type here —
	// the desk client is about to subscribe to both rather than poll them.
	store.ObjectArea,
	store.ObjectDeskPreferences,
	// ClientPreferences (#375, urn:mailroom:desk) is the same story one type
	// over: it has its own /changes method and change_log rows, but was left
	// off this list, so the web client had to poll it instead — an unknown
	// name in `types` is a whole-request 400 (parseEventTypes below), so a
	// client cannot even subscribe to the types this server DOES push
	// alongside it without naming every one individually.
	store.ObjectClientPreferences,
	// SetupRun (#446, urn:mailroom:desk) is the one type here a client watches
	// for MINUTES rather than for the life of a session: the day-one import
	// drains in chunks and the wizard's progress comes from this type's change
	// rows. Leaving it off would make the wizard poll Setup/get on a timer —
	// which is exactly the cost the ClientPreferences omission above already
	// paid for once.
	store.ObjectSetupRun,
}

// EventSourceHandler implements GET /jmap/eventsource/ (RFC 8620 §7.3):
// text/event-stream, `state` events carrying a StateChange object, and the
// `types`, `closeafter` and `ping` parameters.
func (a *API) EventSourceHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := contextUserID(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported by this server", http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	types, err := parseEventTypes(q.Get("types"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest,
			newRequestError("invalidArguments", http.StatusBadRequest, err.Error()))
		return
	}
	closeAfterState := q.Get("closeafter") == "state"
	ping, err := parsePing(q.Get("ping"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest,
			newRequestError("invalidArguments", http.StatusBadRequest, err.Error()))
		return
	}

	// Bound the connections BEFORE writing a 200: a client that is refused must
	// be refused with a status it can act on.
	if a.eventConns.Add(1) > maxEventSourceConns {
		a.eventConns.Add(-1)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable,
			newRequestError("limit", http.StatusServiceUnavailable,
				"too many open event streams"))
		return
	}
	defer a.eventConns.Add(-1)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Defeats response buffering in a reverse proxy (nginx and friends), which
	// would otherwise hold every event until the connection closed — i.e. forever.
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// The opening event carries the CURRENT state of every subscribed type, so a
	// client can align its cursors the moment it connects (and so `closeafter=state`
	// is a useful one-shot "what is the state now?").
	current, err := a.changeStates(r.Context(), userID, types)
	if err != nil {
		return // the connection is already 200; nothing useful to say. It closes.
	}
	if !writeStateChange(w, flusher, userID, current) {
		return
	}
	if closeAfterState {
		return
	}

	tick := time.NewTicker(eventSourceInterval)
	defer tick.Stop()
	pingC := noPing
	if ping > 0 {
		pinger := time.NewTicker(ping)
		defer pinger.Stop()
		pingC = pinger.C
	}

	// The session re-check (ADR-048 section 6): a stream outlives the request
	// that opened it, so a device signed out meanwhile must not keep receiving.
	// Only a request with a session has one to check (dev-auth does not).
	// The check stamps nothing and never renews: a stream is not USE, or a
	// forgotten tab would keep a session alive for ever.
	sess, hasSession := auth.SessionFromContext(r.Context())
	recheckC := noPing
	if hasSession && a.sessionLive != nil {
		interval := a.streamRecheck
		if interval <= 0 {
			interval = streamRecheckInterval
		}
		rechecker := time.NewTicker(interval)
		defer rechecker.Stop()
		recheckC = rechecker.C
	}
	recheckFailed := false

	for {
		select {
		case <-r.Context().Done():
			return // the client went away
		case <-recheckC:
			live, err := a.sessionLive(r.Context(), sess.ID, userID)
			if err != nil {
				// Keep the stream: a database blip must not drop everyone's push
				// channel. Said once per stream, not every 30 seconds.
				if !recheckFailed && r.Context().Err() == nil {
					recheckFailed = true
					slog.WarnContext(r.Context(), "event stream: could not re-check the session; keeping the stream", "err", err)
				}
				continue
			}
			if !live {
				return // the client reconnects and is refused with a 401
			}
		case <-a.baseCtx.Done():
			return // the server is shutting down
		case <-pingC:
			if !writeEvent(w, flusher, "ping", "",
				fmt.Sprintf(`{"interval":%d}`, int(ping.Seconds()))) {
				return
			}
		case <-tick.C:
			next, err := a.changeStates(r.Context(), userID, types)
			if err != nil {
				// A transient database failure must not kill a client's push
				// channel; the next tick tries again. (A permanent one shows up as
				// the client's own Foo/changes failing, which it already handles.)
				continue
			}
			changed := diffStates(current, next)
			if len(changed) == 0 {
				continue
			}
			current = next
			if !writeStateChange(w, flusher, userID, changed) {
				return
			}
			if closeAfterState {
				return
			}
		}
	}
}

// noPing is a nil channel: it never fires, so `ping=0` (RFC 8620's "no pings")
// costs no timer at all.
var noPing <-chan time.Time

// changeStates reads the current change sequence of each subscribed type, in ONE
// transaction, so the states in a single StateChange event are mutually
// consistent (data-model §3.1) rather than smeared across several reads.
func (a *API) changeStates(ctx context.Context, userID ulid.ULID, types []store.ObjectType) (map[store.ObjectType]string, error) {
	out := make(map[store.ObjectType]string, len(types))
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		for _, t := range types {
			seq, err := tx.ChangeState(ctx, t)
			if err != nil {
				return err
			}
			out[t] = encodeState(seq)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// diffStates returns the types whose state moved.
func diffStates(prev, next map[store.ObjectType]string) map[store.ObjectType]string {
	changed := map[store.ObjectType]string{}
	for t, s := range next {
		if prev[t] != s {
			changed[t] = s
		}
	}
	return changed
}

// writeStateChange emits one RFC 8620 §7.3 StateChange event. The event id is
// the state itself, so a reconnecting client's Last-Event-ID is meaningful to a
// human reading a log (the client's own recovery is Foo/changes, which needs no
// replay from us).
func writeStateChange(w http.ResponseWriter, f http.Flusher, userID ulid.ULID, states map[store.ObjectType]string) bool {
	typed := make(map[string]string, len(states))
	for t, s := range states {
		typed[string(t)] = s
	}
	payload, err := json.Marshal(map[string]any{
		"@type":   "StateChange",
		"changed": map[string]any{userID.String(): typed},
	})
	if err != nil {
		return false
	}
	return writeEvent(w, f, "state", eventID(states), string(payload))
}

// eventID renders a stable id for a StateChange (the sorted type:state pairs).
func eventID(states map[store.ObjectType]string) string {
	parts := make([]string, 0, len(states))
	for _, t := range eventSourceTypes { // a fixed order: ids must be stable
		if s, ok := states[t]; ok {
			parts = append(parts, string(t)+":"+s)
		}
	}
	return strings.Join(parts, ",")
}

// writeEvent writes one SSE event and flushes it. A write failure means the
// client is gone: the caller unwinds the handler (and with it the connection).
func writeEvent(w http.ResponseWriter, f http.Flusher, event, id, data string) bool {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + event + "\n")
	// SSE frames data line by line; our payloads are single-line JSON, but a
	// multi-line one must not be able to break the framing.
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return false
	}
	f.Flush()
	return true
}

// parseEventTypes reads the `types` parameter: "*" (or empty) means every type,
// otherwise a comma-separated list. An unknown type is an ERROR, not a silent
// omission — a client that subscribed to a type this server does not push must
// find out now, not by waiting forever for an event.
func parseEventTypes(param string) ([]store.ObjectType, error) {
	if param == "" || param == "*" {
		return eventSourceTypes, nil
	}
	known := make(map[string]store.ObjectType, len(eventSourceTypes))
	for _, t := range eventSourceTypes {
		known[string(t)] = t
	}
	seen := map[store.ObjectType]bool{}
	var out []store.ObjectType
	for _, name := range strings.Split(param, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("this server does not push changes for %q", name)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return eventSourceTypes, nil
	}
	return out, nil
}

// parsePing reads the `ping` parameter (seconds; 0 = no ping), clamped to a
// server minimum.
func parsePing(param string) (time.Duration, error) {
	if param == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(param)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("ping must be a non-negative number of seconds")
	}
	if n == 0 {
		return 0, nil
	}
	d := time.Duration(n) * time.Second
	if d < minPingInterval {
		d = minPingInterval
	}
	return d, nil
}
