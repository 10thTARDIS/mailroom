<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# PGP keys

How to generate, publish, back up and restore a user's PGP key, and the DNS you must get right.

**On this page:** [Generate and bind a key](#generate-and-bind-a-key) · [Publish it over WKD](#publish-it-over-wkd) ·
[The DNS half](#the-dns-half) · [Back up and restore a key](#back-up-and-restore-a-key) ·
[Key discovery](#key-discovery)

Keys live behind the separate `crypto` container ([security](security.md#the-crypto-process)).
Commands use `mradmin` from [admin CLI](admin-cli.md#run-a-command).

## Generate and bind a key

```sh
mradmin pgp-generate --user-id <ULID> --email you@example.com
mradmin pgp-bind     --user-id <ULID> --key-id <KEY> --identity-id <IDENTITY>
mradmin pgp-list     --user-id <ULID>
```

Mail sent from a bound identity is signed. While `crypto` is down those identities cannot send.
A generated key exists nowhere else, so [back it up](#back-up-and-restore-a-key) before relying on it.

## Publish it over WKD

Publishing makes a key findable by anyone who knows the address. That is why it is explicit.
Nothing is public until this step; until then every lookup returns 404.

```sh
mradmin pgp-publish --user-id <ULID> --key-id <KEY>     # prints the exact URLs clients will request
mradmin pgp-unpublish --user-id <ULID> --key-id <KEY>   # withdraw
```

Then set `MAILROOM_WKD_DOMAIN` in `.env` to the mail domain and restart. Check it:

```sh
PORT="${MAILROOM_HTTP_PORT:-8080}"
curl -sI "http://localhost:$PORT/.well-known/openpgpkey/policy"           # 200
curl -s  "http://localhost:$PORT/.well-known/openpgpkey/hu/<hash>" | file -   # "OpenPGP Public Key" or "data", NOT ASCII text
```

If the last command says "ASCII text" the endpoint is broken even though it returned 200: WKD
serves raw key packets, and every server-side signal stays green while no mail client accepts the key.

Withdrawing stops new lookups. It cannot recall a key someone already cached; revocation
(`pgp-revoke`) is the way to say "stop trusting this".

## The DNS half

A correspondent's client tries the advanced method first, then the direct one:

```
https://openpgpkey.example.com/.well-known/openpgpkey/example.com/hu/<hash>
https://example.com/.well-known/openpgpkey/hu/<hash>
```

An `openpgpkey.` subdomain that exists but serves keys wrongly **breaks every lookup**, including
ones the direct method would have answered. It must be absent or fully correct. The proof is a real
client, not `curl`:

```sh
gpg --auto-key-locate clear,wkd --locate-keys you@example.com
```

## Back up and restore a key

A portable copy that neither the database nor the PGP root is needed to open. Take one for every
key you generate. It is one ASCII-armoured private key encrypted under a passphrase **you** choose
(first line of a file, at least 12 characters). Mailroom neither stores nor recovers it, and
`gpg --import` reads it.

```sh
C=deploy/compose.yml; umask 077
printf '%s' '<a passphrase of 12+ characters>' > /tmp/pgp-pass.txt
docker compose -f $C cp /tmp/pgp-pass.txt mailroom:/tmp/pgp-pass.txt
docker compose -f $C exec mailroom /mailroom-admin pgp-export-wrapped \
  --user-id <ULID> --key-id <KEY> --passphrase-file /tmp/pgp-pass.txt --out /tmp/pgp-key-<KEY>.asc
docker compose -f $C cp mailroom:/tmp/pgp-key-<KEY>.asc ./pgp-key-<KEY>.asc
docker compose -f $C exec mailroom rm -f /tmp/pgp-pass.txt /tmp/pgp-key-<KEY>.asc
shred -u /tmp/pgp-pass.txt 2>/dev/null || rm -f /tmp/pgp-pass.txt
gpg --import ./pgp-key-<KEY>.asc          # check it opens BEFORE relying on it
```

`--out` must not exist; the command refuses to overwrite. Keep the file and the passphrase in
**different places** (file in off-host storage, passphrase in a password manager). Either alone is
useless; together they are the private key. Keep neither on this host.

**Restore** on a fresh instance or after a deletion:

```sh
docker compose -f $C cp ./pgp-key-<KEY>.asc mailroom:/tmp/pgp-key.asc
docker compose -f $C cp /tmp/pgp-pass.txt mailroom:/tmp/pgp-pass.txt
docker compose -f $C exec mailroom /mailroom-admin pgp-import \
  --user-id <ULID> --file /tmp/pgp-key.asc --passphrase-file /tmp/pgp-pass.txt
docker compose -f $C exec mailroom rm -f /tmp/pgp-key.asc /tmp/pgp-pass.txt
mradmin pgp-list --user-id <ULID>
mradmin pgp-bind --user-id <ULID> --key-id <NEW-KEY> --identity-id <IDENTITY>   # re-attach
mradmin pgp-publish --user-id <ULID> --key-id <NEW-KEY>                         # if it was published
```

The fingerprint is unchanged and the key id is new (use the one `pgp-import` prints). A wrong
passphrase gives one error, "could not unlock the key"; without `--passphrase-file` a protected
file is refused. Importing a key already held changes nothing.

**Without a backup,** if the database or the PGP root is lost the key is gone for good: nothing can
sign as it and mail already encrypted to it cannot be opened. A published key must be replaced and
re-published. Keys you imported yourself are different: you still hold the original.

## Key discovery

From v1.64, Mailroom exchanges keys in band with no configuration or DNS:

- **Outbound:** mail from an identity with a bound key carries an `Autocrypt:` header with the
  public key. Only designated identities (those with a `default_key_id`) do, so a catch-all's
  derived aliases advertise nothing.
- **Inbound:** a correspondent's `Autocrypt:` key is filed against their contact. It is discarded
  unless the header's address equals the message's `From`, so a sender cannot plant a key under
  someone else's address.
- Mailroom does not advertise `prefer-encrypt=mutual`, so correspondents encrypt to you only when
  they choose to rather than by default.

To look rather than wait, sweep your contacts' domains (report first):

```sh
mradmin key-discover --user <ULID>            # report only
mradmin key-discover --user <ULID> --apply    # store what it finds
```

Lookups use the guarded egress client. Expect a near-empty result; the report prints how many
addresses it tried, so a zero reads as "nobody publishes".

## Related

- [Security](security.md)
- [Key escrow](../key-escrow.md)
- [Admin CLI](admin-cli.md#pgp)
