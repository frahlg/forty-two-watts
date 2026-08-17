#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
probe="${root}/scripts/check-ghcr-write-access.sh"
test_tmp="$(mktemp -d)"
trap 'rm -rf "${test_tmp}"' EXIT

mock_curl="${test_tmp}/curl"
cat > "${mock_curl}" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

request=GET
headers=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --request)
      request="$2"
      shift 2
      ;;
    --dump-header)
      headers="$2"
      shift 2
      ;;
    --data-urlencode)
      printf 'scope %s\n' "$2" >> "${FTW_MOCK_CURL_LOG}"
      shift 2
      ;;
    --user)
      printf 'authenticated\n' >> "${FTW_MOCK_CURL_LOG}"
      shift 2
      ;;
    --header|--output|--write-out)
      shift 2
      ;;
    --fail-with-body|--silent|--show-error|--get)
      shift
      ;;
    *)
      url="$1"
      shift
      ;;
  esac
done

case "${url}" in
  https://ghcr.io/token)
    printf '{"token":"probe-token"}\n'
    ;;
  https://ghcr.io/v2/*/blobs/uploads/)
    printf '%s %s\n' "${request}" "${url}" >> "${FTW_MOCK_CURL_LOG}"
    if [ "${FTW_MOCK_CURL_MODE:-success}" = deny ]; then
      printf '403'
      exit 0
    fi
    if [ "${FTW_MOCK_CURL_MODE:-success}" = unsafe-location ]; then
      printf 'HTTP/1.1 202 Accepted\r\nLocation: https://example.com/upload\r\n\r\n' > "${headers}"
    else
      printf 'HTTP/1.1 202 Accepted\r\nLocation: /v2/test/package/blobs/uploads/probe\r\n\r\n' > "${headers}"
    fi
    printf '202'
    if [ "${FTW_MOCK_CURL_MODE:-success}" = accepted-then-transport-error ]; then
      exit 56
    fi
    ;;
  https://ghcr.io/v2/*/blobs/uploads/*)
    printf '%s %s\n' "${request}" "${url}" >> "${FTW_MOCK_CURL_LOG}"
    case "${FTW_MOCK_CURL_MODE:-success}" in
      cleanup-not-supported) printf '405' ;;
      cleanup-failure) printf '500' ;;
      *) printf '204' ;;
    esac
    ;;
  *)
    printf 'unexpected mock curl URL: %s\n' "${url}" >&2
    exit 1
    ;;
esac
MOCK
chmod +x "${mock_curl}"

mock_log="${test_tmp}/curl.log"
: > "${mock_log}"
FTW_CURL="${mock_curl}" FTW_MOCK_CURL_LOG="${mock_log}" \
  GHCR_USERNAME=test GHCR_TOKEN=secret \
  bash "${probe}" srcfl/ftw srcfl/ftw-updater

grep -Fq 'scope scope=repository:srcfl/ftw:pull,push' "${mock_log}"
grep -Fq 'scope scope=repository:srcfl/ftw-updater:pull,push' "${mock_log}"
if [ "$(grep -c '^POST ' "${mock_log}")" -ne 2 ] || \
   [ "$(grep -c '^DELETE ' "${mock_log}")" -ne 2 ]; then
  echo "each package write probe must start and cancel one empty upload" >&2
  exit 1
fi

: > "${mock_log}"
FTW_CURL="${mock_curl}" FTW_MOCK_CURL_LOG="${mock_log}" \
  FTW_MOCK_CURL_MODE=cleanup-not-supported \
  GHCR_USERNAME=test GHCR_TOKEN=secret \
  bash "${probe}" srcfl/ftw
grep -Fq 'POST https://ghcr.io/v2/srcfl/ftw/blobs/uploads/' "${mock_log}"
grep -Fq 'DELETE https://ghcr.io/v2/test/package/blobs/uploads/probe' "${mock_log}"

if FTW_CURL="${mock_curl}" FTW_MOCK_CURL_LOG="${mock_log}" \
  FTW_MOCK_CURL_MODE=cleanup-failure \
  GHCR_USERNAME=test GHCR_TOKEN=secret \
  bash "${probe}" srcfl/ftw >/dev/null 2>&1; then
  echo "an unexpected upload cleanup failure passed" >&2
  exit 1
fi

: > "${mock_log}"
if FTW_CURL="${mock_curl}" FTW_MOCK_CURL_LOG="${mock_log}" \
  FTW_MOCK_CURL_MODE=accepted-then-transport-error \
  GHCR_USERNAME=test GHCR_TOKEN=secret \
  bash "${probe}" srcfl/ftw >/dev/null 2>&1; then
  echo "a transport error after an accepted upload passed" >&2
  exit 1
fi
grep -Fq 'POST https://ghcr.io/v2/srcfl/ftw/blobs/uploads/' "${mock_log}"
grep -Fq 'DELETE https://ghcr.io/v2/test/package/blobs/uploads/probe' "${mock_log}"

if FTW_CURL="${mock_curl}" FTW_MOCK_CURL_LOG="${mock_log}" FTW_MOCK_CURL_MODE=deny \
  GHCR_USERNAME=test GHCR_TOKEN=secret \
  bash "${probe}" srcfl/ftw 2>/dev/null; then
  echo "a denied package write probe passed" >&2
  exit 1
fi

: > "${mock_log}"
if FTW_CURL="${mock_curl}" FTW_MOCK_CURL_LOG="${mock_log}" FTW_MOCK_CURL_MODE=unsafe-location \
  GHCR_USERNAME=test GHCR_TOKEN=secret \
  bash "${probe}" srcfl/ftw 2>/dev/null; then
  echo "an unsafe upload cleanup location passed" >&2
  exit 1
fi
if grep -Fq 'example.com' "${mock_log}"; then
  echo "the registry bearer token would be sent to an untrusted upload host" >&2
  exit 1
fi

echo "GHCR package write probe checks passed"
