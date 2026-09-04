---
"ftw": patch
---

The Python shadow's verdict is now Core's price for both plans. Core walks
the challenger's own action sequence through its forward pass and costs it
with the same arithmetic the DP uses on itself, instead of reading the total
the challenger arrived with — and it does the same for its own plan, so the
subtraction stays symmetric if the DP's bookkeeping ever moves. The
difference keeps the key `python_minus_core_ore_terminal_corrected`.

What the challenger reported is kept beside the verdict rather than inside
it: `self_reported_ore` is the cost it claimed for its own plan, and
`self_reported_objective_ore` is the value it actually minimized —
scenario-weighted and CVaR-shaped, so not comparable with any cost. On the
owner's box those two differ by 450 öre on a single plan.

A plan Core cannot cost honestly — wrong length, non-finite output, a
battery driven outside its operating band or past its power limits — is
recorded as `evaluation_refused_reason` with no difference number at all.
Core costing its own plan differently from what it reported is a bug in
Core, and now says so with a warning and `active_evaluation_drift_ore`.

The replay bench measures the same way, prints the challenger's own figure
in its own column, and takes `FTW_MPC_BENCH_CVAR_WEIGHT`,
`FTW_MPC_BENCH_CVAR_ALPHA` and `FTW_MPC_BENCH_MIP_GAP`. Those knobs matter:
a snapshot records the planning inputs but not the site's solver settings,
so replaying one with the bench's defaults asks the challenger a different
question than the box asked — worth more than 130 öre per plan on a summer
day with PV uncertainty, which is larger than the gap being measured.

A second bench asks what the price twin is worth. It re-solves each snapshot
three ways — the confidences the box used, those same forecast slots
flattened to the horizon mean, and the forecast slots deleted — and reports
the only number that reaches hardware: the first slot's battery power. On
every snapshot recorded so far the three agree exactly, and they keep
agreeing until the known window falls under four hours.
