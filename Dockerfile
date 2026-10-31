# syntax=docker/dockerfile:1
#
# Mailroom production image (spec §11: containers non-root, minimal, no
# plaintext secrets baked in) and (spec §16: Docker Compose ops).
#
# Five binaries and the web client ship in the same image:
#   /mailroom          — the server (cmd/mailroom), default entrypoint.
#   /mailroom-crypto    — the crypto process (cmd/mailroom-crypto, ADR-024): the
#                          only holder of PGP private keys, run as its OWN
#                          long-lived service by deploy/compose.yml with its own
#                          root secret. It ships in the same image for one
#                          release-integrity reason — the server and the process
#                          it talks to must never be different builds — and for
#                          no other: at RUNTIME they are separate containers,
#                          with separate secrets, and the crypto container does
#                          not get the master key mounted.
#   /mailroom-migrate   — the goose-based schema migrator (cmd/mailroom-migrate),
#                          run as a one-shot job by deploy/compose.yml, never
#                          the default entrypoint (it must run as the
#                          mailroom_owner DB role, not the app's role).
#   /mailroom-admin     — the operator CLI (cmd/mailroom-admin): add/list
#                          accounts, run a one-shot sync. Not an entrypoint; it
#                          is the out-of-band escape hatch for fixing an account
#                          the declarative accounts file got wrong
#                          (`docker compose exec mailroom /mailroom-admin ...`).
#   /mailroom-census    — the read-only IMAP census (cmd/mailroom-census, #529):
#                          measures a mailbox BEFORE Mailroom touches it, to
#                          size a host and plan a cutover. It opens every folder
#                          with EXAMINE and holds no method that can write. In
#                          the image so the cutover runbook can run it wherever
#                          the image can be pulled, instead of "build it
#                          separately" (which no document explained).
#   /srv/web            — the built web client (web/, Forgejo #23), served at "/"
#                          by cmd/mailroom's webHandler when the directory
#                          exists (config.DefaultWebRoot). Without it the server
#                          answers "mailroom: no web client installed" and only
#                          the API works — which is exactly what shipped before
#                          this stage existed.
#
# Build:  docker build -t mailroom:test .
# Run:    docker run --rm mailroom:test          # starts the server
#         docker run --rm --entrypoint /mailroom-migrate mailroom:test status
#
# ⚠ EVERY OTHER BINARY NEEDS --entrypoint. The ENTRYPOINT is /mailroom and docker
# APPENDS arguments, so `docker run mailroom:test /mailroom-admin …` runs
# `/mailroom /mailroom-admin …` — a second SERVER against whatever database the
# environment names — rather than the tool.
#
# RELEASE builds must stamp the version, or the image reports itself as
# 0.0.0-dev and there is no way to tell which release a running instance is
# (#134):
#
#   docker build --build-arg MAILROOM_VERSION=1.0.1 -t mailroom:test .
#   docker run --rm mailroom:test --version            # -> 1.0.1
#
# Note the version check passes NO binary path: ENTRYPOINT is /mailroom and
# `docker run` APPENDS its arguments to it, so naming the binary again would
# make argv "/mailroom /mailroom --version" and the flag would be missed.

# ---------------------------------------------------------------------------
# Stage 1a: build the web client (Vite + TypeScript → static assets).
# ---------------------------------------------------------------------------
# Separate from the Go stage on purpose: it is a different toolchain, it has no
# CGO/musl constraints, and keeping it separate means a web change does not
# invalidate the (expensive) Go+Tantivy layers, or vice versa. `npm ci` needs
# only the two manifest files, so the dependency layer is cached until the
# lockfile itself moves.
#
# `npm run build` = `tsc --noEmit && vite build`: a type error fails the IMAGE
# build, not just a lint job. The web client is a shipped artifact, so it is held
# to the same bar as the Go binaries.
#
# Every FROM below is pinned `tag@sha256:...` (#535): a tag is a pointer
# upstream can repoint whenever it likes, so a rebuild — or a fresh VM standing
# up this image for the first time — must get the exact bytes that were built
# and tested, not whatever `node:24-alpine` currently resolves to. The tag stays
# alongside the digest so this file is still readable, and Dependabot proposes the
# bump when upstream moves the tag rather than the FROM silently drifting.
FROM node:24-alpine@sha256:06d7adf3e0ef61a13f1d28682726366f97a15aa1888c934d3ecec86c33918b53 AS web

WORKDIR /web

COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
RUN npm run build && test -f dist/index.html

# ---------------------------------------------------------------------------
# Stage 1b: build the binaries.
# ---------------------------------------------------------------------------
# ALPINE, not Debian, and that is load-bearing (ADR-006 / docs/search-spike.md):
# the server links the embedded Tantivy search engine through CGO, and
# anyproto/tantivy-go publishes the Linux static library ONLY as a musl build.
# Alpine's gcc links musl natively, so the prebuilt .a drops straight in — no
# Rust toolchain in the image, no cross-compiler, just `apk add gcc musl-dev`
# and a pinned 58 MB artifact. On a glibc base we would additionally need
# x86_64-linux-musl-gcc for no benefit.
FROM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS builder

# TANTIVY_VERSION pins the native library to the same release the Go module is
# pinned to in go.mod. Both must move together.
ARG TANTIVY_VERSION=v1.0.6

# MAILROOM_VERSION is stamped into the server binary as main.version. The
# default is deliberately not a version number — an unstamped build (the CI
# image job, a local `docker build`) must not claim to be a release. Release
# builds pass the tag: --build-arg MAILROOM_VERSION=1.0.1 (#134).
ARG MAILROOM_VERSION=0.0.0-dev
# Extra Go build tags for the SERVER binary. EMPTY by default and never set by a
# release build; only scripts/proxied-images-e2e.sh passes one (#676).
ARG MAILROOM_EXTRA_TAGS=

WORKDIR /src

RUN apk add --no-cache gcc musl-dev curl tar

# The prebuilt Rust static library the CGO binding links against. Fetched, not
# built: no Rust toolchain enters this image (search-spike.md §3).
RUN curl -fsSL "https://github.com/anyproto/tantivy-go/releases/download/${TANTIVY_VERSION}/linux-amd64-musl.tar.gz" \
      | tar -xz -C /usr/local/lib \
 && test -f /usr/local/lib/libtantivy_go.a

# Cache module downloads separately from source changes. The vendored go-imap
# fork (#52, ADR-047; go.mod's replace directive) is a local module, so its
# tree has to be here before `go mod download` can resolve the graph.
COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download

COPY . .

ENV GOOS=linux \
    GOFLAGS=-trimpath

# The SERVER is the only binary that needs the search engine, so it is the only
# one built with CGO and the `tantivy` tag (ADR-006/CLAUDE.md: "CGO reserved for
# the search engine only"). The migrator, the admin CLI and the crypto process
# stay pure-Go static binaries — they have no reason to carry 58 MB of native
# index code, and keeping them CGO-free means a broken search build can never
# block a migration, or the process that holds the PGP keys.
#
# Without -tags tantivy the server still builds and still runs; search then falls
# back to the in-memory backend (internal/search/tantivy/disabled.go). The tag is
# what makes the shipped image use the real engine.
RUN CGO_ENABLED=1 go build -tags "tantivy ${MAILROOM_EXTRA_TAGS}" \
      -ldflags="-s -w -X main.version=${MAILROOM_VERSION}" -o /out/mailroom ./cmd/mailroom \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mailroom-migrate ./cmd/mailroom-migrate \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mailroom-admin ./cmd/mailroom-admin \
 && CGO_ENABLED=0 go build \
      -ldflags="-s -w -X main.version=${MAILROOM_VERSION}" -o /out/mailroom-crypto ./cmd/mailroom-crypto \
 && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/mailroom-census ./cmd/mailroom-census

# ---------------------------------------------------------------------------
# Stage 2: minimal non-root runtime.
# ---------------------------------------------------------------------------
# Alpine rather than distroless/static: it is still minimal and we run it
# non-root and (in deploy/compose.yml) read-only, but it keeps a shell +
# wget/busybox so `docker compose` healthchecks (spec §16 "health probes")
# can exec a check from *inside* the container. Distroless static:nonroot has
# no shell at all, which would leave HEALTHCHECK/compose healthchecks with
# nothing to run — a real trade-off (bigger attack surface than
# distroless/static) accepted here in favor of a working health probe; a
# follow-up could ship a tiny statically-linked probe binary and switch back.
#
# It also has to be musl-based now: the server is a CGO binary linked against
# musl (see stage 1).
FROM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

# libgcc is NOT optional: the Tantivy static library is Rust, and Rust's panic
# machinery pulls in the unwinder (_Unwind_*), which lives in libgcc_s.so.1.
# Without it the server binary does not even start — it dies at dynamic-link time
# with "Error loading shared library libgcc_s.so.1". Nothing short of running the
# final image catches this: the build, the unit/integration suites and even
# `docker build` all pass, because they never execute the linked binary on the
# bare runtime rootfs.
#
# age is here for a consumer OUTSIDE this container: scripts/backup.sh seals
# every backup with age inside a throwaway container (MAILROOM_AGE_IMAGE), and
# a production host should not depend on Alpine's mirror at 03:00, nor get
# whatever `age` version that mirror happens to be serving that night. Baking
# it in here means the version is pinned by THIS image's own digest — the same
# digest deploy/compose.yml already pins — instead of floating on a second,
# unpinned tag (#633). It is in Alpine's community repo, which the official
# alpine image has enabled, so no extra repository config is needed.
RUN apk add --no-cache ca-certificates wget libgcc age \
 && addgroup -S mailroom \
 && adduser -S -G mailroom -H -h /nonexistent -s /sbin/nologin mailroom

# The encrypted mail-blob store (config.DefaultBlobRoot) and the search index
# root (config.DefaultSearchRoot). The server runs non-root and read-only, so
# both paths MUST be volumes — and the directories have to exist in the image,
# owned by the runtime user, before the volumes are attached: Docker initializes
# an empty named volume from the image directory it covers, inheriting its
# ownership and mode. Without this, a fresh volume would land root-owned and the
# non-root server could not write a single message.
#
# The search root is 0700 for the same reason internal/search creates every
# per-user directory 0700: it holds the PLAINTEXT of every message, including
# mail that arrived encrypted (spec §7). Two further deployment requirements
# that this image cannot enforce and the operator must (spec §7/§11):
#   * put the search volume on an at-rest-encrypted filesystem (LUKS/dm-crypt);
#   * do NOT back it up — it is a disposable cache, rebuildable from Postgres.
RUN mkdir -p /var/lib/mailroom/blobs /var/lib/mailroom/search \
 && chown -R mailroom:mailroom /var/lib/mailroom \
 && chmod 700 /var/lib/mailroom/blobs /var/lib/mailroom/search

# The crypto process's socket directory (pgpkeyd.DefaultSocketPath). Same
# reasoning as the two above: deploy/compose.yml shares it between the server
# and the crypto container as a volume, and a fresh named volume inherits the
# ownership and mode of the image directory it covers — so it has to exist here,
# owned by the runtime user, or the socket cannot be created at all.
#
# 0700, and the socket itself is chmod'd 0600 at listen time: WHO MAY TALK TO
# THE PROCESS HOLDING THE PGP KEYS is a kernel decision, not a protocol one.
RUN mkdir -p /run/mailroom-crypto \
 && chown mailroom:mailroom /run/mailroom-crypto \
 && chmod 700 /run/mailroom-crypto

COPY --from=builder /out/mailroom /mailroom
COPY --from=builder /out/mailroom-migrate /mailroom-migrate
COPY --from=builder /out/mailroom-admin /mailroom-admin
COPY --from=builder /out/mailroom-crypto /mailroom-crypto
COPY --from=builder /out/mailroom-census /mailroom-census

# The web client (Forgejo #23), at config.DefaultWebRoot. cmd/mailroom's
# webHandler stats this directory at boot: present → http.FileServer over it,
# behind the auth middleware; absent → "no web client installed" and API only.
#
# root-owned and world-readable ON PURPOSE: the server only ever reads these
# files, and it runs as `mailroom` with a read-only root filesystem
# (deploy/compose.yml). A web root the server could write to would be a
# self-serving-upload hole for no benefit.
COPY --from=web /web/dist /srv/web

USER mailroom:mailroom

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=5 \
  CMD wget -q -O - http://127.0.0.1:8080/readyz || exit 1

ENTRYPOINT ["/mailroom"]
