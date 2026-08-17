#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: check-ghcr-write-access.sh OWNER/PACKAGE [...]" >&2
  exit 2
fi
if [ -z "${GHCR_USERNAME:-}" ] || [ -z "${GHCR_TOKEN:-}" ]; then
  echo "GHCR_USERNAME and GHCR_TOKEN are required" >&2
  exit 2
fi

curl_command="${FTW_CURL:-curl}"
headers="$(mktemp)"
upload_location=""
bearer=""
cleanup() {
  if [ -n "${upload_location}" ] && [ -n "${bearer}" ]; then
    "${curl_command}" --silent --output /dev/null --request DELETE \
      --header "Authorization: Bearer ${bearer}" \
      "${upload_location}" || true
  fi
  rm -f "${headers}"
}
trap cleanup EXIT

capture_upload_location() {
  local location
  upload_location=""
  location="$(awk '
    tolower($0) ~ /^location:/ {
      sub(/^[^:]*:[[:space:]]*/, "")
      sub(/\r$/, "")
      value = $0
    }
    END { print value }
  ' "${headers}")"
  case "${location}" in
    /v2/*) upload_location="https://ghcr.io${location}" ;;
    https://ghcr.io/v2/*) upload_location="${location}" ;;
    "") return 1 ;;
    *)
      echo "GHCR write probe returned an unsafe upload location: ${location}" >&2
      return 2
      ;;
  esac
}

for repository in "$@"; do
  if [[ ! "${repository}" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
    echo "invalid GHCR repository: ${repository}" >&2
    exit 2
  fi

  bearer="$({
    "${curl_command}" --fail-with-body --silent --show-error --get \
      --user "${GHCR_USERNAME}:${GHCR_TOKEN}" \
      --data-urlencode "service=ghcr.io" \
      --data-urlencode "scope=repository:${repository}:pull,push" \
      https://ghcr.io/token
  } | jq -er '.token // .access_token // empty')"

  : > "${headers}"
  status=""
  if ! status="$(
    "${curl_command}" --silent --show-error --output /dev/null \
      --dump-header "${headers}" --write-out '%{http_code}' \
      --request POST \
      --header "Authorization: Bearer ${bearer}" \
      --header 'Content-Length: 0' \
      "https://ghcr.io/v2/${repository}/blobs/uploads/"
  )"; then
    # A proxy or connection can fail after GHCR accepted the POST. Recover a
    # safe Location from received headers so the EXIT trap can still cancel it.
    capture_upload_location || true
    echo "GHCR write probe transport failed for ${repository}" >&2
    exit 1
  fi
  if [ "${status}" != "202" ]; then
    echo "GHCR write probe failed for ${repository} (HTTP ${status})" >&2
    exit 1
  fi

  if ! capture_upload_location; then
    upload_location=""
    echo "GHCR write probe returned no safe upload location for ${repository}" >&2
    exit 1
  fi

  cleanup_status="$(
    "${curl_command}" --silent --show-error --output /dev/null --write-out '%{http_code}' \
      --request DELETE \
      --header "Authorization: Bearer ${bearer}" \
      "${upload_location}"
  )"
  case "${cleanup_status}" in
    204)
      ;;
    405)
      # GHCR accepts the empty upload start but does not currently implement
      # the Distribution API's upload-cancel operation. No blob, manifest,
      # package version or tag exists until an upload is finalized.
      ;;
    *)
      echo "GHCR write probe cleanup failed for ${repository} (HTTP ${cleanup_status})" >&2
      exit 1
      ;;
  esac
  upload_location=""
done
