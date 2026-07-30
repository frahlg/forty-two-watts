// Package mdnsresolve resolves RFC 6762 ".local" host names over multicast
// DNS and provides a dialer that uses it.
//
// Go never does this itself. net/conf.go routes a ".local" lookup to libc only
// when cgo is available, and every FTW build sets CGO_ENABLED=0, so the pure Go
// resolver is always selected: it reads /etc/resolv.conf and sends a *unicast*
// query to the site router, which has no idea what "inverter.local" is. That
// holds on every base image and every libc — musl and glibc alike — so
// resolving here is the only portable fix.
//
// Only ".local" names take this path. Literal IPs and ordinary DNS names are
// handed straight to the standard dialer.
package mdnsresolve

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// mdnsAddr is the RFC 6762 IPv4 multicast group. A var, not a const, so tests
// can aim a query at a loopback responder.
var mdnsAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// listenPacket opens the ephemeral socket a query is sent from. Replaced in
// tests. It deliberately does NOT bind port 5353: avahi-daemon already owns
// that on the host, and the QU bit below asks responders to reply directly to
// this socket instead.
var listenPacket = func() (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
}

// now is swappable so cache-expiry tests do not have to sleep.
var now = time.Now

const (
	// queryTimeout matches the budget the scanner already uses for its reverse
	// lookups — long enough for a sleepy device, short enough that a driver
	// dial does not stall a control tick.
	queryTimeout = 900 * time.Millisecond

	// classQU is IN with the RFC 6762 unicast-response bit set.
	classQU = dnsmessage.Class(0x8001)

	// A responder's TTL is advisory here. The floor stops a device that
	// advertises a very short TTL from turning every Modbus reconnect into a
	// multicast storm; the ceiling keeps a DHCP move from taking effect
	// arbitrarily late, which is the whole point of binding by name.
	minTTL = 30 * time.Second
	maxTTL = 120 * time.Second

	// negativeTTL is deliberately short: a device that was off when we first
	// looked should become reachable soon after it boots.
	negativeTTL = 5 * time.Second
)

type cacheEntry struct {
	addrs   []netip.Addr // empty means a cached negative answer
	expires time.Time
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

// IsLocal reports whether host is a ".local" name that mDNS should resolve.
// A literal IP is never one, so a configured "192.168.1.5" keeps the plain
// dial path and never touches the network for resolution.
func IsLocal(host string) bool {
	if host == "" || net.ParseIP(host) != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".local")
}

func canonical(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

func cacheLookup(key string) ([]netip.Addr, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := cache[key]
	if !ok || now().After(entry.expires) {
		return nil, false
	}
	return entry.addrs, true
}

func cacheStore(key string, addrs []netip.Addr, ttl time.Duration) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache[key] = cacheEntry{addrs: addrs, expires: now().Add(ttl)}
}

// Flush drops every cached answer. Tests use it; nothing in production does.
func Flush() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = map[string]cacheEntry{}
}

// Lookup resolves a ".local" name to its advertised addresses.
func Lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	key := canonical(name)
	if addrs, ok := cacheLookup(key); ok {
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no mDNS responder for %s (cached)", name)
		}
		return addrs, nil
	}

	addrs, ttl, err := queryAddrs(ctx, key)
	if err != nil || len(addrs) == 0 {
		cacheStore(key, nil, negativeTTL)
		if err == nil {
			err = fmt.Errorf("no mDNS responder for %s", name)
		}
		return nil, err
	}

	cacheStore(key, addrs, ttl)
	// Logged on a cache miss only, so this is at most one line per TTL per
	// device rather than one per reconnect.
	slog.Info("resolved host over mDNS", "host", key, "addr", addrs[0].String(), "ttl", ttl)
	return addrs, nil
}

func queryAddrs(ctx context.Context, name string) ([]netip.Addr, time.Duration, error) {
	qname, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: bad name %q: %w", name, err)
	}
	// One packet, two questions. RFC 6762 §5.2 allows it and it saves a round
	// trip on dual-stack devices.
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{
		{Name: qname, Type: dnsmessage.TypeA, Class: classQU},
		{Name: qname, Type: dnsmessage.TypeAAAA, Class: classQU},
	}}
	packed, err := msg.Pack()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: pack query: %w", err)
	}

	var (
		addrs []netip.Addr
		ttl   time.Duration
	)
	err = exchange(ctx, packed, func(packet []byte) bool {
		got, gotTTL, ok := parseAddrAnswer(packet, name+".")
		if !ok {
			return false
		}
		addrs, ttl = got, gotTTL
		return true
	})
	if err != nil {
		return nil, 0, err
	}
	return addrs, ttl, nil
}

// exchange sends one multicast query and feeds every reply to handle until it
// accepts one or the deadline passes.
func exchange(ctx context.Context, packed []byte, handle func([]byte) bool) error {
	conn, err := listenPacket()
	if err != nil {
		return fmt.Errorf("mdns: open socket: %w", err)
	}
	defer conn.Close()

	deadline := now().Add(queryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("mdns: set deadline: %w", err)
	}
	if _, err := conn.WriteToUDP(packed, mdnsAddr); err != nil {
		return fmt.Errorf("mdns: send query: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return fmt.Errorf("mdns: no usable answer: %w", err)
		}
		if handle(buf[:n]) {
			return nil
		}
	}
}

func parseAddrAnswer(packet []byte, qname string) ([]netip.Addr, time.Duration, bool) {
	var p dnsmessage.Parser
	if _, err := p.Start(packet); err != nil {
		return nil, 0, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, 0, false
	}
	var addrs []netip.Addr
	ttl := maxTTL
	// Labelled so a parse error inside the type switch abandons the whole
	// packet: once the parser desynchronises, every later record is suspect.
parse:
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			break parse
		}
		if !strings.EqualFold(h.Name.String(), qname) {
			if err := p.SkipAnswer(); err != nil {
				break parse
			}
			continue
		}
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				break parse
			}
			addrs = append(addrs, netip.AddrFrom4(r.A))
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				break parse
			}
			// Unmap so a v4-mapped AAAA dials as plain IPv4.
			addrs = append(addrs, netip.AddrFrom16(r.AAAA).Unmap())
		default:
			if err := p.SkipAnswer(); err != nil {
				break parse
			}
			continue
		}
		if d := time.Duration(h.TTL) * time.Second; d < ttl {
			ttl = d
		}
	}
	return finishAnswer(addrs, ttl)
}

func finishAnswer(addrs []netip.Addr, ttl time.Duration) ([]netip.Addr, time.Duration, bool) {
	if len(addrs) == 0 {
		return nil, 0, false
	}
	switch {
	case ttl < minTTL:
		ttl = minTTL
	case ttl > maxTTL:
		ttl = maxTTL
	}
	return addrs, ttl, true
}

// Dialer dials TCP addresses, resolving ".local" host names over mDNS first.
// The embedded net.Dialer carries timeout and keep-alive; anything that is not
// a ".local" name is handed straight to it.
//
// Resolution happens per dial, not once at startup. That is what makes binding
// a device by name survive a DHCP lease change: callers that rebuild their
// connection from the original address string pick up the new IP on reconnect.
type Dialer struct {
	net.Dialer
}

// DialContext resolves address if it names a ".local" host, then dials it.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !IsLocal(host) {
		return d.Dialer.DialContext(ctx, network, address)
	}

	addrs, err := Lookup(ctx, host)
	if err != nil {
		// Name the mechanism. Without this the operator sees a bare dial
		// failure and has no way to tell that resolution was the reason.
		slog.Warn("mDNS resolution failed; check the device is on this LAN and the container uses host networking",
			"host", host, "err", err)
		return nil, fmt.Errorf("resolve %s over mDNS: %w", host, err)
	}

	var firstErr error
	for _, a := range addrs {
		conn, err := d.Dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("dial %s over mDNS: %w", host, firstErr)
}

// Dial is the context-free form, for callers that have no context to pass.
func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	ctx := context.Background()
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	return d.DialContext(ctx, network, address)
}

// DialContext dials with default settings.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var d Dialer
	return d.DialContext(ctx, network, address)
}

// DialTimeout mirrors net.DialTimeout with mDNS resolution added.
func DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	d := Dialer{Dialer: net.Dialer{Timeout: timeout}}
	return d.Dial(network, address)
}
