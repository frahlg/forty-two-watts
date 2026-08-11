---
"ftw": patch
---

Stop progressive hedging after its initial scenario solves when their decisions already meet the configured residual tolerance. This avoids a redundant solver iteration that could exhaust the time limit and discard a valid plan.
