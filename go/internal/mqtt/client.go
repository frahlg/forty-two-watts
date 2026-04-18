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
	client paho.Client

	mu       sync.Mutex
	incoming []drivers.MQTTMessage
}

// Dial connects to an MQTT broker and returns a Capability.
func Dial(host string, port int, username, password, clientID string) (*Capability, error) {
	cap := &Capability{}
	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", host, port)).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
			cap.mu.Lock()
			cap.incoming = append(cap.incoming, drivers.MQTTMessage{
				Topic:   m.Topic(),
				Payload: string(m.Payload()),
			})
			cap.mu.Unlock()
		})
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
func (c *Capability) Subscribe(topic string) error {
	tok := c.client.Subscribe(topic, 0, nil)
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
