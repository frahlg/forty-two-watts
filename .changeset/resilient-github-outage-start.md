---
"ftw": patch
---

Startup no longer hard-fails when GitHub/GHCR is unreachable. `docker compose pull` fetches images from ghcr.io (GitHub Container Registry), and every start path gated `up` on it: `scripts/install.sh` (under `set -e`) aborted before `up -d`, and the Raspberry Pi first-boot provisioner retried the pull forever and never reached `up -d`. During a GitHub outage that left the box unable to start.

The pull is now best-effort and the stack starts from the locally-present last-known-good images instead: install falls through to `up -d` after a failed pull, and first-boot brings the stack up on local images first, only reaching GHCR for a genuinely missing image. A truly fresh host with no local image still needs GHCR reachable — pre-baking images into the OS image is the follow-up for that.
