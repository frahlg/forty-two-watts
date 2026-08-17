package drivers

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsTestProxy struct {
	listener net.Listener
	hits     atomic.Int32
	hosts    chan string
}

func newWSTestProxy(t *testing.T) *wsTestProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &wsTestProxy{listener: listener, hosts: make(chan string, 8)}
	t.Cleanup(func() { _ = listener.Close() })
	go p.serve(t)
	return p
}

func (p *wsTestProxy) serve(t *testing.T) {
	t.Helper()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(conn)
	}
}

func (p *wsTestProxy) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	connectReq, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	p.hits.Add(1)
	p.hosts <- connectReq.Host
	if connectReq.Method != http.MethodConnect {
		return
	}
	_, _ = fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	upgradeReq, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	key := upgradeReq.Header.Get("Sec-WebSocket-Key")
	sum := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	_, _ = fmt.Fprintf(conn,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		accept,
	)
	// The client closes the connection after the handshake test. Keep the
	// tunnel open until then so Gorilla can finish Dial successfully.
	_, _ = reader.ReadByte()
}

func (p *wsTestProxy) proxyURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("http://" + p.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (p *wsTestProxy) nextHost(t *testing.T) string {
	t.Helper()
	select {
	case host := <-p.hosts:
		return host
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive CONNECT")
		return ""
	}
}

func TestGorillaWSLocalDestinationBypassesProxy(t *testing.T) {
	proxy := newWSTestProxy(t)
	proxyURL := proxy.proxyURL(t)

	for _, allow := range []bool{false, true} {
		name := "opt-in off"
		if allow {
			name = "opt-in on"
		}
		t.Run(name, func(t *testing.T) {
			dialer := newGorillaWSDialer(allow, http.ProxyURL(proxyURL))
			baseDial := dialer.NetDialContext
			dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address == "inverter.local:80" {
					return nil, errors.New("test blocked direct local dial")
				}
				return baseDial(ctx, network, address)
			}
			conn, _, err := dialer.Dial("ws://inverter.local/v1", nil)
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil {
				t.Fatal("local WebSocket unexpectedly completed its blocked direct dial")
			}
			if got := proxy.hits.Load(); got != 0 {
				t.Fatalf("local WebSocket reached proxy %d times, want 0", got)
			}
		})
	}

	t.Run("ordinary host still uses the proxy", func(t *testing.T) {
		dialer := newGorillaWSDialer(false, http.ProxyURL(proxyURL))
		conn, _, err := dialer.Dial("ws://ordinary.example/v1", nil)
		if err != nil {
			t.Fatalf("ordinary WebSocket failed: %v", err)
		}
		_ = conn.Close()
		if got := proxy.hits.Load(); got != 1 {
			t.Fatalf("ordinary WebSocket total proxy hits = %d, want 1", got)
		}
		if host := proxy.nextHost(t); host != "ordinary.example:80" {
			t.Fatalf("proxy CONNECT host = %q, want ordinary.example:80", host)
		}
	})
}
