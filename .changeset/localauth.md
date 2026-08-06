---
"ftw": minor
---

Local user accounts foundation for API authentication: a `users` table (operator/viewer roles, argon2id password hashes in PHC format) and an in-memory session layer with 24 h expiry, per-user revocation, and constant-time verification. Sessions are deliberately memory-only — a restart logs everyone out, the safe failure for a control system, and no session secret ever touches the database. This release adds the packages and schema; API enforcement (`api.auth.mode`) lands separately and nothing changes for existing installs.
