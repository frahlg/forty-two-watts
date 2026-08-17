#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temp="$(mktemp -d)"
trap 'rm -rf "${temp}"' EXIT

state="${temp}/asset-present"
delete_calls="${temp}/delete-calls"
touch "${state}"

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
  printf '%s\n' '{"name":"ftw-test.tar.gz","state":"uploaded","size":1}'
  exit 0
fi

if [ -f "${MOCK_STATE}" ]; then
  printf '%s\n' '{"assets":[{"id":456,"name":"ftw-test.tar.gz"}]}'
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
  "${root}/scripts/github-release-by-id.sh" upload 123 --clobber "${asset}"

test "$(cat "${delete_calls}")" = 1
test ! -e "${state}"

echo "numeric GitHub Release helper checks passed"
