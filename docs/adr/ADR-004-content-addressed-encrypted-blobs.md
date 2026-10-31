# ADR-004: Content-addressed blobs, per-user DEKs, versioned envelope

**Status:** Accepted (spec v0.4 §9/§10; design review D4)

## Context

Raw MIME, attachments, credentials, and private keys need at-rest encryption
(spec §11 inventory). Blob layout must survive schema migrations untouched
and must not let a path-construction bug leak another user's plaintext.

## Decision

Blobs are content-addressed (`SHA-256` of plaintext) under per-user prefixes
derived from `HMAC(server_key, user_id)` (derivable ⇒ not stored;
non-enumerable). Encryption is envelope-based: per-user, per-purpose DEKs
wrapped by the user KEK; ciphertext carries a versioned envelope header
(key-id + algorithm); large attachments use chunked AEAD. Filesystem blobs
behind a `BlobStore` interface (MinIO slots in later).

## Consequences

- Per-user DEKs are the real isolation backstop: a path bug leaks ciphertext
  under the wrong key — whole bug classes become non-events.
- Cross-user blob dedup is deliberately impossible (isolation property).
- DEK rotation = new DEK for new writes + lazy background re-encrypt; schema
  migrations never touch blob bytes.

## Amendment (2026-09-19): what the file names reveal, accepted

The per-user **directory** is keyed (the HMAC above), but the **file name**
inside it is the unkeyed SHA-256 of the plaintext (`internal/blob/fs.go`,
`blobPath`; `internal/blob/blob.go`, `ContentHash`). Nothing here said so. The
contents stay encrypted, but someone who can list the blob volume, without the
master key, learns:

- **whether a message they already hold is stored**: hash it and look for the
  name (confirmation of a file);
- **that two users hold identical content**: the same name under two
  different, but unreadable, prefixes;
- per-prefix counts, sizes and modification times.

**Accepted, not fixed.** The threat is someone reading the volume offline, and
at-rest encryption of the data volumes is already a deployment requirement
(`deploy/README.md` §0a), which covers it. Keying the file names (an HMAC of
the content hash under a key derived from the master key) would close it, but
it renames every stored blob on every instance: a migration touching every
file, with its own failure modes, for a leak the volume encryption already
contains.

**Revisit** if blobs move to storage that is not encrypted at rest under the
operator's control (an object store, a shared backup target that keeps the
raw layout), or if the deployment model gains tenants who do not trust the
operator's host. Backup artifacts are unaffected: the whole blob tree is
sealed inside the age-encrypted artifact, names included.

## References

spec §9, §10, §11, §14 · mailroom-data-model.md §2 · design review D4, deep-dive A/D
