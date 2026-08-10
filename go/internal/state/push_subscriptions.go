package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// PushSubscription is one browser push endpoint an enrolled phone stored,
// with the RFC 8291 keys the box encrypts toward. device_id is the enrolled
// device that stored it — stamped from the authenticated caller, never from
// a request body — so revoking the phone can take its subscriptions with it.
type PushSubscription struct {
	ID          string `json:"id"`
	Endpoint    string `json:"endpoint"`
	P256dh      string `json:"p256dh"`
	Auth        string `json:"auth"`
	DeviceID    string `json:"device_id,omitempty"`
	CreatedAtMs int64  `json:"created_at_ms"`
}

// SavePushSubscription upserts on the endpoint — a browser re-subscribing
// sends the same endpoint with fresh keys, and that is one subscription,
// not two — and returns the row's stable id.
func (s *Store) SavePushSubscription(endpoint, p256dh, auth, deviceID string) (string, error) {
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", fmt.Errorf("push_subscriptions id: %w", err)
	}
	id := hex.EncodeToString(idBytes[:])
	_, err := s.db.Exec(
		`INSERT INTO push_subscriptions (id, endpoint, p256dh, auth, device_id, created_at_ms)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(endpoint) DO UPDATE SET
				p256dh = excluded.p256dh,
				auth = excluded.auth,
				device_id = excluded.device_id`,
		id, endpoint, p256dh, auth, deviceID, time.Now().UnixMilli())
	if err != nil {
		return "", fmt.Errorf("push_subscriptions upsert: %w", err)
	}
	// The upsert kept the existing id when the endpoint was known; read
	// back whichever id the row actually carries.
	var storedID string
	if err := s.db.QueryRow(
		`SELECT id FROM push_subscriptions WHERE endpoint = ?`, endpoint,
	).Scan(&storedID); err != nil {
		return "", fmt.Errorf("push_subscriptions read back: %w", err)
	}
	return storedID, nil
}

// PushSubscriptions lists every stored subscription, oldest first.
func (s *Store) PushSubscriptions() ([]PushSubscription, error) {
	rows, err := s.db.Query(
		`SELECT id, endpoint, p256dh, auth, device_id, created_at_ms
			FROM push_subscriptions ORDER BY created_at_ms, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.ID, &p.Endpoint, &p.P256dh, &p.Auth,
			&p.DeviceID, &p.CreatedAtMs); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePushSubscription removes one row by id. Idempotent: deleting what
// is already gone is the outcome the caller wanted.
func (s *Store) DeletePushSubscription(id string) error {
	_, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("push_subscriptions delete: %w", err)
	}
	return nil
}

// DeletePushSubscriptionsByDevice removes every subscription a device
// stored. Called from the enrollment revocation path: a phone that may no
// longer read the house may no longer be told about it either. Returns how
// many rows went.
func (s *Store) DeletePushSubscriptionsByDevice(deviceID string) (int64, error) {
	if deviceID == "" {
		// An empty device would sweep every row no phone claimed —
		// exactly the rows revocation must not touch.
		return 0, nil
	}
	res, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE device_id = ?`, deviceID)
	if err != nil {
		return 0, fmt.Errorf("push_subscriptions device sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
