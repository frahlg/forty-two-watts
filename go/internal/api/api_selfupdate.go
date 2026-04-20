package api

import (
	"net/http"
)

// handleVersionCheck returns the cached self-update state. ?force=1 bypasses
// the cache and contacts GitHub directly. All fields in selfupdate.Info are
// passed through verbatim so the UI does the rendering.
func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	force := r.URL.Query().Get("force") == "1"
	if force {
		info, err := s.deps.SelfUpdate.Check(r.Context(), true)
		if err != nil {
			// Return the full Info schema with Err populated so the UI has
			// one shape to handle (not a special error envelope).
			info.Err = err.Error()
			writeJSON(w, 502, info)
			return
		}
	}
	writeJSON(w, 200, s.deps.SelfUpdate.Info())
}

// handleVersionSkip persists a dismissed version. A subsequent /check with a
// NEWER release resurfaces the notification automatically — Skip only hides
// the version passed in the body, not everything above it.
func (s *Server) handleVersionSkip(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := s.deps.SelfUpdate.Skip(body.Version); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "skipped": true, "version": body.Version})
}

// handleVersionUnskip clears the persisted skip. Called from the UI's
// "Check for updates" action so a user who skipped vX.Y.Z can resurface it
// without waiting for a newer release.
func (s *Server) handleVersionUnskip(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	if err := s.deps.SelfUpdate.Unskip(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "skipped": false})
}

// handleVersionUpdate signals the sidecar to pull the latest image + compose
// up the main service. Returns as soon as the sidecar acknowledges; the UI
// polls /api/version/update/status for progress.
//
// Before handing off to the sidecar we capture a rollback-point snapshot
// (state.db + config.yaml) into SnapshotDir. A failed snapshot aborts
// the update — the whole point of offering "Update" is that the user
// knows they can back out, and shipping without the safety net breaks
// that promise. Two exceptions skip the snapshot:
//
//   - SnapshotDir is empty (operator opted out at deploy time).
//   - The request body sets {"skip_snapshot": true} (operator opted
//     out for this specific update via the UI checkbox, typically
//     because the existing 5 retained snapshots already cover them).
//
// Both exceptions return \`snapshot_skipped: true\` in the response so
// the UI can differentiate "no snapshot taken on purpose" from "no
// snapshot field because the field was elided".
func (s *Server) handleVersionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}

	// Body is optional — empty body / null JSON yields the zero value
	// (SkipSnapshot false), so pre-checkbox UIs keep getting the snapshot.
	var body struct {
		SkipSnapshot bool `json:"skip_snapshot,omitempty"`
	}
	// readJSON caps at 1 MB (api.go:153). Errors here include EOF on an
	// empty body, which we treat as the operator using the legacy no-body
	// path.
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
			return
		}
	}

	info := s.deps.SelfUpdate.Info()

	var snap SnapshotInfo
	snapshotSkipped := body.SkipSnapshot || s.deps.SnapshotDir == ""
	if !snapshotSkipped {
		var err error
		snap, err = s.createPreUpdateSnapshot("update", info.Current, info.Latest)
		if err != nil {
			writeJSON(w, 500, map[string]string{
				"error":   "snapshot failed: " + err.Error(),
				"hint":    "Update aborted so you keep a rollback point. Check SnapshotDir permissions or free space — or re-submit with skip_snapshot=true if you accept the risk.",
				"snapshot_dir": s.deps.SnapshotDir,
			})
			return
		}
	}

	if err := s.deps.SelfUpdate.Trigger(r.Context(), "update", info.Latest); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]any{"status": "started", "action": "update", "target": info.Latest}
	if snap.ID != "" {
		resp["snapshot"] = snap
	}
	if snapshotSkipped {
		resp["snapshot_skipped"] = true
	}
	writeJSON(w, 202, resp)
}

// handleVersionRestart signals the sidecar to pull + force-recreate the
// main service regardless of whether a newer image exists. Exists so the
// full update flow can be exercised end-to-end in dev / CI before cutting
// a real release.
func (s *Server) handleVersionRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	if err := s.deps.SelfUpdate.Trigger(r.Context(), "restart", ""); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, map[string]any{"status": "started", "action": "restart"})
}

// handleVersionUpdateStatus passes through the sidecar's state.json. The
// shared tmpfs volume makes this survive the main container being recreated:
// the new container reads the same file written by the (still-running)
// sidecar and serves the last transition (pulling → restarting → done) to
// the UI which is still polling from the browser.
func (s *Server) handleVersionUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.SelfUpdate == nil {
		writeJSON(w, 503, map[string]string{"error": "self-update disabled"})
		return
	}
	writeJSON(w, 200, s.deps.SelfUpdate.Status())
}
