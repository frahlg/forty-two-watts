package loadpoint

type manualSessionBinding struct {
	deviceID, sessionID string
	generation          uint64
	configuration       uint64
}

func (m *Manager) manualSessionBinding(id string) manualSessionBinding {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if lp := m.byID[id]; lp != nil {
		return manualSessionBinding{lp.sessionDeviceID, lp.sessionID, lp.sessionGeneration, lp.configGeneration}
	}
	return manualSessionBinding{}
}

// markManualExplicit prevents a delayed first telemetry sample from replacing
// an action the user already took. Call outside holdMu in Set/ClearManualHold.
func (c *Controller) markManualExplicit(id string) bool {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.manualRestored == nil {
		c.manualRestored = map[string]bool{}
	}
	first := !c.manualRestored[id]
	c.manualRestored[id] = true
	if c.manager != nil {
		if c.manualBindings == nil {
			c.manualBindings = map[string]manualSessionBinding{}
		}
		c.manualBindings[id] = c.manager.manualSessionBinding(id)
		c.manager.SetManualRestoreUnconfirmed(id, false)
	}
	return first
}

// restoreManualHoldForSession runs after ObserveSession, before dispatch. The
// disk read stays outside holdMu; recheck explicit actions before installing.
func (c *Controller) restoreManualHoldForSession(id string) {
	if c.manager == nil {
		return
	}
	c.manualPersistMu.Lock()
	defer c.manualPersistMu.Unlock()
	current := c.manager.manualSessionBinding(id)
	c.holdMu.Lock()
	if c.manualBindings == nil {
		c.manualBindings = map[string]manualSessionBinding{}
	}
	previous, bound := c.manualBindings[id]
	changed := bound && previous.configuration != current.configuration
	if bound && previous.deviceID != "" && current.deviceID != "" {
		changed = changed || previous.deviceID != current.deviceID
		// An explicit Start from a paused/unconfirmed session may acquire its
		// first session ID as charging starts. It already names this hardware.
		firstSessionProof := previous.sessionID == "" && current.sessionID != ""
		changed = changed || (!firstSessionProof && previous.generation != current.generation)
	}
	// A Start before the first hardware reading binds here. Once bound, a
	// different hardware/session/config generation must not inherit its power.
	if current.deviceID != "" {
		c.manualBindings[id] = current
	}
	if changed {
		if _, held := c.holds[id]; held {
			c.holds[id] = ManualHold{Persistent: true}
			c.manualRestored[id] = true
			c.manager.SetManualRestoreUnconfirmed(id, true)
			c.holdMu.Unlock()
			// Keep the safe pause across another restart. The next explicit
			// Start or Return to plan decides how this new session proceeds.
			_ = c.manager.PersistManualHold(id, ManualHold{Persistent: true}, false)
			c.resetManualIdle(id)
			return
		}
	}
	done := c.manualRestored[id]
	c.holdMu.Unlock()
	if done {
		return
	}
	h, status := c.manager.RestoreManualHold(id)
	if status == "pending" && !h.Persistent {
		return
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.manualRestored[id] {
		return
	}
	if c.manualRestored == nil {
		c.manualRestored = map[string]bool{}
	}
	_, hadPending := c.manualRestored[id]
	c.manualRestored[id] = status != "pending"
	if status == "pending" || status == "restored" || status == "unconfirmed" {
		if c.holds == nil {
			c.holds = map[string]ManualHold{}
		}
		c.holds[id] = h
		c.manager.SetManualRestoreUnconfirmed(id, status == "unconfirmed" || status == "pending")
	} else if hadPending {
		delete(c.holds, id)
		c.manager.SetManualRestoreUnconfirmed(id, false)
	}
}
