package loadpoint

import (
	"encoding/json"
	"fmt"
	"strings"
)

type storedManualHold struct {
	Version   int        `json:"version"`
	DeviceID  string     `json:"device_id"`
	SessionID string     `json:"session_id,omitempty"`
	Hold      ManualHold `json:"hold"`
}

type pendingManualHold struct {
	hold    ManualHold
	cleared bool
}

func manualHoldKey(deviceID string) string {
	return "ev_manual_hold:" + strings.TrimPrefix(sessionKey(deviceID), "ev_session:")
}

// PersistManualHold records an explicit operator action. Before first hardware
// telemetry, retain the action in memory and bind it to the first fresh device
// reading. Such a Start still works immediately; it does not silently gain
// permission to charge another car after a later process restart.
func (m *Manager) PersistManualHold(id string, h ManualHold, cleared bool) error {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	if m.pendingManual == nil {
		m.pendingManual = map[string]pendingManualHold{}
	}
	m.pendingManual[id] = pendingManualHold{h, cleared}
	if cleared && m.sessionStore != nil {
		// A clear before telemetry must survive another immediate reboot.
		// This barrier can only remove an old request; it grants no power.
		if err := m.sessionStore.SaveConfig("ev_manual_clear:"+id, "pending"); err != nil {
			return err
		}
		if err := m.sessionStore.SaveConfig("loadpoint_manual_hold:"+id, "{}"); err != nil {
			return err
		}
	}
	return m.flushManualHold(id)
}

func (m *Manager) flushManualHold(id string) error {
	pending, ok := m.pendingManual[id]
	if !ok || m.sessionStore == nil {
		return nil
	}
	m.mu.RLock()
	lp := m.byID[id]
	var deviceID, sessionID string
	if lp != nil {
		deviceID, sessionID = lp.sessionDeviceID, lp.sessionID
	}
	m.mu.RUnlock()
	if deviceID == "" {
		return nil
	}
	body := "{}"
	if !pending.cleared && pending.hold.Persistent {
		h := pending.hold
		if !finite(h.PowerW) || h.PowerW < 0 {
			return fmt.Errorf("invalid stored manual power")
		}
		b, err := json.Marshal(storedManualHold{Version: 1, DeviceID: deviceID, SessionID: sessionID, Hold: h})
		if err != nil {
			return err
		}
		body = string(b)
	}
	if err := m.sessionStore.SaveConfig(manualHoldKey(deviceID), body); err != nil {
		return err
	}
	// This name-keyed index is only a hint to pause while identity is unknown;
	// it never grants positive power or bypasses the hardware/session match.
	if err := m.sessionStore.SaveConfig("ev_manual_binding:"+id, deviceID); err != nil {
		return err
	}
	// Retire the legacy name key too: a cleared new record must not fall back
	// to an earlier unbound hold on the next boot.
	if err := m.sessionStore.SaveConfig("loadpoint_manual_hold:"+id, "{}"); err != nil {
		return err
	}
	if err := m.sessionStore.SaveConfig("ev_manual_clear:"+id, ""); err != nil {
		return err
	}
	delete(m.pendingManual, id)
	return nil
}

// RestoreManualHold returns pending until hardware identity is known. A
// verified same-session positive hold or a hardware-bound pause is restored.
// Unknown/changed sessions and legacy records return a persistent zero-W hold
// plus unconfirmed: failed restoration must not start automatic charging.
func (m *Manager) RestoreManualHold(id string) (ManualHold, string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.mu.RLock()
	lp := m.byID[id]
	var deviceID, sessionID string
	if lp != nil {
		deviceID, sessionID = lp.sessionDeviceID, lp.sessionID
	}
	m.mu.RUnlock()
	if m.sessionStore == nil {
		return ManualHold{}, "none"
	}
	if clear, _ := m.sessionStore.LoadConfig("ev_manual_clear:" + id); clear == "pending" {
		if m.pendingManual == nil {
			m.pendingManual = map[string]pendingManualHold{}
		}
		m.pendingManual[id] = pendingManualHold{cleared: true}
		_ = m.flushManualHold(id)
		return ManualHold{}, "none"
	}
	if deviceID == "" {
		binding, bound := m.sessionStore.LoadConfig("ev_manual_binding:" + id)
		if bound && binding != "" {
			if raw, found := m.sessionStore.LoadConfig(manualHoldKey(binding)); !found || (raw != "" && raw != "{}") {
				return ManualHold{Persistent: true}, "pending"
			}
		}
		legacy, found := m.sessionStore.LoadConfig("loadpoint_manual_hold:" + id)
		if found && legacy != "" && legacy != "{}" {
			return ManualHold{Persistent: true}, "pending"
		}
		return ManualHold{}, "pending"
	}
	raw, found := m.sessionStore.LoadConfig(manualHoldKey(deviceID))
	if !found {
		if binding, ok := m.sessionStore.LoadConfig("ev_manual_binding:" + id); ok && binding != "" {
			if old, present := m.sessionStore.LoadConfig(manualHoldKey(binding)); !present || (old != "" && old != "{}") {
				return ManualHold{Persistent: true}, "unconfirmed"
			}
		}
		legacy, ok := m.sessionStore.LoadConfig("loadpoint_manual_hold:" + id)
		if ok && legacy != "" && legacy != "{}" {
			return ManualHold{Persistent: true}, "unconfirmed"
		}
		return ManualHold{}, "none"
	}
	if raw == "" || raw == "{}" {
		return ManualHold{}, "none"
	}
	var record storedManualHold
	if json.Unmarshal([]byte(raw), &record) != nil || record.Version != 1 ||
		record.DeviceID != deviceID || !record.Hold.Persistent ||
		!record.Hold.ExpiresAt.IsZero() || !finite(record.Hold.PowerW) || record.Hold.PowerW < 0 {
		return ManualHold{Persistent: true}, "unconfirmed"
	}
	if record.Hold.PowerW == 0 || (sessionID != "" && sessionID == record.SessionID) {
		return record.Hold, "restored"
	}
	return ManualHold{Persistent: true}, "unconfirmed"
}

func (m *Manager) SetManualRestoreUnconfirmed(id string, value bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp := m.byID[id]; lp != nil {
		lp.manualRestoreUnconfirmed = value
	}
}
