// SPDX-License-Identifier: AGPL-3.0-only

package census

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// imap.go is the only file in this package that imports go-imap — the
// anti-corruption boundary (CLAUDE.md): every conversion from go-imap's
// types to census's own (Mailbox/Examined/Meta/Part) happens here, and
// nowhere else in this package sees a go-imap type.

// TLSMode selects how Dial establishes transport security. Mirrors
// internal/sync.TLSMode's naming and meaning; this package intentionally
// does not import internal/sync (that package's SyncSession has write
// methods, which is exactly what census.Reader must not have — see
// reader.go), so the enum is restated rather than shared.
type TLSMode int

const (
	// TLSImplicit dials with TLS from the first byte (IMAPS, typically port
	// 993). The default, and the expected mode for a real account.
	TLSImplicit TLSMode = iota
	// TLSStartTLS dials in plaintext (typically port 143) and upgrades via
	// STARTTLS before authenticating.
	TLSStartTLS
	// TLSInsecure never negotiates TLS. Only appropriate against a
	// local/test IMAP server (the Dovecot harness, scripts/sync-test.sh) —
	// never use this against a real account.
	TLSInsecure
)

// String implements fmt.Stringer.
func (m TLSMode) String() string {
	switch m {
	case TLSImplicit:
		return "implicit"
	case TLSStartTLS:
		return "starttls"
	case TLSInsecure:
		return "insecure"
	default:
		return fmt.Sprintf("TLSMode(%d)", int(m))
	}
}

// defaultDialTimeout bounds the TCP connect phase. Not exposed on Config
// (which the task deliberately keeps to five fields); callers that need a
// different bound should give Dial's ctx a deadline instead.
const defaultDialTimeout = 30 * time.Second

// Config holds connection parameters for a single read-only census run.
// census's own type — go-imap never appears here.
type Config struct {
	Host     string
	Port     int
	TLS      TLSMode
	Username string
	Password string
}

// validate checks cfg is usable, without performing any I/O.
func (c Config) validate() error {
	if c.Host == "" {
		return fmt.Errorf("census: config: host is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("census: config: port %d out of range", c.Port)
	}
	if c.Username == "" {
		return fmt.Errorf("census: config: username is required")
	}
	if c.Password == "" {
		return fmt.Errorf("census: config: password is required")
	}
	switch c.TLS {
	case TLSImplicit, TLSStartTLS, TLSInsecure:
	default:
		return fmt.Errorf("census: config: invalid TLS mode %v", c.TLS)
	}
	return nil
}

func (c Config) addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// Dial connects and authenticates to an IMAP server, returning a Reader.
// ctx bounds how long the caller is willing to wait: on cancellation, Dial
// stops waiting and returns promptly, but because go-imap's Dial routines
// have no native context support, an in-flight TCP handshake or LOGIN is
// not aborted immediately — Dial closes the resulting connection in the
// background once it completes, so no socket is leaked. Mirrors
// internal/sync.Dial's shape; see that function's doc for the same
// reasoning in more detail.
func Dial(ctx context.Context, cfg Config) (Reader, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("census: dial: %w", err)
	}

	options := &imapclient.Options{
		Dialer: &net.Dialer{Timeout: defaultDialTimeout},
	}
	if cfg.TLS != TLSInsecure {
		options.TLSConfig = &tls.Config{ServerName: cfg.Host}
	}

	type dialResult struct {
		client *imapclient.Client
		err    error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		client, err := dialClient(cfg, options)
		if err == nil {
			err = client.Login(cfg.Username, cfg.Password).Wait()
		}
		resultCh <- dialResult{client, err}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			if res.client != nil {
				_ = res.client.Close() // best-effort: we're already returning res.err
			}
			return nil, fmt.Errorf("census: dial: %w", res.err)
		}
		return &imapReader{client: res.client}, nil
	case <-ctx.Done():
		go func() {
			res := <-resultCh
			if res.client != nil {
				_ = res.client.Close() // best-effort cleanup of a connection nobody is waiting on anymore
			}
		}()
		return nil, fmt.Errorf("census: dial: %w", ctx.Err())
	}
}

// DialProbe adapts Dial to the shape ProbeConnections needs: a fresh
// Dial+LOGIN IS a held connection (Reader.Close logs the session out), so
// no separate "probe dial" concept exists.
func DialProbe(cfg Config) func(ctx context.Context) (io.Closer, error) {
	return func(ctx context.Context) (io.Closer, error) {
		r, err := Dial(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return r, nil
	}
}

// dialClient performs the TLS-mode-specific connect step. It does not log in.
func dialClient(cfg Config, options *imapclient.Options) (*imapclient.Client, error) {
	addr := cfg.addr()
	switch cfg.TLS {
	case TLSImplicit:
		return imapclient.DialTLS(addr, options)
	case TLSStartTLS:
		return imapclient.DialStartTLS(addr, options)
	case TLSInsecure:
		return imapclient.DialInsecure(addr, options)
	default:
		return nil, fmt.Errorf("census: unsupported TLS mode %v", cfg.TLS)
	}
}

// imapReader implements Reader over *imapclient.Client. Not safe for
// concurrent use — one imapReader drives one IMAP connection, exactly like
// internal/sync's session.
type imapReader struct {
	client *imapclient.Client
}

var _ Reader = (*imapReader)(nil)

func (r *imapReader) Capabilities(ctx context.Context) ([]string, error) {
	var out []string
	err := r.withContext(ctx, "capability", func() error {
		caps, err := r.client.Capability().Wait()
		if err != nil {
			return err
		}
		out = make([]string, 0, len(caps))
		for c := range caps {
			out = append(out, string(c))
		}
		return nil
	})
	return out, err
}

// capForLookup mirrors go-imap's own capability canonicalization
// (internal.canonicalCap, unexported in the module and restated here):
// every capability name is uppercased, except IMAP4rev1/IMAP4rev2, which
// fold to their registered mixed-case spelling. CapabilitiesEffective runs
// BOTH the raw advertised names and the names it is asked about through
// this, so the identity check for IMAP4rev2 itself — and everything
// IMAP4rev2's presence implies — works regardless of casing on either
// side, exactly as it would for a live imap.CapSet built by go-imap itself.
func capForLookup(name string) imap.Cap {
	switch strings.ToUpper(name) {
	case "IMAP4REV1":
		return imap.CapIMAP4rev1
	case "IMAP4REV2":
		return imap.CapIMAP4rev2
	default:
		return imap.Cap(strings.ToUpper(name))
	}
}

// CapabilitiesEffective reports, for each name in want, whether it is
// EFFECTIVE given the raw advertised capability list raw — per go-imap's
// own imap.CapSet.Has(), which folds in what a capability's presence
// IMPLIES (IMAP4rev2 implies UIDPLUS/MOVE/IDLE/UNSELECT/…, RFC 9051 §6.2.1;
// QRESYNC implies CONDSTORE, RFC 7162 §3.2.1) rather than only what the
// server chose to print verbatim in its CAPABILITY response.
//
// This is the EXACT call internal/sync.SyncSession.Supports makes —
// `s.client.Caps().Has(imap.Cap(c))`, internal/sync/session.go — so its
// answer is what Mailroom's own sync engine would actually decide, which is
// the number worth reporting: "not advertised" can still mean "Mailroom
// will use it anyway", and a census that only echoed the raw CAPABILITY
// line back would misreport that as absent.
//
// A pure function over strings, not a Reader method, so census.go's Run —
// and its fake-Reader unit tests — can call it without a live connection.
// This file stays the only one in the package that imports go-imap: this
// is the one place that builds the imap.CapSet the answer depends on.
func CapabilitiesEffective(raw []string, want []string) map[string]bool {
	set := make(imap.CapSet, len(raw))
	for _, c := range raw {
		set[capForLookup(c)] = struct{}{}
	}
	out := make(map[string]bool, len(want))
	for _, w := range want {
		out[w] = set.Has(capForLookup(w))
	}
	return out
}

func (r *imapReader) ListMailboxes(ctx context.Context) ([]Mailbox, error) {
	var out []Mailbox
	err := r.withContext(ctx, "list", func() error {
		data, err := r.client.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
		if err != nil {
			return err
		}
		out = make([]Mailbox, 0, len(data))
		for _, d := range data {
			out = append(out, Mailbox{Name: d.Mailbox, Attributes: lowercaseAttrs(d.Attrs)})
		}
		return nil
	})
	return out, err
}

func lowercaseAttrs(attrs []imap.MailboxAttr) []string {
	if attrs == nil {
		return nil
	}
	out := make([]string, len(attrs))
	for i, a := range attrs {
		out[i] = strings.ToLower(string(a))
	}
	return out
}

// Examine opens name read-only (EXAMINE, never SELECT — see the Examined.
// ReadOnly doc for exactly what that does and does not prove with this
// client library) and streams one Meta per message.
//
// It fetches UID, FLAGS, INTERNALDATE, RFC822.SIZE, ENVELOPE and the
// EXTENDED BODYSTRUCTURE (the extended form is what carries
// Content-Disposition, which HasAttachment needs) — and nothing else. It
// never requests BODY[] or any body section: the census counts and
// classifies mail, it does not read it.
//
// The fetch streams via the command's Next()/Collect() rather than
// Command.Collect(), exactly like internal/sync.SyncSession.FetchByUID —
// see that method's doc for why buffering an entire mailbox response is
// unsafe at the sizes this tool is meant to run against.
func (r *imapReader) Examine(ctx context.Context, name string, fn func(Meta) error) (Examined, error) {
	var examined Examined
	err := r.withContext(ctx, "examine", func() error {
		data, err := r.client.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return err
		}
		examined = Examined{
			Messages:    data.NumMessages,
			UIDValidity: data.UIDValidity,
			UIDNext:     uint32(data.UIDNext),
		}
		if data.NumMessages == 0 {
			return nil
		}

		var seqSet imap.SeqSet
		seqSet.AddRange(1, 0) // "1:*" — everything currently in the mailbox
		fetchOpts := &imap.FetchOptions{
			UID:           true,
			Flags:         true,
			InternalDate:  true,
			RFC822Size:    true,
			Envelope:      true,
			BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		}

		cmd := r.client.Fetch(seqSet, fetchOpts)
		for {
			md := cmd.Next()
			if md == nil {
				break
			}
			buf, err := md.Collect()
			if err != nil {
				_ = cmd.Close() // drains the rest so the connection stays usable
				return err
			}
			if err := fn(mapMeta(buf)); err != nil {
				_ = cmd.Close()
				return err
			}
		}
		return cmd.Close()
	})
	return examined, err
}

// mapMeta converts a collected FETCH response for one message into Meta.
func mapMeta(buf *imapclient.FetchMessageBuffer) Meta {
	m := Meta{
		UID:          uint32(buf.UID),
		Flags:        mapFlags(buf.Flags),
		InternalDate: buf.InternalDate,
		Size:         buf.RFC822Size,
	}
	if buf.Envelope != nil {
		m.MessageID = buf.Envelope.MessageID
	}
	if buf.BodyStructure != nil {
		m.Structure = mapPart(buf.BodyStructure)
	}
	return m
}

func mapFlags(flags []imap.Flag) []string {
	if flags == nil {
		return nil
	}
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}

// mapPart converts a go-imap BodyStructure tree into census's own *Part
// tree. A multipart container becomes a Part with Children; a single part
// becomes a leaf Part with no Children — including message/rfc822, whose
// OWN nested structure (bs.MessageRFC822.BodyStructure) is deliberately not
// descended into: HasAttachment treats message/rfc822 as a leaf that is
// never itself an attachment, to match how internal/mime/walk.go's
// walkNestedMessage keeps a nested message out of the parent's flattened
// Attachments/has_attachments (see census.go's HasAttachment doc).
func mapPart(bs imap.BodyStructure) *Part {
	switch v := bs.(type) {
	case *imap.BodyStructureMultiPart:
		children := make([]*Part, 0, len(v.Children))
		for _, c := range v.Children {
			children = append(children, mapPart(c))
		}
		return &Part{MediaType: v.MediaType(), Children: children}
	case *imap.BodyStructureSinglePart:
		disp := ""
		if d := v.Disposition(); d != nil {
			disp = strings.ToLower(d.Value)
		}
		return &Part{MediaType: v.MediaType(), Disposition: disp}
	default:
		return nil
	}
}

func (r *imapReader) Close() error {
	// LOGOUT first, best-effort: it tells the server this session is done
	// rather than just vanishing, but a failure to log out cleanly should
	// not mask (or block on) releasing the socket via the client Close call
	// below, which is the part that actually matters for ProbeConnections
	// (releasing a held connection).
	_ = r.client.Logout().Wait()
	if err := r.client.Close(); err != nil {
		return fmt.Errorf("census: close: %w", err)
	}
	return nil
}

// withContext runs fn in a goroutine and waits for either it to finish or
// ctx to be done. go-imap's Client has no native per-command cancellation,
// so on cancellation this force-closes the connection to unblock fn — same
// shape and same reasoning as internal/sync's withContext.
func (r *imapReader) withContext(ctx context.Context, op string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("census: %s: %w", op, err)
	}

	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("census: %s: %w", op, err)
		}
		return nil
	case <-ctx.Done():
		_ = r.client.Close()
		<-done
		return fmt.Errorf("census: %s: %w", op, ctx.Err())
	}
}
