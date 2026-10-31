// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSMTP is a minimal ESMTP server for probing tests: it greets, answers
// EHLO with a fixed capability set, and closes on QUIT. It speaks just enough
// of RFC 5321 for net/smtp's EHLO handshake.
func fakeSMTP(t *testing.T, caps []string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeSMTP(conn, caps)
		}
	}()

	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}

func serveFakeSMTP(conn net.Conn, caps []string) {
	defer func() { _ = conn.Close() }()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	writeLine := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }

	writeLine("220 fake ESMTP ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			// Multiline 250: all but the last line use "250-".
			lines := append([]string{"fake greets you"}, caps...)
			for i, c := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				writeLine("250" + sep + c)
			}
		case strings.HasPrefix(cmd, "HELO"):
			writeLine("250 fake")
		case strings.HasPrefix(cmd, "QUIT"):
			writeLine("221 Bye")
			return
		default:
			writeLine("250 OK")
		}
	}
}

func TestProbeSMTPDetectsSMTPUTF8(t *testing.T) {
	host, port := fakeSMTP(t, []string{"SIZE 35882577", "SMTPUTF8", "AUTH PLAIN LOGIN"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	caps, err := ProbeSMTP(ctx, host, port, WithoutSTARTTLS(), WithAllowPrivateHosts())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.SMTPUTF8 {
		t.Error("SMTPUTF8 should be detected")
	}
	if caps.Size != 35882577 {
		t.Errorf("Size = %d, want 35882577", caps.Size)
	}
	if fmt.Sprint(caps.Auth) != "[PLAIN LOGIN]" {
		t.Errorf("Auth = %v, want [PLAIN LOGIN]", caps.Auth)
	}
}

func TestProbeSMTPAbsentSMTPUTF8(t *testing.T) {
	host, port := fakeSMTP(t, []string{"SIZE 10240000", "AUTH PLAIN"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	caps, err := ProbeSMTP(ctx, host, port, WithoutSTARTTLS(), WithAllowPrivateHosts())
	if err != nil {
		t.Fatal(err)
	}
	if caps.SMTPUTF8 {
		t.Error("SMTPUTF8 should be absent — compose must reject EAI addresses")
	}
	if caps.STARTTLS {
		t.Error("STARTTLS was not advertised")
	}
}

func TestProbeSMTPDialError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Port 0 is not connectable.
	if _, err := ProbeSMTP(ctx, "127.0.0.1", 1, WithoutSTARTTLS(), WithAllowPrivateHosts()); err == nil {
		t.Error("expected a dial error")
	}
}
