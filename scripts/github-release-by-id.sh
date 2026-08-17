#!/usr/bin/env bash
# Read and mutate a GitHub Release by numeric id.
#
# `gh release` finds drafts through a pending-tag GraphQL query. GitHub Actions
# tokens cannot use that query reliably across workflow runs. The REST release
# id is stable and supports the same reads, asset operations and publication.

set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"

gh_command="${FTW_GH_COMMAND:-gh}"
retry_sleep_seconds="${FTW_RETRY_SLEEP_SECONDS:-2}"

valid_release_id() {
  [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

retry_gh() {
  local attempt=1
  local output
  output="$(mktemp)"
  while [ "${attempt}" -le 5 ]; do
    if "${gh_command}" "$@" > "${output}"; then
      cat "${output}"
      rm -f "${output}"
      return 0
    fi
    if [ "${attempt}" -lt 5 ]; then
      sleep $((attempt * retry_sleep_seconds))
    fi
    attempt=$((attempt + 1))
  done
  rm -f "${output}"
  return 1
}

show_release() {
  local release_id="$1"
  valid_release_id "${release_id}"
  retry_gh api "repos/${GITHUB_REPOSITORY}/releases/${release_id}"
}

delete_asset() {
  local release_id="$1"
  local asset_id="$2"
  local attempt=1
  local release_json
  valid_release_id "${release_id}"
  valid_release_id "${asset_id}"

  while [ "${attempt}" -le 5 ]; do
    if "${gh_command}" api --method DELETE \
      "repos/${GITHUB_REPOSITORY}/releases/assets/${asset_id}" >/dev/null; then
      return 0
    fi

    # DELETE is not idempotent. If GitHub accepted it but the client lost the
    # response, a retry returns 404. Re-read the bound release and accept an
    # already absent asset instead of failing the whole recovery.
    if release_json="$(show_release "${release_id}")" &&
       jq -e '.assets | type == "array"' <<<"${release_json}" >/dev/null &&
       ! jq -e --arg id "${asset_id}" \
         '.assets[] | select((.id | tostring) == $id)' \
         <<<"${release_json}" >/dev/null; then
      return 0
    fi

    if [ "${attempt}" -lt 5 ]; then
      sleep $((attempt * retry_sleep_seconds))
    fi
    attempt=$((attempt + 1))
  done
  return 1
}

download_assets() {
  local release_id="$1"
  local destination="$2"
  local pattern="${3:-}"
  local release_json matches
  valid_release_id "${release_id}"
  mkdir -p "${destination}"
  release_json="$(show_release "${release_id}")"
  matches="$(jq -r --arg pattern "${pattern}" '
    .assets[]
    | select($pattern == "" or .name == $pattern)
    | [.id, .name] | @tsv
  ' <<<"${release_json}")"
  if [ -z "${matches}" ]; then
    echo "Release ${release_id} has no matching asset ${pattern:-<all>}." >&2
    return 1
  fi
  while IFS=$'\t' read -r asset_id name; do
    if [[ ! "${asset_id}" =~ ^[1-9][0-9]*$ ]] || \
       [ -z "${name}" ] || [[ "${name}" == */* ]]; then
      echo "Release ${release_id} contains an unsafe asset record." >&2
      return 1
    fi
    temp="${destination}/.${name}.partial"
    retry_gh api \
      -H 'Accept: application/octet-stream' \
      "repos/${GITHUB_REPOSITORY}/releases/assets/${asset_id}" > "${temp}"
    mv "${temp}" "${destination}/${name}"
  done <<<"${matches}"
}

upload_assets() {
  local release_id="$1"
  local clobber="$2"
  shift 2
  local file name encoded release_json existing upload_json
  valid_release_id "${release_id}"
  if [ "$#" -eq 0 ]; then
    echo "At least one asset path is required." >&2
    return 1
  fi
  for file in "$@"; do
    if [ ! -s "${file}" ]; then
      echo "Release asset ${file} is missing or empty." >&2
      return 1
    fi
    name="$(basename "${file}")"
    release_json="$(show_release "${release_id}")"
    existing="$(jq -r --arg name "${name}" '.assets[] | select(.name == $name) | .id' <<<"${release_json}")"
    if [ -n "${existing}" ] && [ "${clobber}" != "true" ]; then
      echo "Release asset ${name} already exists." >&2
      return 1
    fi
    while IFS= read -r asset_id; do
      [ -z "${asset_id}" ] && continue
      [[ "${asset_id}" =~ ^[1-9][0-9]*$ ]]
      delete_asset "${release_id}" "${asset_id}"
    done <<<"${existing}"
    encoded="$(jq -rn --arg value "${name}" '$value | @uri')"
    upload_json="$(retry_gh api --method POST \
      --header 'Content-Type: application/octet-stream' \
      --input "${file}" \
      "https://uploads.github.com/repos/${GITHUB_REPOSITORY}/releases/${release_id}/assets?name=${encoded}")"
    jq -e --arg name "${name}" '
      .name == $name and .state == "uploaded" and .size > 0
    ' <<<"${upload_json}" >/dev/null
  done
}

publish_release() {
  local release_id="$1"
  valid_release_id "${release_id}"
  retry_gh api --method PATCH \
    -F draft=false \
    -F prerelease=false \
    -f make_latest=true \
    "repos/${GITHUB_REPOSITORY}/releases/${release_id}"
}

command="${1:-}"
case "${command}" in
  show)
    [ "$#" -eq 2 ]
    show_release "$2"
    ;;
  download)
    [ "$#" -ge 3 ] && [ "$#" -le 4 ]
    download_assets "$2" "$3" "${4:-}"
    ;;
  upload)
    [ "$#" -ge 3 ]
    release_id="$2"
    clobber=false
    shift 2
    if [ "${1:-}" = "--clobber" ]; then
      clobber=true
      shift
    fi
    upload_assets "${release_id}" "${clobber}" "$@"
    ;;
  publish)
    [ "$#" -eq 2 ]
    publish_release "$2"
    ;;
  *)
    echo "usage: $0 {show ID|download ID DIR [NAME]|upload ID [--clobber] FILE...|publish ID}" >&2
    exit 2
    ;;
esac
