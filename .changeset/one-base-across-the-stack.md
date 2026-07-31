---
"ftw": minor
---

Move the core and updater images to `debian:bookworm-slim`, so the whole deployment runs one base and one libc.

The core runtime was `alpine:3.22` and the updater sidecar was `docker:27-cli`
(also alpine), while the optimizer was already `python:3.12-slim-bookworm`. All
three now share the same Debian rootfs, so a host pulls that layer once instead
of pulling two different userlands, and there is a single security stream to
track rather than musl and glibc side by side.

What the move buys beyond consistency: glibc means the image can run ordinary
prebuilt vendor binaries, which musl cannot; a full userland makes on-site
`docker exec` debugging far easier; and `libnss-mdns` is installed and wired
into `/etc/nsswitch.conf`, so `.local` names resolve for glibc tools inside the
container when an avahi socket is mounted.

The FTW process itself does not depend on that: it resolves `.local` in Go via
`internal/mdnsresolve`, which works regardless of base or libc. NSS is bypassed
entirely by a `CGO_ENABLED=0` binary, so the OS-level resolver is a debugging
and future-compatibility affordance, not the mechanism.

The binary stays fully static and cross-compiled on the build platform. `wget`
is now installed explicitly — it is contractual, because `ftw-updater`
`docker exec`s it inside the core image to decide whether an update commits, and
updaters already deployed in the field will keep doing so. The zoneinfo database
is also embedded in the binary as a fallback so a base image without `tzdata`
can never silently push `time.Local` to UTC and mis-time price and plan windows.

Uncompressed image size grows from about 53 MB to about 128 MB. Data ownership
is unchanged: the process still runs as the bare numeric uid 100 / gid 101, and
gid 101 is still what grants access to the optimizer's socket.
