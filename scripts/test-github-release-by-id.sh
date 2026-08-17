#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temp="$(mktemp -d)"
trap 'rm -rf "${temp}"' EXIT

state="${temp}/asset-present"
delete_calls="${temp}/delete-calls"
upload_calls="${temp}/upload-calls"
printf 'old\n' >"${state}"

mock_gh="${temp}/gh"
cat >"${mock_gh}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ " $* " == *" --method DELETE "* ]]; then
  count=0
  [ ! -f "${MOCK_DELETE_CALLS}" ] || count="$(cat "${MOCK_DELETE_CALLS}")"
  printf '%s\n' "$((count + 1))" >"${MOCK_DELETE_CALLS}"
  rm -f "${MOCK_STATE}"
  exit 1
fi

if [[ " $* " == *" --method POST "* ]]; then
  count=0
  [ ! -f "${MOCK_UPLOAD_CALLS}" ] || count="$(cat "${MOCK_UPLOAD_CALLS}")"
  printf '%s\n' "$((count + 1))" >"${MOCK_UPLOAD_CALLS}"
  printf 'uploaded\n' >"${MOCK_STATE}"
  exit 1
fi

if [ "$(cat "${MOCK_STATE}" 2>/dev/null || true)" = old ]; then
  printf '%s\n' '{"assets":[{"id":456,"name":"ftw-test.tar.gz"}]}'
elif [ "$(cat "${MOCK_STATE}" 2>/dev/null || true)" = uploaded ]; then
  printf '%s\n' '{"assets":[{"id":789,"name":"ftw-test.tar.gz","state":"uploaded","size":1}]}'
else
  printf '%s\n' '{"assets":[]}'
fi
EOF
chmod +x "${mock_gh}"

asset="${temp}/ftw-test.tar.gz"
printf 'x' >"${asset}"

GH_TOKEN=test \
GITHUB_REPOSITORY=srcfl/ftw \
FTW_GH_COMMAND="${mock_gh}" \
FTW_RETRY_SLEEP_SECONDS=0 \
MOCK_STATE="${state}" \
MOCK_DELETE_CALLS="${delete_calls}" \
MOCK_UPLOAD_CALLS="${upload_calls}" \
  "${root}/scripts/github-release-by-id.sh" upload 123 --clobber "${asset}"

test "$(cat "${delete_calls}")" = 1
test "$(cat "${upload_calls}")" = 1
test "$(cat "${state}")" = uploaded

echo "numeric GitHub Release helper checks passed"
