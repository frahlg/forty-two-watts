#!/usr/bin/env bash
# Build the forty-two-watts Raspberry Pi OS image.
#
# Usage:
#   deploy/pi-gen/build.sh
#
# Runs on any Linux host with Docker — pi-gen shells out to Docker to
# run its build stages in a controlled chroot. macOS works too, but
# you'll need Docker Desktop with a recent enough engine (24+).
#
# Output lands in deploy/pi-gen/pi-gen/deploy/<IMG_NAME>-<date>.img.xz.
# That file is what the CI release job uploads to GitHub Releases.
#
# Env overrides:
#   PI_GEN_REF      pi-gen ref to check out (default: master). Pin to a
#                   tag in CI so image reproducibility doesn't depend
#                   on upstream HEAD moving.
#   FTW_COMPOSE     Override the docker-compose.yml source path (default:
#                   repo root). Useful for testing a compose variant.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

PI_GEN_DIR="${SCRIPT_DIR}/pi-gen"
PI_GEN_REF="${PI_GEN_REF:-master}"

# Sync repo-owned files into the stage's files/ directory. We copy
# rather than symlink because pi-gen's stage runner treats files/ as
# a plain tree and doesn't resolve symlinks pointing outside it.
# Both copies are gitignored; the canonical versions live at the
# repo root.
FILES_DIR="${SCRIPT_DIR}/stage-42w/01-ftw-setup/files"
FTW_COMPOSE="${FTW_COMPOSE:-${REPO_ROOT}/docker-compose.yml}"

install -m 0644 "${FTW_COMPOSE}"                                "${FILES_DIR}/docker-compose.yml"
install -m 0644 "${REPO_ROOT}/mosquitto/config/mosquitto.conf"  "${FILES_DIR}/mosquitto.conf"

if [ ! -d "${PI_GEN_DIR}" ]; then
    git clone --depth 1 --branch "${PI_GEN_REF}" \
        https://github.com/RPi-Distro/pi-gen.git "${PI_GEN_DIR}"
fi

# pi-gen reads stages + config from its own working directory, so wire
# our stage + config in via symlinks. -fn makes this idempotent.
ln -sfn "${SCRIPT_DIR}/stage-42w" "${PI_GEN_DIR}/stage-42w"
ln -sfn "${SCRIPT_DIR}/config"    "${PI_GEN_DIR}/config"

# Skip the desktop stages — we only want Lite + our stage on top.
# pi-gen honours SKIP files dropped into each stage's directory.
for stage in stage3 stage4 stage5; do
    touch "${PI_GEN_DIR}/${stage}/SKIP" \
          "${PI_GEN_DIR}/${stage}/SKIP_IMAGES" 2>/dev/null || true
done

cd "${PI_GEN_DIR}"

# build-docker.sh runs pi-gen inside a privileged Docker container so
# the build doesn't touch the host rootfs and works identically on
# Linux + macOS + GitHub Actions runners.
./build-docker.sh

echo ""
echo "Built image(s):"
ls -lah "${PI_GEN_DIR}/deploy/"*.img.* 2>/dev/null \
    || echo "  (no image files produced — check pi-gen logs above)"
