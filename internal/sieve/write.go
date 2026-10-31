// SPDX-License-Identifier: AGPL-3.0-only

package sieve

import (
	"context"
	"strconv"
	"strings"
)

// ⚠ THE WRITING CLIENT (#50, out of office). This is the one type in Mailroom
// that can change what a mail server does with incoming mail, so it is kept
// apart from Session on purpose:
//
//   - Session stays read-only. Its allowlist is readVerbs and its exported
//     method set is pinned by TestSessionMethodSetIsReadOnly; sieve-report
//     goes on using it and cannot write.
//   - WriteSession adds exactly four verbs: CHECKSCRIPT, PUTSCRIPT, SETACTIVE
//     and DELETESCRIPT. RENAMESCRIPT and HAVESPACE are not needed and are not
//     allowed. Its own allowlist and method set are pinned by
//     TestWriteSessionSurfaceIsExactlyThis, so a new write verb is a reviewed
//     change to that test, not a quiet addition.
//
// The type knows nothing about WHICH script it may touch. That policy — never
// write, activate or delete a script Mailroom did not install — belongs to its
// one caller, internal/outofoffice, which names its own script and refuses to
// act while another is active.

// writeSessionVerbs is everything a WriteSession may send.
var writeSessionVerbs = func() map[string]bool {
	m := map[string]bool{
		"CHECKSCRIPT":  true,
		"PUTSCRIPT":    true,
		"SETACTIVE":    true,
		"DELETESCRIPT": true,
	}
	for v := range readVerbs {
		m[v] = true
	}
	return m
}()

// WriteSession is an authenticated ManageSieve session that can also upload,
// activate and delete scripts.
type WriteSession struct{ c *conn }

// OpenWrite opens a session that may write. Same dial, TLS and auth rules as
// Open.
func OpenWrite(ctx context.Context, cfg Config) (*WriteSession, error) {
	c, err := openConn(ctx, cfg, writeSessionVerbs)
	if err != nil {
		return nil, err
	}
	return &WriteSession{c: c}, nil
}

// Capabilities returns the post-authentication capability listing.
func (s *WriteSession) Capabilities() Capabilities { return s.c.caps }

// ListScripts returns every script on the server.
func (s *WriteSession) ListScripts(context.Context) ([]Script, error) { return s.c.listScripts() }

// GetScript returns one script's body.
func (s *WriteSession) GetScript(_ context.Context, name string) (string, error) {
	return s.c.getScript(name)
}

// CheckScript asks the server to compile body without storing it (RFC 5804
// §2.12). A NO carries the server's compile error.
func (s *WriteSession) CheckScript(_ context.Context, body string) error {
	_, err := s.c.do("CHECKSCRIPT", body)
	return err
}

// PutScript stores body under name, replacing a script of that name. It does
// not change which script is active (RFC 5804 §2.6).
func (s *WriteSession) PutScript(_ context.Context, name, body string) error {
	_, err := s.c.do("PUTSCRIPT", name, body)
	return err
}

// SetActive makes name the active script; "" deactivates every script
// (RFC 5804 §2.8).
func (s *WriteSession) SetActive(_ context.Context, name string) error {
	_, err := s.c.do("SETACTIVE", name)
	return err
}

// DeleteScript deletes an INACTIVE script; the server refuses to delete the
// active one (RFC 5804 §2.10).
func (s *WriteSession) DeleteScript(_ context.Context, name string) error {
	_, err := s.c.do("DELETESCRIPT", name)
	return err
}

// Logout ends the session politely. The connection is closed either way.
func (s *WriteSession) Logout(context.Context) error { return s.c.logout() }

// Close closes the connection without LOGOUT.
func (s *WriteSession) Close() error { return s.c.nc.Close() }

// MaxScriptSize is the server's advertised script size limit in bytes, from a
// "MAXSCRIPTSIZE" capability, or 0 when the server states none. (RFC 5804
// does not define this capability; providers that cap scripts advertise it,
// and the server's own PUTSCRIPT refusal remains the final word.)
func (c Capabilities) MaxScriptSize() int {
	for k, v := range c.Other {
		if strings.EqualFold(k, "MAXSCRIPTSIZE") {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}
