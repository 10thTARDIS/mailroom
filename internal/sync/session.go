// SPDX-License-Identifier: AGPL-3.0-only

package sync

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailroom/internal/egress"
)

// notificationBufferSize bounds SyncSession.Notifications. It is a hint
// channel, not a delivery-guaranteed one — see notify.
const notificationBufferSize = 64

// SyncSession is the anti-corruption seam over go-imap v2's imapclient
// package (spec §3/§4). No go-imap type appears in this interface, or
// anywhere else exported from this package: callers only ever see
// mailroom's own Mailbox/MailboxStatus/Message/Notification/Error types.
// This keeps go-imap's pre-stable API churn from rippling into the
// single-account sync engine (#10) or anything downstream of it.
type SyncSession interface {
	// ListMailboxes returns every mailbox visible to the account, including
	// RFC 6154 SPECIAL-USE attributes when the server supports SPECIAL-USE.
	//
	// When the server supports LIST-STATUS (RFC 5819) or IMAP4rev2, the same
	// LIST also asks for each mailbox's UIDVALIDITY, UIDNEXT, MESSAGES and —
	// with CONDSTORE — HIGHESTMODSEQ, and returns them in Mailbox.Status: one
	// round trip that says which of an account's folders changed at all
	// (#620). Without that support the LIST is exactly what it always was and
	// every Status is nil.
	ListMailboxes(ctx context.Context) ([]Mailbox, error)

	// Select opens a mailbox (SELECT, or EXAMINE when ReadOnly is set) and
	// returns its current status. Set opts.CondStore only after confirming
	// Supports(CapabilityCondStore).
	Select(ctx context.Context, mailbox string, opts SelectOptions) (*MailboxStatus, error)

	// FetchByUID streams the requested items for every message identified by
	// uids, calling fn once per message AS IT ARRIVES. uids must not be empty.
	// An error from fn aborts the fetch and is returned.
	//
	// ⚠ IT STREAMS BECAUSE THE ALTERNATIVE COULD NOT BE MADE SAFE (#351).
	// This returned []Message and buffered the whole response. A first sync —
	// and every UIDVALIDITY roll, which rewinds the cursor to 0 — asks for the
	// entire mailbox WITH BODIES, so peak memory was ~2x the mailbox's raw
	// bytes: tens of GB on the archives spec §18 sizes for, against the 16-32 GB
	// it budgets. The obvious fix, fetching in bounded UID ranges, is unavailable
	// precisely where it is needed: a rolled mailbox must be re-fetched WHOLE,
	// because ReconcileRetiredGeneration soft-deletes anything the re-fetch did
	// not return (internal/mailsync/adapter.go). Bounding that fetch would delete
	// mail that had merely not arrived yet.
	//
	// Streaming resolves the conflict instead of trading one defect for another:
	// the same UIDs are requested, so the completeness proof is untouched, while
	// peak memory falls to a single message.
	FetchByUID(ctx context.Context, uids UIDSet, items FetchItems, fn func(Message) error) error

	// LiveUIDs EXAMINEs mailbox and returns, from that one SELECT, its
	// status together with every UID the server currently holds in it
	// (UID SEARCH ALL, RFC 9051 §6.4.4). It is how the expunge reaper
	// (internal/mailsync) learns which messages VANISHED: CONDSTORE has no
	// vanished list and QRESYNC is not drivable through this client library
	// (NOTES.md), so the only reliable answer is to ask the server for the
	// whole UID set and diff it. UIDs are 4 bytes, so even a six-figure
	// mailbox answers in a few hundred kilobytes.
	//
	// The status and the UID set are returned TOGETHER, deliberately, and
	// the caller must treat them as one observation:
	//
	//   - UIDVALIDITY names the generation the UIDs belong to. A UID set is
	//     meaningless against provenance rows keyed by a different
	//     generation, so the caller must check it before diffing.
	//   - UIDNEXT bounds the UID space this observation actually covers.
	//     Anything at or above it did not exist when the server answered,
	//     and must not be concluded absent.
	//   - NumMessages (EXISTS) cross-checks the answer: an empty UID set
	//     from a mailbox the server just said holds messages is a broken
	//     answer, not an emptied mailbox.
	//
	// An empty set with a nil error means the server said the mailbox is
	// empty. An error means we FAILED TO ASK — never that anything is gone.
	LiveUIDs(ctx context.Context, mailbox string) (*MailboxStatus, []UID, error)

	// StoreFlags issues a UID STORE that adds and/or removes flags/keywords
	// on the message identified by uid in mailbox (RFC 9051 §6.4.6),
	// implemented as two STORE commands (+FLAGS.SILENT / -FLAGS.SILENT) when
	// both add and remove are non-empty, one otherwise. It is a no-op when
	// both are empty. mailbox is SELECTed read-write first if it is not
	// already this session's current write-selected mailbox (see the
	// package doc on writeSelected in session.go for why writes never trust
	// a prior read-only EXAMINE).
	StoreFlags(ctx context.Context, mailbox string, uid UID, add, remove []Flag) error

	// Copy copies every message in uids from mailbox to dest in one command
	// (UID COPY, RFC 9051 §6.4.8). uids must not be empty.
	Copy(ctx context.Context, mailbox string, uids UIDSet, dest string) error

	// Move moves every message in uids from mailbox to dest in one command
	// (UID MOVE, RFC 6851) when the server advertises
	// Supports(CapabilityMove). Otherwise go-imap's client library falls
	// back internally to UID COPY + UID STORE +FLAGS.SILENT \Deleted +
	// (UID) EXPUNGE/EXPUNGE — see move.go in
	// github.com/emersion/go-imap/v2/imapclient, read directly out of the
	// module cache for this change. uids must not be empty.
	Move(ctx context.Context, mailbox string, uids UIDSet, dest string) error

	// Expunge permanently removes every message in uids from mailbox (UID
	// EXPUNGE, RFC 4315 UIDPLUS / RFC 9051 IMAP4rev2). When the server
	// supports neither, there is no protocol-level way to expunge a UID
	// subset, so Expunge falls back to a plain EXPUNGE, which removes every
	// \Deleted message in mailbox — on such servers the caller must ensure
	// uids is exactly the set of messages it marked \Deleted before calling
	// Expunge. uids must not be empty.
	//
	// It returns HOW MANY MESSAGES THE SERVER SAID IT REMOVED — one per
	// untagged EXPUNGE response — because a UID EXPUNGE naming a UID the
	// server no longer holds succeeds while removing nothing (#296). Without
	// that number, "I destroyed the message" and "there was nothing there"
	// are the same green, which is the failure mode #268 was about. A count
	// of 0 with a nil error is therefore a real and expected answer, not an
	// error; on the no-UIDPLUS fallback the count is every \Deleted message
	// the mailbox held, so treat it as "at least this much happened" rather
	// than as attributable per UID.
	Expunge(ctx context.Context, mailbox string, uids UIDSet) (int, error)

	// Append appends body (raw RFC 5322 message bytes) to mailbox with the
	// given flags and internalDate (RFC 9051 §6.3.11). A zero internalDate
	// lets the server assign one. APPEND does not require mailbox to be
	// selected. It returns the message's assigned UID when the server
	// advertises Supports(CapabilityUIDPlus) (RFC 4315) and actually returns
	// an APPENDUID response code; otherwise it returns UID(0) with a nil
	// error, and the caller must discover the UID some other way (e.g. a
	// follow-up FetchByUID/search) on such servers.
	Append(ctx context.Context, mailbox string, flags []Flag, internalDate time.Time, body []byte) (UID, error)

	// SearchMessageID returns the UIDs in mailbox whose Message-ID header
	// equals messageID (angle brackets optional; the server matches the
	// header field, RFC 9051 §6.4.4 SEARCH HEADER). An empty result means
	// the message is not in that mailbox.
	//
	// It exists for the Sent self-append dedup (spec §4 / ADR-001): before
	// APPENDing our copy of a message we just sent, ask the provider whether
	// it already filed one of its own. Message-ID is the right handle for
	// that question and a content hash is not — a provider that adds a
	// header on the way out (Gmail) produces a copy with the same
	// Message-ID and different bytes.
	SearchMessageID(ctx context.Context, mailbox string, messageID string) ([]UID, error)

	// UIDPresent reports whether uid is still present in mailbox — a bare UID
	// SEARCH for that single UID (RFC 9051 §6.4.4). It exists to answer the
	// one question a UID EXPUNGE's own removal count cannot when that count
	// is zero (#611): did the server genuinely have nothing to remove, or
	// does it still hold the message despite saying so? Not every provider
	// is strictly read-your-writes across a STORE \Deleted and the UID
	// EXPUNGE that follows it, so a caller that trusts a zero count alone
	// can retire an intent whose target is still on the server.
	//
	// A message flagged \Deleted but not yet expunged still matches a bare
	// UID SEARCH — the flag does not remove it from the mailbox, only
	// EXPUNGE does — which is exactly the "still present" case #611 needs
	// distinguished from "genuinely gone".
	UIDPresent(ctx context.Context, mailbox string, uid UID) (bool, error)

	// Supports reports whether the server advertised a capability. It never
	// performs I/O beyond what the connection has already negotiated.
	Supports(cap Capability) bool

	// Noop issues a NOOP (RFC 9051 §6.1.2) and waits for the server's tagged
	// response. It is the connection health probe: a nil return means the
	// connection is alive and usable, and any error means the connection is
	// dead or wedged and the caller must discard it and re-Dial. It exists so
	// a warm, reused session (syncrunner's per-account sync pool, mailroom#103
	// Stage 3) is validated before a pass rather than discovering the drop
	// halfway through one. Like every other op it is force-closed on ctx
	// cancellation, so a canceled Noop is itself connection-fatal.
	Noop(ctx context.Context) error

	// Notifications returns the channel unilateral server data (new
	// messages, expunges, flag/mailbox updates) is delivered on. It is
	// populated whenever the connection receives unsolicited data,
	// including but not limited to while an IdleSession is running. The
	// channel is bounded; under sustained backpressure notifications are
	// dropped rather than blocking the connection — callers that need a
	// complete view must still reconcile via CONDSTORE (spec §4) rather
	// than relying on push alone.
	Notifications() <-chan Notification

	// Idle starts an IMAP IDLE command so the server can push updates
	// instead of the client polling (RFC 2177 / RFC 9051). It blocks until
	// the server acknowledges IDLE.
	Idle(ctx context.Context) (IdleSession, error)

	// Logout sends LOGOUT and waits for the server's response.
	Logout(ctx context.Context) error

	// Close immediately closes the underlying connection. Safe to call
	// after Logout, or instead of it.
	Close() error
}

// IdleSession represents a running IMAP IDLE command.
type IdleSession interface {
	// Stop ends IDLE, unblocking the connection for other commands. It
	// does not wait for the server to acknowledge that; call Wait for
	// that.
	Stop() error
	// Wait blocks until the IDLE command completes, after Stop or on
	// connection error.
	Wait() error
}

// Dial connects and authenticates to an IMAP server (spec §4). ctx bounds
// how long the caller is willing to wait: on cancellation, Dial stops
// waiting and returns promptly, but because go-imap's Dial routines have no
// native context support, an in-flight TCP handshake or LOGIN is not
// aborted immediately — Dial closes the resulting connection in the
// background once it completes, so no socket is leaked, but the goroutine
// outlives the canceled call by however long the handshake takes.
func Dial(ctx context.Context, cfg Config) (SyncSession, error) {
	if err := cfg.Validate(); err != nil {
		return nil, &Error{Kind: ErrorKindConfig, Op: "dial", Err: err}
	}

	notifications := make(chan Notification, notificationBufferSize)
	options := &imapclient.Options{
		UnilateralDataHandler: unilateralHandler(notifications),
	}
	if cfg.TLS != TLSInsecure {
		options.TLSConfig = cfg.imapTLSConfig()
	}

	type dialResult struct {
		client *imapclient.Client
		// op is the step that failed, and so the Error.Op the failure is
		// classified under: "dial" for everything up to and including the
		// server's greeting (connect, TLS, STARTTLS, a refused or dropped
		// greeting), "login" for the LOGIN command itself. The distinction
		// carries meaning: classifyRefusal reads any non-OK refusal under
		// "login" as a credentials problem (#888), which is only true once
		// the server has greeted us.
		op  string
		err error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		client, err := dialClient(ctx, cfg, options)
		if err != nil {
			resultCh <- dialResult{op: "dial", err: err}
			return
		}
		// Settle the greeting under "dial" before LOGIN, so a server that
		// greets with NO/BYE (or drops the connection) is not mistaken for a
		// rejected password by the "login" classification (#888).
		if err := client.WaitGreeting(); err != nil {
			resultCh <- dialResult{client: client, op: "dial", err: err}
			return
		}
		// LOGIN for a password account, AUTHENTICATE XOAUTH2 when cfg.Auth is
		// set (ADR-046); both are the "login" step.
		err = cfg.authenticate(ctx, client)
		resultCh <- dialResult{client: client, op: "login", err: err}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			if res.client != nil {
				_ = res.client.Close() // best-effort: we're already returning res.err
			}
			return nil, newOpError(res.op, res.err)
		}
		return &session{client: res.client, notifications: notifications, searchLimiter: cfg.SearchLimiter}, nil
	case <-ctx.Done():
		go func() {
			res := <-resultCh
			if res.client != nil {
				_ = res.client.Close() // best-effort cleanup of a connection nobody is waiting on anymore
			}
		}()
		return nil, &Error{Kind: ErrorKindTransient, Op: "dial", Err: ctx.Err()}
	}
}

// dialClient performs the guarded connect and the TLS-mode-specific
// transport step. It does not log in.
//
// ⚠ THE CONNECTION IS MADE HERE, NOT BY imapclient's Dial* HELPERS, and that
// is the whole of #625 §3's IMAP half. Those helpers take a hostname and a
// *net.Dialer and resolve the name themselves at connect time, so the address
// actually connected to was one nothing had ever checked: an account whose host
// resolved public when it was validated could resolve to loopback or the cloud
// metadata service on any later sync pass, and LOGIN would hand it the
// password. Instead, egress.Dialer resolves ONCE, refuses unless every address
// passes the egress policy (unless AllowPrivateHosts), and dials the checked IP
// literal; the result is wrapped here exactly as the helpers would have wrapped
// it (implicit TLS with the "imap" ALPN, STARTTLS, or plaintext).
//
// TLS verifies against the HOSTNAME: options.TLSConfig.ServerName is
// cfg.tlsServerName() (ServerName, else Host), never the literal dialled.
func dialClient(ctx context.Context, cfg Config, options *imapclient.Options) (*imapclient.Client, error) {
	switch cfg.TLS {
	case TLSImplicit, TLSStartTLS, TLSInsecure:
	default:
		return nil, fmt.Errorf("sync: unsupported TLS mode %v", cfg.TLS)
	}

	// One budget for resolve + connect + (implicit) handshake: the span
	// tls.DialWithDialer's Timeout covered when imapclient.DialTLS did this.
	dctx, cancel := context.WithTimeout(ctx, cfg.dialTimeout())
	defer cancel()

	d := &egress.Dialer{AllowPrivate: cfg.AllowPrivateHosts, Resolver: cfg.Resolver}
	conn, err := d.DialContext(dctx, "tcp", cfg.addr())
	if err != nil {
		if errors.Is(err, egress.ErrBlockedAddress) {
			// Names the host and the refused address (both in err) and the way
			// out. The credentials are not in scope here and cannot leak.
			return nil, fmt.Errorf("imap host %q is not a permitted connection target: %w. If this "+
				"instance must reach a mail server on a private network, set "+
				"MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS=1", cfg.Host, err)
		}
		return nil, err
	}

	switch cfg.TLS {
	case TLSImplicit:
		tlsConfig := options.TLSConfig.Clone()
		if tlsConfig.NextProtos == nil {
			tlsConfig.NextProtos = []string{"imap"} // as imapclient.DialTLS sets it
		}
		tconn := tls.Client(conn, tlsConfig)
		if err := tconn.HandshakeContext(dctx); err != nil {
			_ = conn.Close() // best-effort: we're already returning err
			return nil, err
		}
		return imapclient.New(tconn, options), nil
	case TLSStartTLS:
		// NewStartTLS closes conn itself on failure. options.TLSConfig already
		// carries ServerName, which imapclient.DialStartTLS would otherwise have
		// filled in from the (hostname) address.
		return imapclient.NewStartTLS(conn, options)
	default: // TLSInsecure
		return imapclient.New(conn, options), nil
	}
}

func (c Config) imapTLSConfig() *tls.Config {
	return &tls.Config{
		ServerName:         c.tlsServerName(),
		InsecureSkipVerify: c.InsecureSkipVerify,
		RootCAs:            c.rootCAs,
	}
}

// unilateralHandler builds the go-imap unilateral-data handler that
// forwards updates onto our Notification channel.
func unilateralHandler(notifications chan<- Notification) *imapclient.UnilateralDataHandler {
	return &imapclient.UnilateralDataHandler{
		Expunge: func(seqNum uint32) {
			notify(notifications, Notification{Kind: NotificationExpunge, SeqNum: seqNum})
		},
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				notify(notifications, Notification{Kind: NotificationExists, NumMessages: data.NumMessages})
			}
			if data.Flags != nil || data.PermanentFlags != nil {
				notify(notifications, Notification{Kind: NotificationFlags})
			}
		},
	}
}

// notify delivers n without blocking. The go-imap docs warn that the
// unilateral data handler runs on the connection's read goroutine and
// blocks it while running, so a full channel drops the notification rather
// than stalling the connection. This is safe because Notification is a
// hint, not a delivery-guaranteed event: the periodic reconcile (spec §4)
// is the documented backstop for anything missed here.
func notify(ch chan<- Notification, n Notification) {
	select {
	case ch <- n:
	default:
	}
}

// session is the SyncSession implementation wrapping *imapclient.Client.
//
// Not safe for concurrent use by multiple goroutines: like the underlying
// *imapclient.Client, one session drives one IMAP connection, and every
// exported method here already blocks its caller until the command
// completes (see withContext), so callers needing concurrency use one
// session per goroutine (see TestLiveConcurrentSessions), not one session
// shared across goroutines.
type session struct {
	client        *imapclient.Client
	notifications chan Notification

	// writeSelected is the mailbox name this session last SELECTed
	// read-write (as opposed to EXAMINEd, or not selected at all), or "" if
	// unknown/not-writable. Write ops (StoreFlags/Copy/Move/Expunge) target
	// "the selected mailbox" implicitly per RFC 9051, and the read-sync path
	// may have EXAMINEd a different mailbox (or none) most recently, so
	// writes consult this before trusting the connection's selected state;
	// see ensureWritableSelect.
	writeSelected string

	// keywordProbes is what every READ-WRITE SELECT this session issued said
	// about new keywords, by mailbox: PERMANENTFLAGS contained `\*` (#740).
	// Only a read-write SELECT can answer that question — a server answers
	// EXAMINE with an empty PERMANENTFLAGS, because nothing is permanent in a
	// read-only session (RFC 9051 §6.3.3) — so the sync pass, which EXAMINEs,
	// cannot, and the writeback session, which already SELECTs read-write to do
	// its job, records it on the way past. Read by WritableKeywordProbes.
	keywordProbes map[string]bool

	// searchLimiter paces this session's UID SEARCHes (Config.SearchLimiter).
	// Nil means unpaced — the pre-#638 behaviour, and still what tests and
	// servers without a SEARCH ceiling get.
	searchLimiter Limiter
}

// awaitSearchSlot blocks until this session may issue another UID SEARCH.
//
// It is called from INSIDE each UIDSearch op's closure, immediately before the
// command, rather than at the top of the method: the token pays for the SEARCH,
// so a SELECT that fails first must not spend one. Nil limiter = no pacing and
// no cost. A cancelled wait returns ctx's error, which withContext classifies as
// transient like any other cancellation — the search is NOT issued.
func (s *session) awaitSearchSlot(ctx context.Context) error {
	if s.searchLimiter == nil {
		return nil
	}
	return s.searchLimiter.Wait(ctx)
}

var _ SyncSession = (*session)(nil)

func (s *session) ListMailboxes(ctx context.Context) ([]Mailbox, error) {
	var mailboxes []Mailbox
	opts := &imap.ListOptions{ReturnSpecialUse: true, ReturnStatus: s.listStatusOptions()}
	err := s.withContext(ctx, "list", func() error {
		data, err := s.client.List("", "*", opts).Collect()
		if err != nil {
			return err
		}
		mailboxes = make([]Mailbox, 0, len(data))
		for _, d := range data {
			mailboxes = append(mailboxes, mapListData(d))
		}
		return nil
	})
	return mailboxes, err
}

// listStatusOptions is the STATUS request ListMailboxes attaches to its LIST
// (#620), or nil — no request at all, today's exact LIST — when the server
// supports neither LIST-STATUS (RFC 5819) nor IMAP4rev2 (which folds it in;
// go-imap's CapSet.Has answers for both). HIGHESTMODSEQ is only asked for when
// CONDSTORE is advertised, because STATUS HIGHESTMODSEQ is a CONDSTORE item
// (RFC 7162 §3.1.8) and a server without it may reject the whole LIST.
func (s *session) listStatusOptions() *imap.StatusOptions {
	if !s.client.Caps().Has(imap.CapListStatus) {
		return nil
	}
	return &imap.StatusOptions{
		UIDValidity:   true,
		UIDNext:       true,
		NumMessages:   true,
		HighestModSeq: s.Supports(CapabilityCondStore),
	}
}

func (s *session) Select(ctx context.Context, mailbox string, opts SelectOptions) (*MailboxStatus, error) {
	var status *MailboxStatus
	err := s.withContext(ctx, "select", func() error {
		data, err := s.client.Select(mailbox, &imap.SelectOptions{
			ReadOnly:  opts.ReadOnly,
			CondStore: opts.CondStore,
		}).Wait()
		if err != nil {
			return err
		}
		status = mapSelectData(mailbox, data)
		// Keep writeSelected in sync regardless of entry point: a plain
		// (non-ReadOnly) Select here is just as writable as one issued by
		// ensureWritableSelect, and an EXAMINE invalidates whatever write
		// selection there was.
		if opts.ReadOnly {
			s.writeSelected = ""
		} else {
			s.writeSelected = mailbox
			s.noteKeywordProbe(mailbox, status.PermanentFlagsAllowNew)
		}
		return nil
	})
	return status, err
}

// ensureWritableSelect issues a read-write SELECT of mailbox unless it is
// already this session's current write-selected mailbox (see the session
// struct doc). It must be called from within an active withContext closure
// — it performs raw client calls and does not itself manage cancellation.
func (s *session) ensureWritableSelect(mailbox string) error {
	if s.writeSelected == mailbox {
		return nil
	}
	data, err := s.client.Select(mailbox, &imap.SelectOptions{}).Wait()
	if err != nil {
		return err
	}
	s.writeSelected = mailbox
	s.noteKeywordProbe(mailbox, mapSelectData(mailbox, data).PermanentFlagsAllowNew)
	return nil
}

// noteKeywordProbe records one read-write SELECT's answer for mailbox. It is
// called ONLY from read-write SELECTs; an EXAMINE's empty PERMANENTFLAGS says
// nothing about the mailbox and must never reach the record (#740).
func (s *session) noteKeywordProbe(mailbox string, acceptsNew bool) {
	if s.keywordProbes == nil {
		s.keywordProbes = map[string]bool{}
	}
	s.keywordProbes[mailbox] = acceptsNew
}

// WritableKeywordProbes returns, for every mailbox this session has SELECTed
// read-write, whether that SELECT's PERMANENTFLAGS admitted new keywords (`\*`).
// A copy: the caller may keep it after the session closes. Mailboxes only ever
// EXAMINEd are absent — not false — because EXAMINE cannot answer (#740).
func (s *session) WritableKeywordProbes() map[string]bool {
	out := make(map[string]bool, len(s.keywordProbes))
	for k, v := range s.keywordProbes {
		out[k] = v
	}
	return out
}

func (s *session) StoreFlags(ctx context.Context, mailbox string, uid UID, add, remove []Flag) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	return s.withContext(ctx, "store", func() error {
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		numSet := uidSetToIMAP(UIDSetNum(uid))
		if len(add) > 0 {
			if err := s.client.Store(numSet, &imap.StoreFlags{
				Op:     imap.StoreFlagsAdd,
				Silent: true,
				Flags:  flagsToIMAP(add),
			}, nil).Close(); err != nil {
				return err
			}
		}
		if len(remove) > 0 {
			if err := s.client.Store(numSet, &imap.StoreFlags{
				Op:     imap.StoreFlagsDel,
				Silent: true,
				Flags:  flagsToIMAP(remove),
			}, nil).Close(); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *session) Copy(ctx context.Context, mailbox string, uids UIDSet, dest string) error {
	if uids.IsEmpty() {
		return &Error{Kind: ErrorKindProtocol, Op: "copy", Err: fmt.Errorf("sync: copy: empty UID set")}
	}
	return s.withContext(ctx, "copy", func() error {
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		_, err := s.client.Copy(uidSetToIMAP(uids), dest).Wait()
		return err
	})
}

func (s *session) Move(ctx context.Context, mailbox string, uids UIDSet, dest string) error {
	if uids.IsEmpty() {
		return &Error{Kind: ErrorKindProtocol, Op: "move", Err: fmt.Errorf("sync: move: empty UID set")}
	}
	return s.withContext(ctx, "move", func() error {
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		// c.Move already falls back to COPY+STORE+EXPUNGE internally when
		// the server lacks MOVE (RFC 6851) — see the SyncSession.Move doc.
		_, err := s.client.Move(uidSetToIMAP(uids), dest).Wait()
		return err
	})
}

func (s *session) Expunge(ctx context.Context, mailbox string, uids UIDSet) (int, error) {
	if uids.IsEmpty() {
		return 0, &Error{Kind: ErrorKindProtocol, Op: "expunge", Err: fmt.Errorf("sync: expunge: empty UID set")}
	}
	var removed int
	err := s.withContext(ctx, "expunge", func() error {
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		var cmd *imapclient.ExpungeCommand
		if s.Supports(CapabilityUIDPlus) {
			cmd = s.client.UIDExpunge(uidSetToIMAP(uids))
		} else {
			// No UIDPLUS/IMAP4rev2: no protocol form of "expunge exactly these
			// UIDs" exists, so fall back to a plain EXPUNGE — see the
			// SyncSession.Expunge doc for the caller obligation this implies.
			cmd = s.client.Expunge()
		}
		// Collect() rather than Close(): both drain the command, but Collect
		// hands back the sequence numbers the server reported as expunged, and
		// that count is the ONLY evidence available that mail was actually
		// destroyed (#296 — see the SyncSession.Expunge doc). go-imap routes
		// untagged EXPUNGE responses to a PENDING ExpungeCommand in preference
		// to the unilateral handler (imapclient.Client.handleExpunge), so this
		// really does see them; QRESYNC/VANISHED cannot interfere because the
		// library has no way to drive QRESYNC at all (see this package's doc).
		// Verified against a live Dovecot, both ways round, in
		// TestLiveExpungeMessage: a real destruction reports 1, and expunging
		// the same UID again reports 0 with no error.
		seqNums, err := cmd.Collect()
		removed = len(seqNums)
		return err
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func (s *session) Append(ctx context.Context, mailbox string, flags []Flag, internalDate time.Time, body []byte) (UID, error) {
	var uid UID
	err := s.withContext(ctx, "append", func() error {
		cmd := s.client.Append(mailbox, int64(len(body)), &imap.AppendOptions{
			Flags: flagsToIMAP(flags),
			Time:  internalDate,
		})
		if _, err := cmd.Write(body); err != nil {
			_ = cmd.Close() // best-effort: we're already returning err
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		data, err := cmd.Wait()
		if err != nil {
			return err
		}
		// data.UID is the zero value when the server didn't return an
		// APPENDUID response code (no UIDPLUS support) — see the
		// SyncSession.Append doc.
		uid = UID(data.UID)
		return nil
	})
	return uid, err
}

func (s *session) SearchMessageID(ctx context.Context, mailbox string, messageID string) ([]UID, error) {
	id := strings.Trim(strings.TrimSpace(messageID), "<>")
	if id == "" {
		return nil, &Error{Kind: ErrorKindProtocol, Op: "search",
			Err: fmt.Errorf("sync: search: empty message id")}
	}
	var uids []UID
	err := s.withContext(ctx, "search", func() error {
		// SEARCH runs against the selected mailbox. EXAMINE (read-only) would
		// do, but the write-selected bookkeeping only tracks read-write
		// selections; selecting read-write here keeps a subsequent APPEND/STORE
		// on the same session from having to re-SELECT.
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		// Writeback spends the SAME per-account SEARCH budget as the
		// observation pass (#638) — pace it or a drain landing inside an
		// observation burst is refused.
		if err := s.awaitSearchSlot(ctx); err != nil {
			return err
		}
		data, err := s.client.UIDSearch(&imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{
				{Key: "Message-Id", Value: id},
			},
		}, nil).Wait()
		if err != nil {
			return err
		}
		for _, u := range data.AllUIDs() {
			uids = append(uids, UID(u))
		}
		return nil
	})
	return uids, err
}

func (s *session) UIDPresent(ctx context.Context, mailbox string, uid UID) (bool, error) {
	var present bool
	err := s.withContext(ctx, "uidpresent", func() error {
		// Same write-selection bookkeeping as SearchMessageID, for the same
		// reason: an EXAMINE would answer this SEARCH fine, but it would
		// invalidate the write-selected state a following STORE/EXPUNGE on
		// this session relies on (see ensureWritableSelect).
		if err := s.ensureWritableSelect(mailbox); err != nil {
			return err
		}
		// Same per-account SEARCH budget as the observation pass (#638).
		if err := s.awaitSearchSlot(ctx); err != nil {
			return err
		}
		data, err := s.client.UIDSearch(&imap.SearchCriteria{
			UID: []imap.UIDSet{uidSetToIMAP(UIDSetNum(uid))},
		}, nil).Wait()
		if err != nil {
			return err
		}
		for _, u := range data.AllUIDs() {
			if UID(u) == uid {
				present = true
				break
			}
		}
		return nil
	})
	return present, err
}

func (s *session) FetchByUID(ctx context.Context, uids UIDSet, items FetchItems, fn func(Message) error) error {
	if uids.IsEmpty() {
		return &Error{Kind: ErrorKindProtocol, Op: "fetch", Err: fmt.Errorf("sync: fetch: empty UID set")}
	}

	fetchOpts := &imap.FetchOptions{
		Flags:        items.Flags,
		Envelope:     items.Envelope,
		InternalDate: items.InternalDate,
		RFC822Size:   items.RFC822Size,
		UID:          true,
		ModSeq:       items.ModSeq,
		ChangedSince: items.ChangedSince,
		// Gmail's X-GM-EXT-1 items (#52, ADR-047), parsed by the vendored
		// go-imap patch (third_party/go-imap/MAILROOM-PATCHES.md).
		GmailMsgID:  items.GmailExt,
		GmailThrID:  items.GmailExt,
		GmailLabels: items.GmailExt,
	}
	if items.BodyFull {
		// BODY.PEEK[], NEVER BODY[]. The non-peek form IMPLICITLY SETS \Seen on
		// the server (RFC 9051 §6.4.5) — so fetching a message to sync it would
		// mark it read, for us AND for every other client on the account, and
		// the next flag reconcile would faithfully read that back as the truth.
		// Syncing observes; it must not mutate the mailbox it is observing.
		// TestLiveFetchBodyDoesNotMarkSeen is the regression guard (#129).
		fetchOpts.BodySection = []*imap.FetchItemBodySection{{Peek: true}}
	}

	// ⚠ Next()/Close() rather than Collect(). Collect() materialises every
	// response before the caller sees any of them; this hands each message on as
	// it arrives, so peak memory is one message however large the range is. The
	// per-message Collect() below buffers only that message's own literals.
	return s.withContext(ctx, "fetch", func() error {
		cmd := s.client.Fetch(uidSetToIMAP(uids), fetchOpts)
		for {
			md := cmd.Next()
			if md == nil {
				break
			}
			buf, err := md.Collect()
			if err != nil {
				// Close() drains the rest of the command so the connection is
				// left usable; the collect error is the one worth reporting.
				_ = cmd.Close()
				return err
			}
			if err := fn(mapFetchBuffer(buf)); err != nil {
				_ = cmd.Close()
				return err
			}
		}
		return cmd.Close()
	})
}

// LiveUIDs EXAMINEs mailbox and issues UID SEARCH ALL against it, returning
// the SELECT's status and the resulting UID set as one observation. See the
// SyncSession.LiveUIDs doc for the contract this upholds.
//
// The SELECT is unconditional rather than "only if not already selected": the
// status it returns (UIDVALIDITY, UIDNEXT, EXISTS) is what BOUNDS the reap, and
// bounding a destructive diff with a status read at some earlier, unknown point
// in the session is precisely the mistake that deletes mail. One extra
// round-trip an hour is not a cost worth optimizing against that.
//
// It is EXAMINE (read-only), so it also clears writeSelected — the reaper must
// never leave a mailbox write-selected behind it.
func (s *session) LiveUIDs(ctx context.Context, mailbox string) (*MailboxStatus, []UID, error) {
	var (
		status *MailboxStatus
		uids   []UID
	)
	err := s.withContext(ctx, "liveuids", func() error {
		// EXAMINE (CONDSTORE) when the server has it, so the status carries
		// HIGHESTMODSEQ (#620). The engine records this observation's
		// (UIDVALIDITY, UIDNEXT, MESSAGES, HIGHESTMODSEQ) as the baseline a later
		// pass compares LIST-STATUS against to decide the observation can be
		// skipped; without the modseq that baseline is incomplete and the skip
		// never fires. RFC 7162 does not oblige a server to send HIGHESTMODSEQ on
		// a plain EXAMINE before CONDSTORE has been enabled on the connection, and
		// a pass whose polls were all skipped enables it nowhere else.
		data, err := s.client.Select(mailbox, &imap.SelectOptions{
			ReadOnly:  true,
			CondStore: s.Supports(CapabilityCondStore),
		}).Wait()
		if err != nil {
			return err
		}
		s.writeSelected = ""
		status = mapSelectData(mailbox, data)

		// THE COMMAND THE SERVER COUNTS (#638). One per due mailbox, and a
		// account with hundreds of mailboxes tried to issue all of them as fast as round trips
		// allowed; the limiter is what spaces them under the server's ceiling.
		// It is AFTER the SELECT on purpose — a SELECT that fails spends nothing.
		if err := s.awaitSearchSlot(ctx); err != nil {
			return err
		}

		// An empty SearchCriteria encodes as the IMAP key "ALL" (go-imap's
		// writeSearchKey falls back to it when no criterion was written), so this
		// is exactly `UID SEARCH ALL` against the mailbox just EXAMINEd.
		res, err := s.client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
		if err != nil {
			return err
		}
		found := res.AllUIDs()
		uids = make([]UID, 0, len(found))
		for _, u := range found {
			uids = append(uids, UID(u))
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return status, uids, nil
}

func (s *session) Supports(c Capability) bool {
	return s.client.Caps().Has(imap.Cap(c))
}

func (s *session) Noop(ctx context.Context) error {
	return s.withContext(ctx, "noop", func() error {
		return s.client.Noop().Wait()
	})
}

func (s *session) Notifications() <-chan Notification {
	return s.notifications
}

func (s *session) Idle(ctx context.Context) (IdleSession, error) {
	var cmd *imapclient.IdleCommand
	err := s.withContext(ctx, "idle", func() error {
		var err error
		cmd, err = s.client.Idle()
		return err
	})
	if err != nil {
		return nil, err
	}
	return &idleSession{cmd: cmd}, nil
}

func (s *session) Logout(ctx context.Context) error {
	return s.withContext(ctx, "logout", func() error {
		return s.client.Logout().Wait()
	})
}

func (s *session) Close() error {
	if err := s.client.Close(); err != nil {
		return newOpError("close", err)
	}
	return nil
}

// withContext runs fn in a goroutine and waits for either it to finish or
// ctx to be done. go-imap's Client has no native per-command cancellation,
// so on cancellation we force-close the connection to unblock fn; that ends
// the whole session, not just the one command — callers should treat a
// canceled op as connection-fatal and re-Dial.
func (s *session) withContext(ctx context.Context, op string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return &Error{Kind: ErrorKindTransient, Op: op, Err: err}
	}

	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		if err != nil {
			return newOpError(op, err)
		}
		return nil
	case <-ctx.Done():
		_ = s.client.Close()
		<-done
		return &Error{Kind: ErrorKindTransient, Op: op, Err: ctx.Err()}
	}
}

// idleSession wraps *imapclient.IdleCommand.
type idleSession struct {
	cmd *imapclient.IdleCommand
}

var _ IdleSession = (*idleSession)(nil)

func (i *idleSession) Stop() error {
	if err := i.cmd.Close(); err != nil {
		return newOpError("idle", err)
	}
	return nil
}

func (i *idleSession) Wait() error {
	if err := i.cmd.Wait(); err != nil {
		return newOpError("idle", err)
	}
	return nil
}
