// sim-ferroamp — embedded MQTT broker + Ferroamp simulator.
//
// Phase 1 target: publishes realistic extapi/data/{ehub,eso,sso} topics,
// responds to extapi/control/request commands with first-order lag dynamics.
// Lets the main app be tested end-to-end locally with no real hardware.
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	slog.Info("sim-ferroamp — scaffold, implementation in phase 1")
}
