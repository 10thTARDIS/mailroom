// SPDX-License-Identifier: AGPL-3.0-only

// Package egresstest holds the test doubles every guarded dial path is tested
// with: a scripted resolver and a listener that RECORDS CONNECTIONS.
//
// The listener is the point. A guard test that asserts only "an error came
// back" passes against a dialler that connected first and failed afterwards —
// and for an account dial the connection is the harm (the credential follows
// it). So the guarded paths are asserted on what a real socket observed: a
// refused host must leave the listener with ZERO accepted connections, and the
// permitted case must show exactly the connection the guard allowed. It is only
// imported by tests.
package egresstest

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	gosync "sync"
	"testing"
	"time"
)

// Resolver answers from a script of successive replies; the last reply
// repeats. A two-step script ({public}, {loopback}) is DNS rebinding: the
// answer validation saw, then the answer the next connection gets.
type Resolver struct {
	mu      gosync.Mutex
	answers [][]netip.Addr
	calls   int
}

// NewResolver builds a Resolver from successive answers, each a list of
// address strings. A string may carry an IPv6 zone ("fe80::1%eth0").
func NewResolver(t testing.TB, answers ...[]string) *Resolver {
	t.Helper()
	r := &Resolver{}
	for _, ans := range answers {
		addrs := make([]netip.Addr, 0, len(ans))
		for _, s := range ans {
			a, err := netip.ParseAddr(s)
			if err != nil {
				t.Fatalf("egresstest: bad fixture address %q: %v", s, err)
			}
			addrs = append(addrs, a)
		}
		r.answers = append(r.answers, addrs)
	}
	return r
}

// LookupNetIP implements egress.Resolver.
func (r *Resolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.calls
	r.calls++
	if len(r.answers) == 0 {
		return nil, fmt.Errorf("egresstest: no such host: %s", host)
	}
	if i >= len(r.answers) {
		i = len(r.answers) - 1
	}
	return append([]netip.Addr(nil), r.answers[i]...), nil
}

// Calls reports how many lookups have been answered.
func (r *Resolver) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Listener is a TCP listener on 127.0.0.1 that records every connection it
// accepts and hands each to an optional handler.
type Listener struct {
	ln      net.Listener
	mu      gosync.Mutex
	remotes []string
	arrived chan struct{}
}

// Listen starts a recording listener on 127.0.0.1. handle, if non-nil, serves
// each accepted connection (and must close it); nil closes it immediately.
func Listen(t testing.TB, handle func(net.Conn)) *Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("egresstest: listen: %v", err)
	}
	l := &Listener{ln: ln, arrived: make(chan struct{}, 1024)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			l.remotes = append(l.remotes, c.RemoteAddr().String())
			l.mu.Unlock()
			l.arrived <- struct{}{}
			if handle != nil {
				go handle(c)
			} else {
				_ = c.Close()
			}
		}
	}()
	return l
}

// Port is the listener's port.
func (l *Listener) Port() int {
	_, p, _ := net.SplitHostPort(l.ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

// Connections returns how many connections reached the listener BEFORE this
// call, exactly. It is not a sleep-and-hope count: it makes one sentinel
// connection of its own and waits until the listener has accepted it. The
// kernel's accept queue is FIFO, so once the sentinel is accepted every
// connection that arrived earlier has been accepted too — and recorded.
func (l *Listener) Connections(t testing.TB) int {
	t.Helper()
	c, err := net.DialTimeout("tcp", l.ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("egresstest: sentinel dial: %v", err)
	}
	sentinel := c.LocalAddr().String()
	defer func() { _ = c.Close() }()
	deadline := time.After(5 * time.Second)
	for {
		l.mu.Lock()
		for i, r := range l.remotes {
			if r == sentinel {
				// Remove the sentinel so a later call counts only real traffic.
				l.remotes = append(l.remotes[:i], l.remotes[i+1:]...)
				n := len(l.remotes)
				l.mu.Unlock()
				return n
			}
		}
		l.mu.Unlock()
		select {
		case <-l.arrived:
		case <-deadline:
			t.Fatal("egresstest: the sentinel connection was never accepted")
			return -1
		}
	}
}
