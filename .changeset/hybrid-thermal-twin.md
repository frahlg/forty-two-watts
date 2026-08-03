---
"ftw": minor
---

Add versioned home specifications, calibrated 1R1C and 2R2C thermal models,
matching Modelica/FMI references, and read-only thermal backtesting. Let Core
load promoted artifacts, reject stale or implausible heat-pump telemetry,
bind each artifact to its site, home specification, dataset, and calibration
policy, separate heating from native house load without double counting, and
replay thermal optimizer output before storing it as a diagnostic shadow. The
active storage plan keeps the whole-house load, and no thermal action reaches
the live slot directive until a guarded heat-pump adapter exists.
