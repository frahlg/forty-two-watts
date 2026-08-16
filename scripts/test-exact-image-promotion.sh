#!/usr/bin/env bash
set -euo pipefail

root="${FTW_RELEASE_TEST_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
beta="${root}/.github/workflows/beta.yml"
release="${root}/.github/workflows/release.yml"
assets="${root}/.github/workflows/release-assets.yml"
compose="${root}/docker-compose.yml"
compose_macos="${root}/docker-compose.macos.yml"
dockerfile="${root}/Dockerfile"

grep -Fq 'VERSION=${{ needs.tag.outputs.runtime_version }}' "${beta}"
grep -Fq 'CANDIDATE_TAG=${{ needs.tag.outputs.version }}' "${beta}"
grep -Fq 'org.opencontainers.image.version=${{ needs.tag.outputs.oci_version }}' "${beta}"
grep -Fq 'created: ${{ steps.version.outputs.created }}' "${beta}"
grep -Fq 'group: beta-release-channel' "${beta}"
grep -Fq 'name: guard immutable beta candidate' "${beta}"
grep -Fq 'receipt_found: ${{ steps.guard.outputs.receipt_found }}' "${beta}"
grep -Fq "if: \${{ needs.candidate.outputs.receipt_found != 'true' }}" "${beta}"
grep -Fq 'core_build: ${{ steps.guard.outputs.core_build }}' "${beta}"
grep -Fq 'updater_build: ${{ steps.guard.outputs.updater_build }}' "${beta}"
grep -Fq 'if: ${{ matrix.build_required == '\''true'\'' }}' "${beta}"
grep -Fq 'Reusing verified unfinished component' "${beta}"
grep -Fq 'Could not determine whether ${TAG} has a digest receipt' "${beta}"
grep -Fq 'already points to ${legacy_digest}; refusing to overwrite it with ${SOURCE_DIGEST}.' "${beta}"
grep -Fq 'name: publish coherent beta aliases' "${beta}"
grep -Fq 'Not moving :beta aliases backwards' "${beta}"
grep -Fq '> ftw-image-digests.json' "${beta}"
grep -Fq 'cmp ftw-image-digests.json existing/ftw-image-digests.json' "${beta}"
grep -Fq '"${source}@${SOURCE_DIGEST}"' "${beta}"
grep -Fq -- '-X main.CandidateTag=${CANDIDATE_TAG}' "${dockerfile}"
grep -Fq 'python3 - "${metadata}" "${GITHUB_SHA}" "${VERSION}"' "${release}"
grep -Fq 'STABLE_COMMIT="$(git rev-list -n 1 "${TAG}")"' "${release}"
grep -Fq '[ "${STABLE_COMMIT}" != "${GITHUB_SHA}" ]' "${release}"
grep -Fq 'source_beta:' "${release}"
grep -Fq 'BETA_TAG="${INPUT_BETA}"' "${release}"
grep -Fq -- '--pattern ftw-image-digests.json' "${release}"
grep -Fq 'current_digest="$(scripts/inspect-image-digest.sh "${image_ref}")"' "${release}"
grep -Fq -- '-f source_beta="${BETA_TAG}"' "${release}"
grep -Fq 'name: promote exact beta manifest' "${assets}"
grep -Fq 'group: release-assets-stable-latest' "${assets}"
grep -Fq -- '--pattern ftw-image-digests.json' "${assets}"
grep -Fq -- '--pattern ftw-promotion-receipt.json' "${assets}"
grep -Fq 'source_beta is required for its first promotion.' "${assets}"
grep -Fq 'is already bound to ${BETA_TAG}, not requested ${INPUT_BETA}.' "${assets}"
grep -Fq 'gh release upload "${TAG}" "${PROMOTION_RECORD}"' "${assets}"
grep -Fq 'gh release download "${STABLE_TAG}"' "${assets}"
grep -Fq 'current_digest="$(scripts/inspect-image-digest.sh "${source}")"' "${assets}"
grep -Fq 'test "${source_digest}" = "${target_digest}"' "${assets}"
grep -Fq '"${source}@${SOURCE_DIGEST}"' "${assets}"
grep -Fq 'FTW_IMAGE_TAG: ${FTW_IMAGE_TAG:-}' "${compose}"
grep -Fq 'FTW_IMAGE_TAG: ${FTW_IMAGE_TAG:-}' "${compose_macos}"

release_checkout="$(grep -n '      - name: Checkout$' "${release}" | cut -d: -f1)"
release_setup_node="$(grep -n '      - name: Setup Node$' "${release}" | cut -d: -f1)"
if ! sed -n "${release_checkout},$((release_setup_node - 1))p" "${release}" | grep -Fq 'persist-credentials: false'; then
  echo "release checkout must not persist a second GitHub auth header" >&2
  exit 1
fi
if [ "$(grep -Fc 'AUTH_HEADER_COUNT="' "${release}")" -ne 2 ]; then
  echo "both release git credential phases must enforce exactly one auth header" >&2
  exit 1
fi

stable_guard="$(grep -n 'STABLE_COMMIT="$(git rev-list -n 1 "${TAG}")"' "${release}" | cut -d: -f1)"
already_done="$(grep -n '# Already-done case:' "${release}" | cut -d: -f1)"
if [ "${stable_guard}" -ge "${already_done}" ]; then
  echo "existing stable tag guard runs too late" >&2
  exit 1
fi

candidate_guard="$(grep -n '^  candidate:$' "${beta}" | cut -d: -f1)"
beta_docker="$(grep -n '^  docker:$' "${beta}" | cut -d: -f1)"
beta_build="$(grep -n 'uses: docker/build-push-action' "${beta}" | cut -d: -f1)"
if [ "${candidate_guard}" -ge "${beta_docker}" ] || [ "${beta_docker}" -ge "${beta_build}" ]; then
  echo "beta immutability guard must complete before any image build" >&2
  exit 1
fi

beta_release="$(grep -n '^  release:$' "${beta}" | cut -d: -f1)"
beta_channel="$(grep -n '^  channel:$' "${beta}" | cut -d: -f1)"
if [ "${beta_release}" -ge "${beta_channel}" ]; then
  echo "moving beta aliases must wait for the digest receipt" >&2
  exit 1
fi
if sed -n "${beta_build},${beta_release}p" "${beta}" | grep -Fq ':beta'; then
  echo "component builds must not move beta aliases before both images complete" >&2
  exit 1
fi

beta_channel_block="$(sed -n "${beta_channel},\$p" "${beta}")"
channel_line() {
  awk -v needle="$1" 'index($0, needle) { print NR; exit }' <<<"${beta_channel_block}"
}
canonical_login="$(channel_line 'name: Login to canonical GHCR namespace')"
canonical_write="$(channel_line 'name: Move canonical aliases after both exact manifests are recorded')"
compatibility_login="$(channel_line 'name: Login to compatibility GHCR namespace')"
compatibility_write="$(channel_line 'name: Mirror beta aliases to compatibility namespace')"
if [ -z "${canonical_login}" ] || [ -z "${canonical_write}" ] || \
   [ -z "${compatibility_login}" ] || [ -z "${compatibility_write}" ]; then
  echo "beta alias publication must have separate canonical and compatibility phases" >&2
  exit 1
fi
if [ "${canonical_login}" -ge "${canonical_write}" ] || \
   [ "${canonical_write}" -ge "${compatibility_login}" ] || \
   [ "${compatibility_login}" -ge "${compatibility_write}" ]; then
  echo "each beta alias write must run under its namespace credential" >&2
  exit 1
fi
canonical_block="$(sed -n "${canonical_write},$((compatibility_login - 1))p" <<<"${beta_channel_block}")"
if ! grep -Fq -- '--tag "${canonical}:beta"' <<<"${canonical_block}" || \
   grep -Fq -- '--tag "${compatibility}:beta"' <<<"${canonical_block}"; then
  echo "canonical beta aliases must move before the compatibility login" >&2
  exit 1
fi
compatibility_block="$(sed -n "${compatibility_write},\$p" <<<"${beta_channel_block}")"
if ! grep -Fq -- '--tag "${compatibility}:beta"' <<<"${compatibility_block}" || \
   ! grep -Fq '"${canonical_version}@${expected}"' <<<"${compatibility_block}"; then
  echo "compatibility beta aliases must copy the exact canonical manifests" >&2
  exit 1
fi

promotion_upload="$(grep -n 'gh release upload "${TAG}" "${PROMOTION_RECORD}"' "${assets}" | cut -d: -f1)"
stable_docker="$(grep -n '^  docker:$' "${assets}" | cut -d: -f1)"
if [ "${promotion_upload}" -ge "${stable_docker}" ]; then
  echo "stable promotion receipt must be bound before alias writes" >&2
  exit 1
fi
if grep -Fq 'git tag --list "${TAG}-beta.*"' "${assets}"; then
  echo "stable asset reruns must not auto-select a newer beta" >&2
  exit 1
fi
if grep -Fq 'git tag --list "${TAG}-beta.*"' "${release}"; then
  echo "stable release must use the beta selected by the operator" >&2
  exit 1
fi

trigger_start="$(grep -n '^on:$' "${assets}" | cut -d: -f1)"
permissions_start="$(grep -n '^permissions:$' "${assets}" | cut -d: -f1)"
if sed -n "${trigger_start},$((permissions_start - 1))p" "${assets}" | grep -q '^  push:'; then
  echo "stable release assets must not run from an unbound tag push" >&2
  exit 1
fi

docker_start="$(grep -n '^  docker:$' "${assets}" | cut -d: -f1)"
discord_start="$(grep -n '^  discord:$' "${assets}" | cut -d: -f1)"
if sed -n "${docker_start},$((discord_start - 1))p" "${assets}" | grep -q 'docker/build-push-action'; then
  echo "stable docker job still rebuilds an image" >&2
  exit 1
fi

echo "exact image promotion workflow checks passed"
