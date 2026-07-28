---
"ftw": patch
---

Clearer optimizer status when the `ftw-optimizer` sidecar is unavailable. The
containerized core ships no Python, so the `auto`/`process` transport can never
start the bundled `python3` worker there. Instead of surfacing a bare
`start optimizer "python3": exec: "python3": executable file not found in $PATH`
— which reads as a missing core dependency and hides the real remedy — core now
reports that the optimizer worker is unavailable and points operators at the
`ftw-optimizer` sidecar, while continuing to plan safely on the built-in Go
planner. Native/all-in-one builds that set `FTW_OPTIMIZER_PYTHON` are unchanged.
