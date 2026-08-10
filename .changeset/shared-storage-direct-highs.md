---
"ftw": patch
---

The optional optimizer now uses highspy instead of CVXPY by default for
eligible shared storage plans, sending a sparse linear model straight to
HiGHS. It keeps the same shared actions, service-first solve, scenario risk
cost and replay check as the CVXPY model. Auto mode uses this path only for
continuous, cycle-safe HIGHS requests with storage alone; commercial
constraints, flexible loads, thermal loads, guarded tariffs and direct-solver
failures stay on CVXPY.
