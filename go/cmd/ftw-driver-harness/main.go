// Command ftw-driver-harness loads a single Lua driver, points it at an
// arbitrary IP/port, and streams telemetry + logs as newline-delimited JSON.
//
// Two modes:
//   - run      — one-shot CLI: spawn the driver, stream events to stdout
//                until the duration elapses or SIGINT arrives.
//   - serve    — JSON-RPC over stdio for the Tauri GUI (stub in step 1).
//
// Plus:
//   - catalog  — dump the parsed DRIVER={…} metadata block for each
//                driver file under a directory, as JSON.
//
// The binary reuses the production driver registry + capability wrappers
// (go/internal/drivers, mqtt, modbus, telemetry) unchanged so what the
// harness runs is bit-for-bit what the Pi runs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/frahlg/forty-two-watts/go/internal/harness"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "serve":
		os.Exit(serveCmd(os.Args[2:]))
	case "catalog":
		os.Exit(catalogCmd(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `ftw-driver-harness — load a single Lua driver and stream its telemetry.

Subcommands:
  run      Run one driver; stream newline-delimited JSON events to stdout.
  serve    JSON-RPC over stdio for the Tauri GUI (not yet implemented).
  catalog  List available drivers under a directory as JSON.

Run 'ftw-driver-harness <cmd> -h' for subcommand flags.`)
}

func runCmd(argv []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		driverPath = fs.String("driver", "", "path to driver .lua file (required)")
		name       = fs.String("name", "", "logical driver name (default: basename of -driver)")
		mqttHost   = fs.String("mqtt-host", "", "MQTT broker host (enables MQTT capability)")
		mqttPort   = fs.Int("mqtt-port", 1883, "MQTT broker port")
		mqttUser   = fs.String("mqtt-user", "", "MQTT username (optional)")
		mqttPass   = fs.String("mqtt-pass", "", "MQTT password (optional)")
		mbHost     = fs.String("modbus-host", "", "Modbus TCP host (enables Modbus capability)")
		mbPort     = fs.Int("modbus-port", 502, "Modbus TCP port")
		unitID     = fs.Int("unit-id", 1, "Modbus unit ID")
		httpCap    = fs.Bool("http", false, "enable HTTP capability")
		cfgFile    = fs.String("config-file", "", "YAML file whose top-level map is passed to driver_init()")
		duration   = fs.Duration("duration", 0, "stop after this duration (0 = run until SIGINT)")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ftw-driver-harness run -driver path/to/driver.lua [options]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(argv)
	if *driverPath == "" {
		fmt.Fprintln(os.Stderr, "error: -driver is required")
		fs.Usage()
		return 2
	}
	if *name == "" {
		base := filepath.Base(*driverPath)
		*name = strings.TrimSuffix(base, filepath.Ext(base))
	}

	// Harness-own logger writes to stderr — stdout is reserved for the
	// NDJSON event stream so consumers (jq, the Tauri bridge) can pipe it.
	hlog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	spec := harness.Spec{
		Name:       *name,
		Lua:        *driverPath,
		MQTTHost:   *mqttHost,
		MQTTPort:   *mqttPort,
		MQTTUser:   *mqttUser,
		MQTTPass:   *mqttPass,
		ModbusHost: *mbHost,
		ModbusPort: *mbPort,
		ModbusUnit: *unitID,
		HTTP:       *httpCap,
		ConfigFile: *cfgFile,
		Duration:   *duration,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	writer := harness.NewEventWriter(os.Stdout)
	if err := harness.NewRunner(spec, writer, hlog).Run(ctx); err != nil {
		hlog.Error("run failed", "err", err)
		writer.Error(err.Error())
		return 1
	}
	return 0
}

func serveCmd(argv []string) int {
	// Deferred to step 2: newline-delimited JSON-RPC over stdio. Methods
	// planned: load_driver, reload_driver, call_command, stop, list_catalog.
	// Events: same schema the `run` subcommand already emits. Until then the
	// GUI can shell out to `run` per driver session — this is a progressive
	// rollout, not a blocker.
	fmt.Fprintln(os.Stderr, "serve mode not yet implemented — use 'run' for streaming CLI")
	_ = argv
	return 2
}

func catalogCmd(argv []string) int {
	fs := flag.NewFlagSet("catalog", flag.ExitOnError)
	dir := fs.String("dir", "./drivers", "directory to scan for .lua driver files")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ftw-driver-harness catalog [-dir drivers]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(argv)
	return harness.PrintCatalog(os.Stdout, *dir)
}
