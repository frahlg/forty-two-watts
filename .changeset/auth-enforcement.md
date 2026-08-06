---
"ftw": minor
---

API authentication modes and a mutation audit trail. New `api.auth.mode`: `open` (default — exactly today's behavior), `local_trust` (LAN clients unchanged; remote requests need a login session: viewer to read, operator to mutate; the `FTW_API_TOKEN` bearer path keeps working for automation), and `required` (every API request needs a login, local included; login/health/static assets stay reachable). Accounts are managed on the box with the new `ftw user` subcommand (add/list/passwd/disable/enable/delete; argon2id; refuses to remove the last enabled operator while a login mode is active, and startup refuses non-open modes with zero operators — a typo can never lock you out). Every mutation attempt is recorded to a new `audit_log` table with its principal (username, token, or local) and exposed at `GET /api/audit`.
