#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Runs INSIDE the dovecot/dovecot image (scripts/sieve-live-test.sh copies it
# in). It executes Mailroom's generated out-of-office scripts with Pigeonhole's
# own interpreter, sieve-test, against four messages, and asserts who gets a
# reply (#50).
#
# This is the check the protocol tests cannot make. CHECKSCRIPT only proves a
# script COMPILES; this proves it DOES the right thing on the real
# interpreter: the window guard, the spam headers and the contacts-only list.
# The current window must reply to a contact (the positive control). An expired
# window, a window that has not started yet, a spam-marked message and a
# stranger must not.
set -u
DIR=${1:-/emit}
cd "$DIR" || exit 2

mk() { printf 'From: %s\r\nTo: me@example.com\r\nSubject: hello\r\nMessage-ID: <%s@example.test>\r\n%s\r\nbody\r\n' "$1" "$2" "$3" > "$DIR/$2.eml"; }
mk friend@example.net normal ""
mk friend@example.net spam "X-Spam: Yes"
mk friend@example.net spamstatus "X-Spam-Status: Yes, score=9.1"
mk stranger@example.net stranger ""

expect() { # script message want(REPLY|none)
  out=$(sieve-test -a me@example.com -r me@example.com "$DIR/$1.sieve" "$DIR/$2.eml" 2>&1)
  if printf '%s' "$out" | grep -q "send vacation message"; then got=REPLY; else got=none; fi
  if printf '%s' "$out" | grep -qi "error: .*\(failed to compile\|parse\|unknown\)"; then
    echo "FAIL $1/$2: the interpreter reported an error:"; printf '%s\n' "$out"; return 1
  fi
  if [ "$got" != "$3" ]; then
    echo "FAIL $1/$2: got $got, want $3"; printf '%s\n' "$out"; return 1
  fi
  echo "ok   $1/$2: $got"
}

fail=0
for f in current expired future; do
  [ -f "$DIR/$f.sieve" ] || { echo "FAIL: $f.sieve was not generated"; exit 1; }
done
expect current normal REPLY || fail=1
expect current spam none || fail=1
expect current spamstatus none || fail=1
expect current stranger none || fail=1
expect expired normal none || fail=1
expect future normal none || fail=1
exit $fail
