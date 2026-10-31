#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
#
# Print the CHANGELOG.md section for one version (heading excluded), for use as
# a release body. Fails if the version has no heading or its section is empty.
#
#   sh scripts/changelog-section.sh 1.98.0 > notes.md
set -eu
V="${1:?usage: changelog-section.sh VERSION (no leading v)}"
FILE="${CHANGELOG:-CHANGELOG.md}"
OUT="$(awk -v v="$V" '
  /^## \[/ { if (on) exit; on = (index($0, "## [" v "]") == 1); next }
  on { print }
' "$FILE")"
if ! grep -qF "## [$V]" "$FILE"; then
  echo "error: $FILE has no '## [$V]' heading" >&2
  exit 1
fi
if [ -z "$(printf '%s' "$OUT" | tr -d '[:space:]')" ]; then
  echo "error: the '## [$V]' section of $FILE is empty" >&2
  exit 1
fi
printf '%s\n' "$OUT"
