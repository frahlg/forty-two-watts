// sim-pcs: Modbus TCP server simulating a multi-rack commercial battery
// plant. Each rack is one Modbus unit ID (1..N) with the register map
// documented in pcs/sim.go; a control HTTP port injects faults, comms
// loss and SoC pins for tests and demos.
//
// Run:    go run ./cmd/sim-pcs -racks 4
// Poke:   curl localhost:5610/state
//
//	curl -X POST 'localhost:5610/rack/2/fault?on=1'
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/cmd/sim-pcs/pcs"
)

func main() {
	addr := flag.String("addr", "tcp://0.0.0.0:5602", "Modbus TCP bind URL")
	ctrlAddr := flag.String("control", "127.0.0.1:5610", "control/fault-injection HTTP bind")
	racks := flag.Int("racks", 4, "number of PCS racks (Modbus unit IDs 1..N)")
	tick := flag.Duration("tick", 500*time.Millisecond, "physics refresh interval")
	capWh := flag.Float64("rack-capacity-wh", 50_000, "per-rack usable capacity Wh")
	maxW := flag.Float64("rack-max-w", 25_000, "per-rack max charge/discharge W")
	initSoC := flag.Float64("soc", 0.5, "starting SoC 0..1 (per-rack spread applied)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if *racks < 1 || *racks > pcs.MaxRacks {
		slog.Error("invalid rack count", "racks", *racks, "min", 1, "max", pcs.MaxRacks)
		os.Exit(2)
	}

	cfg := pcs.DefaultRack()
	cfg.CapacityWh = *capWh
	cfg.MaxChargeW = *maxW
	cfg.MaxDischargeW = *maxW
	cfg.InitialSoC = *initSoC
	plant := pcs.NewPlant(*racks, cfg)

	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL: *addr, Timeout: 10 * time.Second, MaxClients: 8,
	}, plant)
	if err != nil {
		slog.Error("create modbus server", "err", err)
		os.Exit(1)
	}
	if err := srv.Start(); err != nil {
		slog.Error("start modbus server", "err", err)
		os.Exit(1)
	}
	defer srv.Stop()
	slog.Info("sim-pcs listening", "modbus", *addr, "control", *ctrlAddr, "racks", *racks)

	ctrl := &http.Server{Addr: *ctrlAddr, Handler: plant.ControlMux()}
	go func() {
		if err := ctrl.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("control server", "err", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(*tick)
		defer t.Stop()
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				plant.Tick(now.Sub(last))
				last = now
			}
		}
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	<-sigc
	slog.Info("shutdown")
	_ = ctrl.Close()
	cancel()
	wg.Wait()
}
