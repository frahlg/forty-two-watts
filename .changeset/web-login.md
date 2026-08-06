---
"ftw": minor
---

Login screen for `api.auth.mode`: a gate overlay that asks `/api/auth/session` on load, removes itself instantly on open-mode sites or live sessions (dashboard untouched), and otherwise blocks the app with a themed sign-in form. Successful login reloads so every component fetches with the session cookie from the start.
