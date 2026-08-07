---
"ftw": patch
---

Notice when a new Debian stable leaves the container base behind.

The images pin a codename (`debian:trixie-slim`, `python:3.12-slim-trixie`)
rather than `stable`, so a major-version jump can never arrive silently on a
rebuild. Nothing noticed when a new stable shipped, and Dependabot cannot: it
orders numeric tags, and a suite codename has no numeric component to order.

A weekly check now reads the pin out of the three Dockerfiles and compares it
with Debian's own `stable` release, failing when the pin falls behind or when
the three images stop agreeing on one suite.

No runtime behaviour changes.
