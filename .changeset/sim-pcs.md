---
"ftw": minor
---

New `sim-pcs` development simulator: a Modbus TCP server modeling a multi-rack commercial battery plant, one rack per Modbus unit ID, each with first-order power response, SoC integration with efficiency and full/empty cutoffs, and a documented SunSpec-style register map. A control HTTP port injects rack faults, comms loss and SoC pins so the upcoming plant module and e2e suite can exercise per-unit allocation and derating without hardware. Wired into `make build` and `make run-sim`.
