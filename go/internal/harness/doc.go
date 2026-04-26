// Package harness runs a single Lua driver end-to-end for interactive testing.
//
// It reuses the production drivers.Registry + telemetry.Store + capability
// wrappers so fidelity with the real forty-two-watts service is guaranteed:
// whatever the harness can run, the Pi can run. Used by the ftw-driver-harness
// binary in both CLI ("run") and (forthcoming) JSON-RPC ("serve") modes, and
// will back the Tauri desktop app via the sidecar pattern.
package harness
