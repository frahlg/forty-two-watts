// Plant end-to-end: sim-pcs racks ← ftw-plant controller ← ftw_plant
// Lua driver ← core driver registry. Verifies the Stage-2 chain the
// architecture promises: core sees one battery; rack faults shrink
// reported headroom; killing the plant module stales the driver and the
// racks ramp themselves to zero on lease expiry.
//
// Run with:  FTW_E2E=1 go test ./go/test/e2e -run TestE2E_Plant -v
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	sv "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/cmd/sim-pcs/pcs"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/plant"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func plantDriverPath(t *testing.T) string {
	t.Helper()
	root := findRepoRoot(t)
	// Prefer the bundled snapshot once the device-drivers pin includes
	// ftw_plant; fall back to the test fixture until then.
	bundled := filepath.Join(root, "drivers", "ftw_plant.lua")
	if _, err := os.Stat(bundled); err == nil {
		return bundled
	}
	return filepath.Join(root, "go", "test", "e2e", "testdata", "ftw_plant.lua")
}

func waitFor(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestE2E_Plant(t *testing.T) {
	if os.Getenv("FTW_E2E") != "1" {
		t.Skip("set FTW_E2E=1 to run the full-stack test")
	}

	// ---- sim-pcs: 3 racks on one Modbus server ----
	modbusPort := freePort(t)
	simPlant := pcs.NewPlant(3, pcs.DefaultRack())
	srv, err := sv.NewServer(&sv.ServerConfiguration{
		URL: fmt.Sprintf("tcp://127.0.0.1:%d", modbusPort), Timeout: 5 * time.Second, MaxClients: 8,
	}, simPlant)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	physCtx, physCancel := context.WithCancel(context.Background())
	defer physCancel()
	go func() {
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		last := time.Now()
		for {
			select {
			case <-physCtx.Done():
				return
			case now := <-tk.C:
				simPlant.Tick(now.Sub(last))
				last = now
			}
		}
	}()

	// ---- ftw-plant controller + /v1 server ----
	units := []plant.UnitConfig{
		{ID: "r1", Host: "127.0.0.1", Port: modbusPort, UnitID: 1},
		{ID: "r2", Host: "127.0.0.1", Port: modbusPort, UnitID: 2},
		{ID: "r3", Host: "127.0.0.1", Port: modbusPort, UnitID: 3},
	}
	plantCtrl := plant.NewController(plant.Config{
		Units:           units,
		PollInterval:    200 * time.Millisecond,
		ControlInterval: 200 * time.Millisecond,
		DefaultLeaseTTL: 3 * time.Second,
		StaleAfter:      2 * time.Second,
	})
	plantCtx, plantCancel := context.WithCancel(context.Background())
	defer plantCancel()
	go plantCtrl.Run(plantCtx)
	plantPort := freePort(t)
	plantSrv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", plantPort),
		Handler: plant.NewServeMux(plantCtrl),
	}
	go func() { _ = plantSrv.ListenAndServe() }()
	defer plantSrv.Close()

	// ---- Core: registry + ftw_plant driver ----
	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	driverCfg := config.Driver{
		Name: "plant", Lua: plantDriverPath(t),
		BatteryCapacityWh: 150_000,
		Capabilities: config.Capabilities{
			HTTP: &config.HTTPCapability{AllowedHosts: []string{"127.0.0.1"}},
		},
		Config: map[string]any{
			"host":         "127.0.0.1",
			"port":         plantPort,
			"lease_ttl_ms": 3000,
		},
	}
	ctx := context.Background()
	if err := reg.Add(ctx, driverCfg); err != nil {
		t.Fatal(err)
	}
	defer reg.Remove("plant")

	// 1. Telemetry flows: one aggregate battery, three units online.
	waitFor(t, 15*time.Second, "battery telemetry", func() bool {
		r := tel.Get("plant", telemetry.DerBattery)
		return r != nil && r.SoC != nil && *r.SoC > 0.4 && *r.SoC < 0.6
	})
	waitFor(t, 10*time.Second, "units_online metric", func() bool {
		v, _, ok := tel.LatestMetric("plant", "plant_units_online")
		return ok && v == 3
	})

	// 2. A battery command dispatches through the driver, the module
	// allocates it, and the racks physically converge on the target.
	payload, _ := json.Marshal(map[string]any{"action": "battery", "power_w": 30000})
	if err := reg.Send(ctx, "plant", payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, 20*time.Second, "racks to converge on 30 kW", func() bool {
		// Keep the lease alive the way core's control loop would.
		_ = reg.Send(ctx, "plant", payload)
		var total float64
		for _, r := range simPlant.Racks() {
			total += r.Snapshot().PowerW
		}
		return math.Abs(total-30000) < 1500
	})

	// 3. Fault a rack: reported headroom shrinks and the remaining two
	// racks keep serving the (reduced but feasible) target.
	simPlant.Racks()[2].SetFault(true)
	waitFor(t, 15*time.Second, "headroom derate after rack fault", func() bool {
		_ = reg.Send(ctx, "plant", payload)
		v, _, ok := tel.LatestMetric("plant", "plant_available_charge_w")
		return ok && v <= 50000
	})
	waitFor(t, 15*time.Second, "online-units metric drop", func() bool {
		_ = reg.Send(ctx, "plant", payload)
		v, _, ok := tel.LatestMetric("plant", "plant_units_online")
		return ok && v == 2
	})

	// 4. Kill the plant module: the driver's polls fail (watchdog would
	// stale it in core) and — critically — the racks ramp to zero on
	// their own when the lease expires. No component needs to succeed
	// for the plant to end up safe.
	plantSrv.Close()
	plantCancel() // controller stops refreshing setpoints too
	waitFor(t, 20*time.Second, "racks to ramp to zero after module death", func() bool {
		var total float64
		for _, r := range simPlant.Racks() {
			total += math.Abs(r.Snapshot().PowerW)
		}
		return total < 500
	})

	// The driver deliberately stops EMITTING on module death (rather
	// than erroring), so core's watchdog sees stale telemetry — the
	// designed path into the autonomous default.
	waitFor(t, 20*time.Second, "battery telemetry to go stale", func() bool {
		return tel.IsStale("plant", telemetry.DerBattery, 5*time.Second)
	})
}
