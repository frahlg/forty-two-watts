#!/usr/bin/env bash
# Regenerate drivers/ from the device-drivers commit pinned in
# drivers/BUNDLED_SOURCE.json.
#
# The bundled drivers are a recovery snapshot, not a source. Startup is
# deliberately local -- a gateway boots and runs offline, and a remote refresh
# must never block it -- so the files have to be in the image. But keeping them
# by hand made this a second source of truth, and it drifted: a fix that landed
# in device-drivers left the bundled copy behind, still carrying the bug.
#
# Usage:
#   scripts/sync-bundled-drivers.sh            # write the snapshot
#   scripts/sync-bundled-drivers.sh --check    # fail if drivers/ has drifted
#
# To take a newer driver: move the commit in BUNDLED_SOURCE.json, run this, and
# commit the result. To add or drop a bundled driver, edit the list there --
# which drivers are bundled is FTW's decision, and device-drivers publishes
# more than FTW needs to carry for recovery.

set -euo pipefail

CHECK=0
[ "${1:-}" = "--check" ] && CHECK=1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIN="${ROOT}/drivers/BUNDLED_SOURCE.json"
DEST="${ROOT}/drivers"

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
[ -f "$PIN" ] || { echo "missing ${PIN}" >&2; exit 2; }

REPOSITORY="$(jq -r .repository "$PIN")"
COMMIT="$(jq -r .commit "$PIN")"
SOURCE_DIR="$(jq -r .source_dir "$PIN")"
# Read into an array the long way: the bash that ships with macOS has no
# mapfile, and this script has to run the same locally and in CI.
DRIVERS=()
while IFS= read -r line; do
  [ -n "$line" ] && DRIVERS+=("$line")
done < <(jq -r '.drivers[]' "$PIN")

case "$COMMIT" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
  *) echo "commit must be a full hex sha, got '${COMMIT}'" >&2; exit 2 ;;
esac

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "fetching ${REPOSITORY} at ${COMMIT:0:12}"
# A tarball of one commit rather than a clone: no history, no refs, and the
# commit cannot move under us the way a branch name could.
curl -fsSL "https://codeload.github.com/${REPOSITORY}/tar.gz/${COMMIT}" \
  | tar -xz -C "${WORK}"

SRC="$(find "${WORK}" -maxdepth 1 -mindepth 1 -type d | head -1)/${SOURCE_DIR}"
[ -d "$SRC" ] || { echo "no ${SOURCE_DIR} at that commit" >&2; exit 1; }

drift=0
missing=0
for id in "${DRIVERS[@]}"; do
  from="${SRC}/${id}.lua"
  to="${DEST}/${id}.lua"
  if [ ! -f "$from" ]; then
    echo "MISSING upstream: ${id}.lua is not in ${REPOSITORY} at this commit" >&2
    missing=1
    continue
  fi
  if [ "$CHECK" = "1" ]; then
    if ! cmp -s "$from" "$to"; then
      echo "DRIFTED ${id}.lua"
      drift=1
    fi
  else
    cp "$from" "$to"
  fi
done

# A .lua in drivers/ that the pin does not list is a file nothing regenerates,
# which is how the second source of truth grew in the first place.
for path in "${DEST}"/*.lua; do
  [ -e "$path" ] || continue
  id="$(basename "$path" .lua)"
  listed=0
  for known in "${DRIVERS[@]}"; do
    [ "$id" = "$known" ] && { listed=1; break; }
  done
  if [ "$listed" = "0" ]; then
    echo "UNLISTED ${id}.lua is in drivers/ but not in BUNDLED_SOURCE.json" >&2
    drift=1
  fi
done

if [ "$missing" = "1" ]; then
  echo "the pin names drivers that do not exist upstream" >&2
  exit 1
fi

if [ "$CHECK" = "1" ]; then
  if [ "$drift" = "1" ]; then
    cat >&2 <<'MSG'

drivers/ no longer matches the pinned device-drivers commit.

These files are generated. Fix the driver in srcfl/device-drivers, move the
commit in drivers/BUNDLED_SOURCE.json, run scripts/sync-bundled-drivers.sh and
commit the result. Editing them here makes this a second source, and the copies
then drift apart silently.
MSG
    exit 1
  fi
  echo "drivers/ matches ${REPOSITORY}@${COMMIT:0:12} (${#DRIVERS[@]} drivers)"
else
  echo "wrote ${#DRIVERS[@]} drivers from ${REPOSITORY}@${COMMIT:0:12}"
fi
