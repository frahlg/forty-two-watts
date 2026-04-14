# Top-level build orchestration for the Go + WASM port.
#
# Targets:
#   make test          — all Go tests + WASM driver tests
#   make wasm          — build all WASM drivers
#   make run-sim       — start both simulators in parallel
#   make run           — start simulators + main app
#   make build         — build native binaries

.PHONY: test wasm run-sim run build clean fmt vet help

# Use rustup's stable toolchain explicitly. Homebrew's rust doesn't have the
# wasm32-wasip1 target even after rustup target add, because the binaries are
# different installations.
RUSTUP_STABLE := /Users/fredde/.rustup/toolchains/stable-aarch64-apple-darwin/bin
CARGO_WASM := PATH="$(RUSTUP_STABLE):$$PATH" cargo

WASM_DRIVERS := ferroamp
WASM_OUT_DIR := drivers-wasm

help:
	@echo "Common targets:"
	@echo "  make test        — run full test suite"
	@echo "  make wasm        — build WASM drivers (into $(WASM_OUT_DIR))"
	@echo "  make run-sim     — start Ferroamp + Sungrow simulators"
	@echo "  make build       — build native Go binaries"
	@echo "  make fmt vet     — format + static check Go code"

test: wasm
	cd go && go test ./...

wasm: $(foreach d,$(WASM_DRIVERS),$(WASM_OUT_DIR)/$(d).wasm)

$(WASM_OUT_DIR)/%.wasm: wasm-drivers/%/src/lib.rs wasm-drivers/%/Cargo.toml
	@mkdir -p $(WASM_OUT_DIR)
	cd wasm-drivers/$* && $(CARGO_WASM) build --target wasm32-wasip1 --release
	cp wasm-drivers/$*/target/wasm32-wasip1/release/$*_driver.wasm $@
	@echo "built $@ ($$(ls -la $@ | awk '{print $$5}') bytes)"

run-sim:
	@echo "Starting simulators (Ctrl+C to stop)..."
	@(cd go && go run ./cmd/sim-ferroamp) &
	@(cd go && go run ./cmd/sim-sungrow) &
	@wait

build:
	cd go && go build -o ../bin/forty-two-watts ./cmd/forty-two-watts
	cd go && go build -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -o ../bin/sim-sungrow ./cmd/sim-sungrow

fmt:
	cd go && go fmt ./...

vet:
	cd go && go vet ./...

clean:
	rm -rf $(WASM_OUT_DIR) bin
	cd go && go clean
	cd wasm-drivers/ferroamp && rm -rf target
