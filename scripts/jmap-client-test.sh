#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# A real third-party JMAP client (go-jmap, the library aerc is built on) against
# the real server: session discovery, Mailbox/Email/Thread reads, /changes
# across a change, and a draft create, with an API key sent as a bearer token.
# See internal/api/jmapclient_integration_test.go for what is checked and why,
# and internal/api/testdata/jmapclient-known-gaps.txt for the non-conformances
# that are allowed to fail today (anything else failing is a regression).
#
# It reuses scripts/integration-test.sh (throwaway PostgreSQL 18, schema applied
# as the owner, Docker API only) and narrows it to one package and one test, so
# the job is the cost of one package build and a handful of requests rather than
# the whole integration suite. The `jmapclient` build tag is what brings the
# client library in; no other lane compiles it.
set -eu
DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
MAILROOM_IT_TAGS="integration,jmapclient" \
MAILROOM_IT_PKGS="./internal/api/" \
GOTESTFLAGS="-run ^TestJMAPClientConformance$ -v" \
  sh "$DIR/scripts/integration-test.sh"
echo "JMAP client conformance passed"
