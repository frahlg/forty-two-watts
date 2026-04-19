// Package mqtt provides an MQTT capability wrapper for per-driver binding.
package mqtt

import (
	"fmt"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/frahlg/forty-two-watts/go/internal/drivers"
)

// Capability wraps a paho client to match drivers.MQTTCap.
type Capability struct {
	client  paho.Client
	handler paho.MessageHandler

	mu       sync.Mutex
	incoming []drivers.MQTTMessage
}

// Dial connects to an MQTT broker and returns a Capability.
func Dial(host string, port int, username, password, clientID string) (*Capability, error) {
	cap := &Capability{}
	// Shared message handler reused for every subscription + the default
	// route. paho.mqtt.golang only falls back to DefaultPublishHandler
	// for messages that don't match any per-subscription callback, and
	// even then the routing behaviour for *live* (non-retained)
	// publishes has historically been flaky with a nil subscription
	// callback — we caught CTEK Chargestorm's CTEK/.../em + /status
	// live publishes dropping on the floor while the retained CCU/...
	// topics routed fine. Passing this exact handler to every Subscribe
	// call removes the ambiguity.
	cap.handler = func(_ paho.Client, m paho.Message) {
		cap.mu.Lock()
		cap.incoming = append(cap.incoming, drivers.MQTTMessage{
			Topic:   m.Topic(),
			Payload: string(m.Payload()),
		})
		cap.mu.Unlock()
	}
	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", host, port)).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetDefaultPublishHandler(cap.handler)
	if username != "" { opts.SetUsername(username) }
	if password != "" { opts.SetPassword(password) }
	cap.client = paho.NewClient(opts)
	if tok := cap.client.Connect(); tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
		return nil, tok.Error()
	}
	return cap, nil
}

// Close disconnects the client. Returns error so the signature matches
// drivers.MQTTCap — that lets the registry call Close() uniformly at
// driver teardown. Without it a stale paho client stays connected
// under the same clientID; the broker kicks the newer one on the
// next Dial and subscribe ACKs to the new client race with the old
// disconnect, which is what caused ferroamp to go silent after a
// POST /api/drivers/ferroamp/restart on 2026-04-17.
func (c *Capability) Close() error {
	c.client.Disconnect(250)
	return nil
}

// Subscribe — implements drivers.MQTTCap.
//
// Passes the shared handler (same one that's wired to the default
// publish handler on the client) so live publishes are reliably
// delivered. paho's internal routing treats a nil subscription
// callback as "no handler" and has shipped with firmware-specific
// quirks where live messages on that topic stop reaching the default
// handler — retained messages still came through, which is how CTEK's
// /em + /status silently dropped while /Configuration + /Version
// worked. See the note on the Dial handler setup.
func (c *Capability) Subscribe(topic string) error {
	tok := c.client.Subscribe(topic, 0, c.handler)
	tok.WaitTimeout(5 * time.Second)
	return tok.Error()
}

// Publish — implements drivers.MQTTCap.
func (c *Capability) Publish(topic string, payload []byte) error {
	tok := c.client.Publish(topic, 0, false, payload)
	tok.WaitTimeout(5 * time.Second)
	return tok.Error()
}

// PopMessages — implements drivers.MQTTCap.
func (c *Capability) PopMessages() []drivers.MQTTMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.incoming
	c.incoming = nil
	return out
}
