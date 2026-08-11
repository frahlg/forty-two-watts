---
"ftw": patch
---

The optional optimizer now uses highspy instead of CVXPY by default for
eligible shared storage plans, sending a sparse linear model straight to
HiGHS. It keeps the same shared-action constraints, service-first solve,
scenario risk cost and replay checks as the CVXPY model. It checks meter flow
against the post-curtailment baseline and retries an exact mixed-integer HiGHS
model within the same time budget when a relaxed candidate crosses import and
export. Auto mode uses this path only for continuous, cycle-safe HIGHS requests
with storage alone; commercial constraints, flexible loads, thermal loads,
guarded tariffs and direct-solver failures stay on CVXPY.
