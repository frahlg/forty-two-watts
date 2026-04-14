// forty-two-watts — Home Energy Management System (Go + WASM port)
//
// See /MIGRATION_PLAN.md for architecture. This is Phase 0: scaffold.
package main

import (
	"flag"
	"log/slog"
	"os"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config.yaml")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	slog.Info("forty-two-watts starting", "config", *configPath, "phase", "0-scaffold")
	slog.Info("Don't Panic 🐬")
}
