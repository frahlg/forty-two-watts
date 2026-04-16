package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/scanner"
)

// runBootstrap starts a minimal HTTP server that serves the setup wizard.
// It is called when config.Load fails (no config.yaml yet). The wizard
// collects initial configuration, writes it to disk, and restarts the
// process so the normal startup path takes over.
func runBootstrap(configPath, webDir, driverDir string) {
	slog.Info("no config found — starting setup wizard", "url", "http://localhost:8080/setup")

	mux := http.NewServeMux()

	// GET / → redirect to setup wizard
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		// Serve static files from web dir.
		serveStatic(w, r, webDir)
	})

	// GET /setup → serve setup.html
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(webDir, "setup.html")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		http.ServeFile(w, r, path)
	})

	// GET /api/drivers/catalog → scan Lua drivers
	mux.HandleFunc("GET /api/drivers/catalog", func(w http.ResponseWriter, r *http.Request) {
		entries, err := drivers.LoadCatalog(driverDir)
		if err != nil {
			writeBootstrapJSON(w, 200, map[string]any{
				"path":    driverDir,
				"entries": []any{},
				"error":   err.Error(),
			})
			return
		}
		writeBootstrapJSON(w, 200, map[string]any{
			"path":    driverDir,
			"entries": entries,
		})
	})

	// GET /api/scan → network scanner
	mux.HandleFunc("GET /api/scan", func(w http.ResponseWriter, r *http.Request) {
		devices, err := scanner.Scan(r.Context())
		if err != nil {
			writeBootstrapJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeBootstrapJSON(w, 200, devices)
	})

	// POST /api/config → validate, write, restart
	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		if err != nil {
			writeBootstrapJSON(w, 400, map[string]string{"error": "read body: " + err.Error()})
			return
		}

		var cfg config.Config
		if err := json.Unmarshal(body, &cfg); err != nil {
			writeBootstrapJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}

		if err := cfg.Validate(); err != nil {
			writeBootstrapJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}

		if err := config.SaveAtomic(configPath, &cfg); err != nil {
			writeBootstrapJSON(w, 500, map[string]string{"error": "write config: " + err.Error()})
			return
		}

		slog.Info("config written by setup wizard — restarting", "path", configPath)
		writeBootstrapJSON(w, 200, map[string]string{"status": "ok", "restart": "true"})

		// Restart the process so the normal startup path picks up the new config.
		go func() {
			exe, err := os.Executable()
			if err != nil {
				exe = os.Args[0]
			}
			_ = syscall.Exec(exe, os.Args, os.Environ())
		}()
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("bootstrap server", "err", err)
		os.Exit(1)
	}
}

// serveStatic serves files from webDir with path-traversal protection.
func serveStatic(w http.ResponseWriter, r *http.Request, webDir string) {
	clean := filepath.Clean(filepath.Join(webDir, r.URL.Path))
	absWeb, _ := filepath.Abs(webDir)
	absPath, _ := filepath.Abs(clean)
	if !strings.HasPrefix(absPath, absWeb+string(filepath.Separator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, clean)
}

func writeBootstrapJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
