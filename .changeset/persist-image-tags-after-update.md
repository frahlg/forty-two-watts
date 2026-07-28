---
"ftw": patch
---

An update now survives the next `docker compose up -d`. Compose resolves `${FTW_IMAGE_TAG:-latest}` fresh every time, and nothing recorded the tag an update installed — so a reboot, adding a service, or any routine compose command on the host silently moved the site back to newest stable. A beta tester lost their beta without touching anything. The updater now records the installed Core and sidecar tags in the project's `.env`, rewriting those two keys in place and leaving every comment, blank line and unrelated setting exactly as it found them. An `.env` that cannot be read is left alone and the update still completes.
