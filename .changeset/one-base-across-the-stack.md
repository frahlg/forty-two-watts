---
"ftw": minor
---

Move the whole container stack to one base: Debian 13 "trixie" slim.

Core was `alpine:3.22`, the updater sidecar was `docker:27-cli` (also alpine),
and the optimizer was `python:3.12-slim-bookworm` — two libcs, three unrelated
base images and three security streams in one deployment. Core, updater and
optimizer now all sit on `debian:trixie-slim`, verified to share a single base
layer, so a host pulls that rootfs once and there is one suite to track. It also
matches the Raspberry Pi OS release the SD image is built from.

What the move buys: glibc, so the image can run ordinary prebuilt vendor
binaries, which musl cannot; a full userland, which makes `docker exec`
debugging on a live site practical; and `libnss-mdns` wired into
`/etc/nsswitch.conf`, so `.local` names resolve for glibc tools inside the
container — `getent hosts zap.local`, `curl`, `wget` — once an avahi socket is
mounted. Alpine has no NSS plugin mechanism at all, so none of that was
available before.

That covers tools in the image, not the FTW process itself: the binary is built
`CGO_ENABLED=0` and so never consults NSS. It stays fully static and still
cross-compiles on the build platform.

`wget` is now installed explicitly and asserted by the container boundary test.
It is contractual rather than incidental: `ftw-updater` `docker exec`s it inside
the core image to decide whether an update commits, and updaters already
deployed in the field will keep doing so. The zoneinfo database is also embedded
in the binary as a fallback, so a base image without `tzdata` can never silently
push `time.Local` to UTC and mis-time price and plan windows.

Size: the core image grows from about 53 MB to about 133 MB uncompressed. The
updater is unchanged at about 203 MB — `docker:27-cli` was never a small image —
and now shares its base with the other two, so the per-host download is well
below the nominal sum.

Data ownership is unchanged: the process still runs as the bare numeric uid 100
/ gid 101, and gid 101 is still what grants access to the optimizer's socket.

The optimizer is versioned independently and moves to 1.4.0, because its image
is a materially different artifact once the base changes. Its release workflow
verifies that a published image's revision label matches the commit it claims,
so the new base could not have shipped under the old version number at all.

The base is pinned to the `trixie` codename rather than a `stable` alias so a
major-version jump can never arrive silently on a rebuild. A new scheduled
`debian base currency` workflow watches for a newer Debian stable and opens a
tracking issue when one appears.
