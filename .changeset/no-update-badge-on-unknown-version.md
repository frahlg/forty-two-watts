---
"ftw": patch
---

The amber update badge no longer appears on a site already running the newest release. The badge counts Core, Optimizer and driver updates together, and the Optimizer was claiming one it could not know about: when its version was never learned — the handshake was rejected, or it had not finished starting when Core looked — Core recorded it as `dev`, which the release comparison read as older than every published version. An unknown version now means exactly that, and claims nothing until a handshake reports what is actually running. An unstamped local build still reports a real version, so testing the update flow on a dev machine is unaffected.
