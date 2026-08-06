// ftw-plant: multi-rack battery plant module. Presents N PCS/rack units
// (Modbus TCP) as one logical battery over the /v1 HTTP contract in
// go/internal/plant, consumed by core through the ftw_plant Lua driver.
//
// Runs in its own container (Dockerfile.plant) with an independent
// release cadence. Safe-absent by construction: if this process stops,
// core's driver goes stale → autonomous default; and every setpoint
// carries a lease, so the racks ramp to zero on their own when the
// controller (or core) goes quiet.
//
// Config (YAML, -config; see plant.example.yaml):
//
//	listen: 127.0.0.1:9200
//	poll_interval_ms: 1000
//	control_interval_ms: 1000
//	default_lease_ttl_ms: 10000
//	units:
//	  - {id: rack-1, host: 192.168.1.60, port: 502, unit_id: 1}
//	  - {id: rack-2, host: 192.168.1.60, port: 502, unit_id: 2}
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/srcfl/ftw/go/internal/plant"
)

type fileConfig struct {
	Listen            string             `yaml:"listen"`
	PollIntervalMs    int                `yaml:"poll_interval_ms"`
	ControlIntervalMs int                `yaml:"control_interval_ms"`
	DefaultLeaseTTLMs int                `yaml:"default_lease_ttl_ms"`
	StaleAfterMs      int                `yaml:"stale_after_ms"`
	Units             []plant.UnitConfig `yaml:"units"`
}

func load(path string) (fileConfig, error) {
	var fc fileConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return fc, err
	}
	if err := yaml.Unmarshal(b, &fc); err != nil {
		return fc, err
	}
	if fc.Listen == "" {
		fc.Listen = "127.0.0.1:9200"
	}
	if len(fc.Units) == 0 {
		return fc, errors.New("at least one unit is required")
	}
	seen := map[string]bool{}
	for _, u := range fc.Units {
		if u.ID == "" || u.Host == "" || u.Port <= 0 || u.UnitID <= 0 {
			return fc, fmt.Errorf("unit %+v: id, host, port and unit_id are required", u)
		}
		if seen[u.ID] {
			return fc, fmt.Errorf("duplicate unit id %q", u.ID)
		}
		seen[u.ID] = true
	}
	return fc, nil
}

func main() {
	configPath := flag.String("config", "plant.yaml", "plant config YAML")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	fc, err := load(*configPath)
	if err != nil {
		slog.Error("plant config", "path", *configPath, "err", err)
		os.Exit(1)
	}

	ctrl := plant.NewController(plant.Config{
		Units:           fc.Units,
		PollInterval:    time.Duration(fc.PollIntervalMs) * time.Millisecond,
		ControlInterval: time.Duration(fc.ControlIntervalMs) * time.Millisecond,
		DefaultLeaseTTL: time.Duration(fc.DefaultLeaseTTLMs) * time.Millisecond,
		StaleAfter:      time.Duration(fc.StaleAfterMs) * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &http.Server{Addr: fc.Listen, Handler: plant.NewServeMux(ctrl)}
	go func() {
		slog.Info("ftw-plant listening", "addr", fc.Listen, "units", len(fc.Units))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server", "err", err)
			cancel()
		}
	}()

	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sigc:
	case <-ctx.Done():
	}
	slog.Info("shutdown — ramping racks to zero")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	cancel()
	<-done
}
