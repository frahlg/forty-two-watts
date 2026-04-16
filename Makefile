# Top-level build for the Go + WASM port.
#
# Common targets:
#   make test         — full test suite (Go + WASM drivers + e2e)
#   make wasm         — build WASM drivers into drivers-wasm/
#   make build        — native binaries for this machine
#   make build-arm64  — cross-compile for linux/arm64 (RPi)
#   make release      — arm64 + amd64 tarballs in release/
#   make run-sim      — start both simulators locally
#   make dev          — start sims + main app (hot-reload workflow)
#   make clean        — remove all build artifacts

.PHONY: help test wasm build build-arm64 build-amd64 release \
        run-sim dev fmt vet clean e2e docs \
        build-lua test-lua dev-lua

# Rustup's stable toolchain — separate from Homebrew's rust.
# Adjust if rustup isn't here.
RUSTUP_STABLE ?= /Users/fredde/.rustup/toolchains/stable-aarch64-apple-darwin/bin
CARGO_WASM := PATH="$(RUSTUP_STABLE):$$PATH" cargo

WASM_DRIVERS := ferroamp sungrow
WASM_OUT_DIR := drivers-wasm

VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || echo "$(shell git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)-dev")
LDFLAGS := -s -w -X main.Version=$(VERSION)

help:
	@echo "forty-two-watts — Go + WASM port"
	@echo ""
	@echo "Targets:"
	@echo "  test         run full test suite"
	@echo "  wasm         build WASM drivers ($(WASM_DRIVERS))"
	@echo "  build        native binaries into bin/"
	@echo "  build-arm64  cross-compile for linux/arm64"
	@echo "  release      arm64 + amd64 tarballs"
	@echo "  run-sim      start Ferroamp + Sungrow simulators"
	@echo "  dev          start sims + main app against config.local.yaml"
	@echo "  build-lua    build main binary only (skip WASM/Rust compile)"
	@echo "  test-lua     run Go tests, skip WASM rebuild (uses checked-in .wasm)"
	@echo "  dev-lua      run main app against config.local.yaml, no WASM rebuild"
	@echo "  e2e          run the full-stack e2e test"
	@echo "  fmt vet      Go format + static checks"
	@echo "  clean        nuke build artifacts"

# ---- Testing ----

test: wasm
	cd go && go test ./...

e2e: wasm
	cd go && go test ./test/e2e -v -timeout 180s

# ---- WASM drivers ----

wasm: $(foreach d,$(WASM_DRIVERS),$(WASM_OUT_DIR)/$(d).wasm)

$(WASM_OUT_DIR)/%.wasm: wasm-drivers/%/src/lib.rs wasm-drivers/%/Cargo.toml
	@mkdir -p $(WASM_OUT_DIR)
	cd wasm-drivers/$* && $(CARGO_WASM) build --target wasm32-wasip1 --release
	cp wasm-drivers/$*/target/wasm32-wasip1/release/$*_driver.wasm $@
	@printf "built %s (%s bytes)\n" "$@" "$$(wc -c <"$@")"

# ---- Native builds ----

build: wasm
	@mkdir -p bin
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts ./cmd/forty-two-watts
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-sungrow ./cmd/sim-sungrow
	@ls -la bin/

build-arm64: wasm
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-linux-arm64 ./cmd/forty-two-watts
	@ls -la bin/forty-two-watts-linux-arm64

build-amd64: wasm
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-linux-amd64 ./cmd/forty-two-watts
	@ls -la bin/forty-two-watts-linux-amd64

# ---- Release tarballs ----

release: build-arm64 build-amd64
	@mkdir -p release
	@for arch in arm64 amd64; do \
		tar czf release/forty-two-watts-linux-$$arch.tar.gz \
			-C bin forty-two-watts-linux-$$arch \
			-C .. $(WASM_OUT_DIR) web config.example.yaml; \
		printf "built release/forty-two-watts-linux-%s.tar.gz (%s bytes)\n" "$$arch" \
			"$$(wc -c <release/forty-two-watts-linux-$$arch.tar.gz)"; \
	done

# ---- Dev workflow ----

run-sim:
	@echo "Starting simulators (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	wait

dev: wasm
	@echo "Starting sims + main app (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	sleep 2 && \
	(cd go && go run ./cmd/forty-two-watts -config ../config.local.yaml -web ../web) & \
	wait

# ---- Lua-only workflow ----
#
# These targets skip the Rust → WASM compile step entirely. Useful when
# you're developing Lua drivers on a machine without the Rust toolchain
# (or without the wasm32-wasip1 target). The checked-in drivers-wasm/*.wasm
# binaries are reused as-is, so the legacy Ferroamp/Sungrow WASM drivers
# still load if your config references them.

build-lua:
	@mkdir -p bin
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts ./cmd/forty-two-watts
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-sungrow ./cmd/sim-sungrow
	@ls -la bin/

test-lua:
	cd go && go test ./...

dev-lua:
	@echo "Starting sims + main app against config.local.yaml (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	sleep 2 && \
	(cd go && go run ./cmd/forty-two-watts -config ../config.local.yaml -web ../web) & \
	wait

# ---- Hygiene ----

fmt:
	cd go && go fmt ./...

vet:
	cd go && go vet ./...

clean:
	rm -rf $(WASM_OUT_DIR) bin release
	cd go && go clean
	cd wasm-drivers/ferroamp && rm -rf target Cargo.lock
	cd wasm-drivers/sungrow && rm -rf target Cargo.lock

docs:
	@echo "see docs/ for:"
	@ls -1 docs/
