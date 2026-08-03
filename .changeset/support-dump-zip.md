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

The report also now carries the slot's energy books — what the plan asked for,
what the batteries actually moved, and what the energy-allocation path thinks
it delivered — plus a finding when a slot is a quarter of the way through and
delivery is under half the rate the plan needs. That is the shape of the
reports that keep arriving: a plan card reading "charge 4.5 kW, now", a live
target of 0 W, and nothing in between to show whether the plan reached
dispatch at all.
