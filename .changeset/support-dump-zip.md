---
"ftw": patch
---

One button, one file when asking for help. The plan card's "Something looks
wrong?" button now downloads `ftw-help-<stamp>.zip` — the help report as
`00-help-report.md`, sorted first, with the redacted config, driver health,
recent logs and an hour of telemetry behind it.

Before this there were two downloads and the user had to guess which one we
wanted: the report from the plan card, the log bundle from a driver's Diagnose
modal. They would send one and we would ask for the other.

The archive is a zip rather than a `.tar.gz` because its whole purpose is to be
handed to somebody else, and Windows and every chat client open a zip without a
second tool. Around 10 kB on a two-driver install.

`GET /api/support/report` still returns the bare Markdown for anyone who wants
only the text.
