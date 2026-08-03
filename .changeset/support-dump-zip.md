---
"ftw": patch
---

Support dump is now a `.zip` instead of a `.tar.gz`. The file's whole purpose
is to be handed to somebody else, and Windows and every chat client open a zip
without a second tool — a `.tar.gz` asks the person you need help from to go
find one first. Contents and size are unchanged: manifest, redacted config,
driver health, recent logs and an hour of telemetry, around 6 kB on a
two-driver install.
