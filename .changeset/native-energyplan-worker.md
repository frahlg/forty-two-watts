---
"ftw": minor
---

Bundle the optional proprietary Sourceful Energyplan worker for Linux ARM64,
Linux AMD64 and macOS ARM64. Verify the compiled workers and their licenses
through a pinned checksum manifest. Core validates their proposed plans and
retains its existing fallback. Source and builds remain in a private repository;
FTW needs no Rust toolchain or private repository access to use the bundle.
