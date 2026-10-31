// SPDX-License-Identifier: AGPL-3.0-only

// Package sieve is Mailroom's ManageSieve (RFC 5804) anti-corruption layer
// (spec §3): the only place the ManageSieve wire protocol appears, and the
// structural reader for the Sieve (RFC 5228) scripts it fetches (#50).
//
// ⚠ READING AND WRITING ARE TWO TYPES, AND THE SPLIT IS STRUCTURAL. The
// provider allows exactly one active Sieve script, and the operator may already
// have server-side filters or a vacation message set in the provider's webmail.
// Writing a script — or merely activating a different one — silently replaces
// whatever filtered their mail before. So:
//
//   - Session is READ-ONLY. Its exported method set is CAPABILITY /
//     LISTSCRIPTS / GETSCRIPT / LOGOUT and nothing else
//     (TestSessionMethodSetIsReadOnly pins it), and every command goes through
//     do, which refuses any verb outside readVerbs BEFORE a byte reaches the
//     wire (TestWriteVerbNeverReachesTheWire drives it with PUTSCRIPT and
//     SETACTIVE against a server that records every byte). `sieve-report`
//     uses it and cannot write.
//   - WriteSession (write.go, #50 out of office) is the one type that can
//     change what a mail server does with incoming mail. It adds exactly
//     CHECKSCRIPT, PUTSCRIPT, SETACTIVE and DELETESCRIPT, pinned by
//     TestWriteSessionSurfaceIsExactlyThis. Which script it may touch is not
//     its decision: its one caller, internal/outofoffice, installs and removes
//     only its own script and refuses to act while another is active.
//
// (Until v1.90.0 this comment said the package was read-only; the writing
// half arrived with out of office, through its own reviewed change, as the
// comment then promised it would.)
//
// Credentials go over the wire in AUTHENTICATE, so TLS is not optional: the
// session is either implicit TLS or upgraded by STARTTLS, or it is abandoned
// before authenticating (AllowInsecureNoTLS exists only for in-process tests).
// The dial goes through egress.Dialer, the same guard as IMAP and SMTP.
package sieve
