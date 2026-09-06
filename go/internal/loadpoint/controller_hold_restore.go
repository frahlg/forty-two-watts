package loadpoint

// markManualExplicit prevents a delayed first telemetry sample from replacing
// an action the user already took. Call outside holdMu in Set/ClearManualHold.
func (c *Controller) markManualExplicit(id string) {
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.manualRestored == nil {
		c.manualRestored = map[string]bool{}
	}
	c.manualRestored[id] = true
	if c.manager != nil {
		c.manager.SetManualRestoreUnconfirmed(id, false)
	}
}

// restoreManualHoldForSession runs after ObserveSession, before dispatch. The
// disk read stays outside holdMu; recheck explicit actions before installing.
func (c *Controller) restoreManualHoldForSession(id string) {
	if c.manager == nil {
		return
	}
	c.holdMu.Lock()
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
