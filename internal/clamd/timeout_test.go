// SPDX-License-Identifier: AGPL-3.0-only

package clamd

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// A stand-in clamd that answers PING normally but never replies to INSTREAM.
//
// It exists to reproduce the one situation the real daemon cannot be asked for
// on demand: a scan that runs past ScanTimeout while the service itself is
// perfectly healthy. Everything else in this package is tested against captured
// output from a real clamd (see clamd_test.go); this behaviour has no wire
// string to capture, only a timing.
type stallingClamd struct {
	ln net.Listener
	// pings counts PING commands answered, so a test can prove the daemon was
	// reachable throughout — otherwise "not an outage" would be an assertion
	// about a server nobody talked to.
	pings chan struct{}
}

func newStallingClamd(t *testing.T) *stallingClamd {
	t.Helper()
	// TCP, not a unix socket: the unit lane must pass on any host, and the
	// transport is irrelevant to what is being proven here.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &stallingClamd{ln: ln, pings: make(chan struct{}, 16)}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *stallingClamd) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			buf := make([]byte, 64)
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "PING") {
				_, _ = c.Write([]byte("PONG\x00"))
				select {
				case s.pings <- struct{}{}:
				default:
				}
				return
			}
			// An INSTREAM: read the caller's chunks and NEVER reply. The
			// connection stays open, so the client sees a read deadline rather
			// than an EOF — the shape a daemon grinding on a zip bomb produces.
			for {
				if _, err := c.Read(buf); err != nil {
					return
				}
			}
		}(conn)
	}
}

func (s *stallingClamd) client() *Client {
	return &Client{
		Network: "tcp", Address: s.ln.Addr().String(),
		// Short on purpose: the property is about which context the follow-up
		// PING runs on, not about how long the wait is.
		ScanTimeout: 300 * time.Millisecond,
	}
}

// ⚠ THE DEFECT THIS TEST EXISTS FOR, found by reading rather than by running.
//
// Scan derived its scan deadline into `ctx` and then issued the "is the daemon
// alive?" PING on that SAME context. A file that stalls the daemon past
// ScanTimeout leaves it Done, so the PING failed instantly with "context
// deadline exceeded" — against a daemon answering PONG on the next socket. The
// verdict came back ErrDaemonUnreachable.
//
// That is not a cosmetic misattribution. ErrDaemonUnreachable refuses the whole
// sync pass and withholds the cursor, so the same slow file is re-fetched first
// on every subsequent pass, forever: the account stops receiving mail because
// one message is slow to scan. ADR-027 §4b names this exact failure — "a single
// message must never pause the account" — and calls the alternative a remote
// kill switch operated by whoever sends the mail.
func TestAScanTimeoutIsPerFileNotAnOutage(t *testing.T) {
	s := newStallingClamd(t)

	v, err := s.client().Scan(context.Background(), []byte("bytes the daemon will never answer about"))
	if err != nil {
		t.Fatalf("Scan returned %v — a scan timeout against a LIVE daemon must be a per-file Unknown, "+
			"not an outage that refuses the account's whole sync pass", err)
	}
	if v.State != Unknown {
		t.Errorf("State = %q, want %q", v.State, Unknown)
	}
	if v.Reason == "" {
		t.Error("Unknown with no reason — the operator cannot tell a timeout from a malformed reply")
	}

	// The control that makes the assertion mean something: the daemon really was
	// reachable while the scan was timing out. Without this, "not an outage"
	// could be true of a server that was never contacted at all.
	select {
	case <-s.pings:
	default:
		t.Fatal("the daemon answered no PING, so this test proves nothing about outage attribution")
	}
}

// The other half of the same context split: when the CALLER gives up — a
// shutdown, a cancelled sync pass — that is neither a verdict nor an outage.
//
// Reporting it as ErrDaemonUnreachable would make every restart during a scan
// raise account health as "the malware scanner is unreachable", which is a false
// statement to the operator at the moment they are least able to check.
func TestACancelledCallerIsNotReportedAsAnOutage(t *testing.T) {
	s := newStallingClamd(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := s.client().Scan(ctx, []byte("bytes the daemon will never answer about"))
	if err == nil {
		t.Fatal("Scan returned a verdict for a cancelled call")
	}
	if errors.Is(err, ErrDaemonUnreachable) {
		t.Errorf("Scan reported a cancelled caller as a daemon outage: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled so the caller can tell a shutdown from a fault", err)
	}
}

// The positive control for BOTH tests above: this client, this transport and
// this call shape DO produce ErrDaemonUnreachable when the daemon is genuinely
// gone. Without it, the two tests could pass because Scan had stopped
// distinguishing anything at all.
func TestADeadDaemonStillProducesTheOutageSentinel(t *testing.T) {
	s := newStallingClamd(t)
	c := s.client()
	_ = s.ln.Close() // nothing is listening any more

	_, err := c.Scan(context.Background(), []byte("bytes"))
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("Scan against a closed listener returned %v, want ErrDaemonUnreachable — "+
			"if this fails, the two tests above prove nothing", err)
	}
}
