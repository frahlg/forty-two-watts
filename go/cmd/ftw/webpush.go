package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/notifications"
	"github.com/srcfl/ftw/go/internal/state"
)

// The web-push glue: state.Store adapted onto the narrow interfaces the
// notifications provider and the dead man's switch ask for. The adapters
// live here so neither package imports storage — the same seam the
// notification history already uses.

// pushSubscriptionSource is state.Store as the web-push provider sees it.
type pushSubscriptionSource struct{ st *state.Store }

func (p pushSubscriptionSource) PushSubscriptions() ([]notifications.PushSubscription, error) {
	rows, err := p.st.PushSubscriptions()
	if err != nil {
		return nil, err
	}
	out := make([]notifications.PushSubscription, 0, len(rows))
	for _, r := range rows {
		out = append(out, notifications.PushSubscription{
			ID: r.ID, Endpoint: r.Endpoint, P256dh: r.P256dh, Auth: r.Auth,
			DeviceID: r.DeviceID, CreatedAtMs: r.CreatedAtMs,
		})
	}
	return out, nil
}

func (p pushSubscriptionSource) DeletePushSubscription(id string) error {
	return p.st.DeletePushSubscription(id)
}

// deadmanFromState renders and encrypts the box.unreachable farewell, one
// row per stored subscription. The sentence comes from the push catalogue
// and the encryption is the same RFC 8291 construction every live push
// uses — the relay stores it and can read none of it. Each row also carries
// the VAPID Authorization header the push service will demand, signed here
// by the provider's own key because the relay must never hold that key; the
// uplink calls back every six hours, so the signature is re-made long
// before its 24 hours run out.
type deadmanFromState struct {
	st *state.Store
	wp *notifications.WebPush
}

func (d deadmanFromState) DeadmanRows() ([]appuplink.DeadmanRow, error) {
	subs, err := d.st.PushSubscriptions()
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, nil
	}
	title, body, err := notifications.RenderPush(notifications.PushBoxUnreachable, nil)
	if err != nil {
		return nil, fmt.Errorf("render box.unreachable: %w", err)
	}
	payload, err := json.Marshal(map[string]string{"title": title, "body": body})
	if err != nil {
		return nil, err
	}
	rows := make([]appuplink.DeadmanRow, 0, len(subs))
	for _, sub := range subs {
		ct, err := notifications.EncryptForSubscription(sub.P256dh, sub.Auth, payload)
		if err != nil {
			// One unencryptable subscription must not disarm the rest.
			slog.Warn("dead man's switch: skipping a subscription", "id", sub.ID, "err", err)
			continue
		}
		auth, err := d.wp.DeadmanAuthorization(sub.Endpoint)
		if err != nil {
			slog.Warn("dead man's switch: skipping a subscription", "id", sub.ID, "err", err)
			continue
		}
		rows = append(rows, appuplink.DeadmanRow{
			SubscriptionID: sub.ID,
			Endpoint:       sub.Endpoint,
			CT:             ct,
			Auth:           auth,
		})
	}
	return rows, nil
}
