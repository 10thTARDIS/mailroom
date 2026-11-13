#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Regenerates scripts/a11y-keychange/ (#150): the SYNTHETIC correspondents with a
# rotated key that the accessibility lane (a11y-e2e.sh) delivers and a11y-check.cjs
# opens. NOT run by the lane: the output is committed, so the lane has no gpg
# dependency and its fixture does not move with the calendar. Run it by hand
# (needs GnuPG 2.4, nothing else) only to change the fixture.
#
# EVERY KEY IS MADE BY A REAL GnuPG, not by Mailroom's own key tooling, so what the
# lane feeds the product is a second implementation's idea of a certification and
# a revocation (fixture realism: a stand-in made by the code under test encodes
# that code's belief about the format). Every key is made with a faked clock in the
# past (2026-08/09/10) and never expires, so the fixture does not age. The secret
# keys are thrown away with the temporary GNUPGHOME: nothing here is a secret.
#
# FIVE correspondents, all at example.org, each with an OLD key P (what the user
# has pinned) and a NEW key N (what a signed message arrives with):
#
#   vera  N carries a certification by P over its user ID          -> vouched
#   rhea  N carries none                                           -> ordinary
#   wren  as vera, but P is revoked as COMPROMISED                  -> compromised
#   pia   as vera; the user trusts N (so the pin is N, source
#         "vouched:P"); P is then revoked (no reason)              -> rule 8
#   cora  as pia, but P is revoked as COMPROMISED                  -> rule 8, stolen
#
# Output, per correspondent <n>:
#   <n>-old.asc          P, as the user imported it
#   <n>-old-revoked.asc  P with its revocation (wren, pia, cora): delivered later
#                        in an Autocrypt header, which is how a revocation arrives
#   <n>-new.asc          N (with the certification, except rhea)
#   <n>.eml              a PGP/MIME message signed by N, with N in its Autocrypt
#                        header (CRLF line ends: the signed bytes are canonical)
#   <n>-revoke.eml       a plain message whose Autocrypt header carries P with its
#                        revocation (wren, pia, cora)
#   fingerprints.txt     "<n> old <fpr>" / "<n> new <fpr>"
set -eu

OUT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/a11y-keychange"
GH="$(mktemp -d)"
trap 'rm -rf "$GH"' EXIT INT TERM
chmod 700 "$GH"
mkdir -p "$OUT"
rm -f "$OUT"/*

# g <homedir> <gpg args...>: no passphrase, no prompts. (--gen-revoke refuses
# --batch, so that one call is spelled out below and answered through --command-fd.)
g() { _h="$1"; shift; gpg --homedir "$_h" --batch --yes --pinentry-mode loopback --passphrase '' "$@"; }

# gen <homedir> <uid> <created>: a key that cannot sign for ever expires never.
gen() {
  mkdir -p "$1"; chmod 700 "$1"
  g "$1" --faked-system-time "$3" --quick-generate-key "$2" ed25519 cert,sign never >/dev/null 2>&1
  _l="$(g "$1" --list-keys --with-colons)"
  _f="$(printf '%s\n' "$_l" | awk -F: '$1=="fpr"{print $10; exit}')"
  g "$1" --faked-system-time "$3" --quick-add-key "$_f" cv25519 encr never >/dev/null 2>&1
  echo "$_f"
}

# autocrypt <addr> <armored pub>: the header, folded as a mail client folds it.
autocrypt() {
  printf 'Autocrypt: addr=%s; keydata=\r\n' "$1"
  gpg --dearmor < "$2" | base64 -w 72 | sed 's/^/ /; s/$/\r/'
}

# message <name> <display> <signer home> <signer fpr> <pub of the signer>
message() {
  _t="$GH/$1.body"
  # The signed part: headers and body, CRLF, no trailing CRLF (RFC 3156 section 5).
  printf 'Content-Type: text/plain; charset=utf-8\r\n\r\nSynthetic note from %s, signed with a key made for this test.' "$2" > "$_t"
  g "$3" --faked-system-time 20261020T100000 --local-user "$4" --detach-sign --armor -o "$GH/$1.sig" "$_t"
  {
    printf 'From: %s <%s@example.org>\r\n' "$2" "$1"
    printf 'To: Reader <reader@example.com>\r\n'
    printf 'Subject: Signed note from %s\r\n' "$2"
    printf 'Message-ID: <kc-%s@example.test>\r\n' "$1"
    printf 'Date: Tue, 20 Oct 2026 10:00:00 +0000\r\n'
    printf 'MIME-Version: 1.0\r\n'
    autocrypt "$1@example.org" "$5"
    printf 'Content-Type: multipart/signed; micalg=pgp-sha512; protocol="application/pgp-signature"; boundary="kcb"\r\n\r\n'
    printf -- '--kcb\r\n'
    cat "$_t"
    printf '\r\n--kcb\r\nContent-Type: application/pgp-signature; name="signature.asc"\r\n\r\n'
    sed 's/$/\r/' "$GH/$1.sig"
    printf -- '--kcb--\r\n'
  } > "$OUT/$1.eml"
}

# revokemsg <name> <display> <pub of the old key with its revocation>
revokemsg() {
  {
    printf 'From: %s <%s@example.org>\r\n' "$2" "$1"
    printf 'To: Reader <reader@example.com>\r\n'
    printf 'Subject: A note about my old key (%s)\r\n' "$1"
    printf 'Message-ID: <kc-%s-revoke@example.test>\r\n' "$1"
    printf 'Date: Wed, 21 Oct 2026 10:00:00 +0000\r\n'
    printf 'MIME-Version: 1.0\r\n'
    autocrypt "$1@example.org" "$3"
    printf 'Content-Type: text/plain; charset=utf-8\r\n\r\nMy old key is no longer in use.\r\n'
  } > "$OUT/$1-revoke.eml"
}

: > "$OUT/fingerprints.txt"
for spec in "vera|Vera Vouched|cert|" "rhea|Rhea Ordinary|nocert|" \
            "wren|Wren Compromised|cert|1" "pia|Pia Previous|cert|3" "cora|Cora Compromised|cert|1"; do
  name="${spec%%|*}"; rest="${spec#*|}"; display="${rest%%|*}"; rest="${rest#*|}"
  cert="${rest%%|*}"; reason="${rest#*|}"
  uid="$display <$name@example.org>"
  PH="$GH/$name-p"; NH="$GH/$name-n"
  P="$(gen "$PH" "$uid" 20260801T100000)"
  N="$(gen "$NH" "$uid" 20260902T100000)"
  [ -n "$P" ] && [ -n "$N" ] && [ "$P" != "$N" ] || { echo "no keys for $name" >&2; exit 1; }
  g "$PH" --armor --export "$P" > "$OUT/$name-old.asc"
  if [ "$cert" = "cert" ]; then
    # P certifies N's user ID (a generic certification, 0x10) once N exists:
    # N's public half goes into P's keyring, P signs it there, and it comes out
    # with the certification attached.
    g "$NH" --export "$N" | g "$PH" --import >/dev/null 2>&1
    g "$PH" --faked-system-time 20260903T100000 --local-user "$P" --quick-sign-key "$N" "$uid" >/dev/null 2>&1
    g "$PH" --armor --export "$N" > "$OUT/$name-new.asc"
  else
    g "$NH" --armor --export "$N" > "$OUT/$name-new.asc"
  fi
  if [ -n "$reason" ]; then
    printf 'y\n%s\nSynthetic revocation\n\ny\n' "$reason" | \
      gpg --homedir "$PH" --no-tty --yes --pinentry-mode loopback --passphrase '' --command-fd 0 --faked-system-time 20261015T100000 --output "$GH/$name.rev" --gen-revoke "$P" >/dev/null 2>&1
    g "$PH" --import "$GH/$name.rev" >/dev/null 2>&1
    g "$PH" --armor --export "$P" > "$OUT/$name-old-revoked.asc"
    revokemsg "$name" "$display" "$OUT/$name-old-revoked.asc"
  fi
  message "$name" "$display" "$NH" "$N" "$OUT/$name-new.asc"
  echo "$name old $P" >> "$OUT/fingerprints.txt"
  echo "$name new $N" >> "$OUT/fingerprints.txt"
done
echo "wrote $OUT"
