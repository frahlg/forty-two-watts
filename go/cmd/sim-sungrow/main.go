// sim-sungrow — Modbus TCP server simulating a Sungrow SH-series hybrid inverter.
//
// Phase 1 target: Modbus TCP on :5502 with realistic holding + input registers
// (SoC, PV production, grid meter, EMS mode/cmd). Writes to 13050/13051 update
// internal state. First-order response dynamics for commanded battery power.
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	slog.Info("sim-sungrow — scaffold, implementation in phase 1")
}
