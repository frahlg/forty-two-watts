package mdnsresolve

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// This file is the fallback described in the package comment: a direct RFC
// 6762 query, used only where avahi-daemon's socket cannot be reached. It is
// deliberately the second choice — see avahi.go for the first.

const mdnsPort = 5353

// mdnsAddr is the RFC 6762 IPv4 multicast group. A var, not a const, so tests
// can aim a query at a loopback responder.
var mdnsAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

// mdnsAddr6 is the RFC 6762 IPv6 multicast group. The interface zone is added
// to a copy for each query because ff02::fb is link-local by definition.
var mdnsAddr6 = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: mdnsPort}

// multicastInterfaces and listenMulticastPacket are vars so tests can select a
// stable interface and responder without touching the host LAN.
var multicastInterfaces = net.Interfaces

// listenQueryPacket opens the ephemeral socket that sends the QU query and
// receives its unicast reply. Binding the multicast group here would prevent
// Linux from delivering a reply sent to the interface's unicast address.
var listenMulticastPacket = openQueryPacket

func openQueryPacket(network string, iface *net.Interface, _ *net.UDPAddr) (*net.UDPConn, error) {
	var local *net.UDPAddr
	switch network {
	case "udp4":
		local = &net.UDPAddr{IP: net.IPv4zero}
	case "udp6":
		local = &net.UDPAddr{IP: net.IPv6unspecified}
	default:
		return nil, fmt.Errorf("mdns: unsupported network %q", network)
	}
	conn, err := net.ListenUDP(network, local)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*net.UDPConn, error) {
		_ = conn.Close()
		return nil, err
	}
	switch network {
	case "udp4":
		packet := ipv4.NewPacketConn(conn)
		if err := packet.SetMulticastInterface(iface); err != nil {
			return closeOnError(fmt.Errorf("set IPv4 multicast interface: %w", err))
		}
		if err := packet.SetMulticastLoopback(false); err != nil {
			return closeOnError(fmt.Errorf("disable IPv4 multicast loopback: %w", err))
		}
	case "udp6":
		packet := ipv6.NewPacketConn(conn)
		if err := packet.SetMulticastInterface(iface); err != nil {
			return closeOnError(fmt.Errorf("set IPv6 multicast interface: %w", err))
		}
		if err := packet.SetMulticastLoopback(false); err != nil {
			return closeOnError(fmt.Errorf("disable IPv6 multicast loopback: %w", err))
		}
	}
	return conn, nil
}

// classQU is IN with the RFC 6762 unicast-response bit set.
const classQU = dnsmessage.Class(0x8001)

// mDNS answer classes may carry the cache-flush bit in the high bit. It is a
// record flag, not a different DNS class.
const classCacheFlush = dnsmessage.Class(0x8000)

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

	lookupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		family int
		addrs  []netip.Addr
		ttl    time.Duration
		err    error
	}
	results := make(chan result, 2)
	go func() {
		addrs, ttl, err := queryIPv4(lookupCtx, packed, name)
		results <- result{family: 4, addrs: addrs, ttl: ttl, err: err}
	}()
	go func() {
		addrs, ttl, err := queryIPv6(lookupCtx, packed, name)
		results <- result{family: 6, addrs: addrs, ttl: ttl, err: err}
	}()

	var v4, v6 result
	for i := 0; i < 2; i++ {
		got := <-results
		if got.family == 4 {
			v4 = got
		} else {
			v6 = got
		}
		if got.family == 4 && got.err == nil && len(got.addrs) > 0 {
			// One family is enough to dial the device. Cancel the other
			// family, then still collect its result before returning so no
			// query goroutine outlives this lookup. Do not cancel on IPv6
			// first: IPv4 remains the preferred address family.
			cancel()
		}
	}

	addrs := appendUnique(nil, v4.addrs...)
	addrs = appendUnique(addrs, v6.addrs...)
	if len(addrs) == 0 {
		return nil, 0, fmt.Errorf("mdns: no usable answer (IPv4: %v; IPv6: %v)", v4.err, v6.err)
	}
	ttl := minAnswerTTL(v4.ttl, v6.ttl)
	return addrs, ttl, nil
}

func queryIPv4(ctx context.Context, packed []byte, name string) ([]netip.Addr, time.Duration, error) {
	ifaces, err := eligibleMulticastInterfaces()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: list IPv4 multicast interfaces: %w", err)
	}
	return queryInterfaces(ctx, packed, name, ifaces, "udp4", mdnsAddr, "")
}

func queryIPv6(ctx context.Context, packed []byte, name string) ([]netip.Addr, time.Duration, error) {
	ifaces, err := eligibleMulticastInterfaces()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: list IPv6 multicast interfaces: %w", err)
	}
	return queryInterfaces(ctx, packed, name, ifaces, "udp6", mdnsAddr6, "interface")
}

func eligibleMulticastInterfaces() ([]net.Interface, error) {
	ifaces, err := multicastInterfaces()
	if err != nil {
		return nil, err
	}
	eligible := make([]net.Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		if !validMulticastInterface(&iface) {
			continue
		}
		eligible = append(eligible, iface)
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no active non-loopback multicast interface")
	}
	return eligible, nil
}

func validMulticastInterface(iface *net.Interface) bool {
	return iface != nil && iface.Index > 0 && iface.Name != "" &&
		iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagMulticast != 0 &&
		iface.Flags&net.FlagLoopback == 0
}

func queryInterfaces(ctx context.Context, packed []byte, name string, ifaces []net.Interface, network string, group *net.UDPAddr, zoneMode string) ([]netip.Addr, time.Duration, error) {
	if len(ifaces) == 0 {
		return nil, 0, fmt.Errorf("no multicast interface")
	}
	type result struct {
		addrs []netip.Addr
		ttl   time.Duration
		err   error
	}
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(ifaces))
	for _, iface := range ifaces {
		iface := iface
		go func() {
			target := *group
			if zoneMode == "interface" && target.IP.IsLinkLocalMulticast() {
				target.Zone = iface.Name
			}
			_, _, err := exchange(queryCtx, packed, &target, network, &iface, func() (*net.UDPConn, error) {
				return listenMulticastPacket(network, &iface, &target)
			}, func(packet []byte, source *net.UDPAddr) bool {
				got, gotTTL, ok := parseAddrAnswer(packet, name+".", source, network, &iface)
				if !ok {
					return false
				}
				results <- result{addrs: got, ttl: gotTTL}
				return true
			})
			if err != nil {
				results <- result{err: err}
			}
		}()
	}

	var (
		addrs    []netip.Addr
		ttl      time.Duration
		firstErr error
	)
	for i := 0; i < len(ifaces); i++ {
		got := <-results
		if got.err != nil {
			if firstErr == nil {
				firstErr = got.err
			}
			continue
		}
		addrs = appendUnique(addrs, got.addrs...)
		ttl = minAnswerTTL(ttl, got.ttl)
		if len(got.addrs) > 0 {
			cancel()
		}
	}
	if len(addrs) == 0 {
		if firstErr == nil {
			firstErr = fmt.Errorf("no usable answer")
		}
		return nil, 0, firstErr
	}
	return addrs, ttl, nil
}

// exchange sends one multicast query and feeds every reply to handle until it
// accepts one or the deadline passes. Closing the socket on context cancel is
// important: a cancelled family/interface query must not linger until the
// original read deadline.
type packetInfo struct {
	ifIndex int
	dst     net.IP
}

type packetReadFunc func([]byte) (int, *net.UDPAddr, *packetInfo, error)

func preparePacketReader(network string, conn *net.UDPConn) (packetReadFunc, error) {
	switch network {
	case "udp4":
		packet := ipv4.NewPacketConn(conn)
		if err := packet.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true); err != nil {
			return nil, fmt.Errorf("enable IPv4 packet info: %w", err)
		}
		return func(buf []byte) (int, *net.UDPAddr, *packetInfo, error) {
			n, cm, source, err := packet.ReadFrom(buf)
			if err != nil {
				return 0, nil, nil, err
			}
			udpSource, _ := source.(*net.UDPAddr)
			if cm == nil {
				return n, udpSource, nil, nil
			}
			return n, udpSource, &packetInfo{ifIndex: cm.IfIndex, dst: append(net.IP(nil), cm.Dst...)}, nil
		}, nil
	case "udp6":
		packet := ipv6.NewPacketConn(conn)
		if err := packet.SetControlMessage(ipv6.FlagInterface|ipv6.FlagDst, true); err != nil {
			return nil, fmt.Errorf("enable IPv6 packet info: %w", err)
		}
		return func(buf []byte) (int, *net.UDPAddr, *packetInfo, error) {
			n, cm, source, err := packet.ReadFrom(buf)
			if err != nil {
				return 0, nil, nil, err
			}
			udpSource, _ := source.(*net.UDPAddr)
			if cm == nil {
				return n, udpSource, nil, nil
			}
			return n, udpSource, &packetInfo{ifIndex: cm.IfIndex, dst: append(net.IP(nil), cm.Dst...)}, nil
		}, nil
	default:
		return nil, fmt.Errorf("unsupported packet network %q", network)
	}
}

func validPacketInfo(info *packetInfo, iface *net.Interface, network string) bool {
	if info == nil || iface == nil || iface.Index <= 0 || info.ifIndex != iface.Index || info.dst == nil {
		return false
	}
	dst, ok := netip.AddrFromSlice(info.dst)
	if !ok {
		return false
	}
	dst = dst.Unmap()
	if dst.IsUnspecified() || dst.IsMulticast() {
		return false
	}
	switch network {
	case "udp4":
		return dst.Is4()
	case "udp6":
		return dst.Is6()
	default:
		return false
	}
}

func exchange(ctx context.Context, packed []byte, target *net.UDPAddr, network string, iface *net.Interface, open func() (*net.UDPConn, error), handle func([]byte, *net.UDPAddr) bool) ([]netip.Addr, time.Duration, error) {
	conn, err := open()
	if err != nil {
		return nil, 0, fmt.Errorf("mdns: open query socket: %w", err)
	}
	readPacket, err := preparePacketReader(network, conn)
	if err != nil {
		_ = conn.Close()
		return nil, 0, fmt.Errorf("mdns: prepare query socket: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	defer conn.Close()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	deadline := now().Add(queryTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, 0, fmt.Errorf("mdns: set query deadline: %w", err)
	}
	if _, err := conn.WriteToUDP(packed, target); err != nil {
		return nil, 0, fmt.Errorf("mdns: send query: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		n, source, info, err := readPacket(buf)
		if err != nil {
			return nil, 0, fmt.Errorf("mdns: no usable answer: %w", err)
		}
		if !validPacketInfo(info, iface, network) {
			continue
		}
		if handle(buf[:n], source) {
			return nil, 0, nil
		}
	}
}

func parseAddrAnswer(packet []byte, qname string, source *net.UDPAddr, network string, iface *net.Interface) ([]netip.Addr, time.Duration, bool) {
	if !validMulticastInterface(iface) || !validMDNSSource(source, network, iface) {
		return nil, 0, false
	}
	var p dnsmessage.Parser
	header, err := p.Start(packet)
	if err != nil || !header.Response || header.RCode != dnsmessage.RCodeSuccess {
		return nil, 0, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, 0, false
	}
	var addrs []netip.Addr
	ttl := maxTTL
	// A malformed record abandons the whole packet rather than keeping the
	// addresses read before it: once the parser desynchronises, nothing after
	// that point is trustworthy, and a truncated answer must not be allowed to
	// populate the cache. The label is only the normal end-of-section exit.
parse:
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			if err == dnsmessage.ErrSectionDone {
				break parse
			}
			return nil, 0, false
		}
		if !strings.EqualFold(h.Name.String(), qname) ||
			h.Class&^classCacheFlush != dnsmessage.ClassINET ||
			!answerTypeMatchesNetwork(h.Type, network) {
			if err := p.SkipAnswer(); err != nil {
				return nil, 0, false
			}
			continue
		}
		switch h.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return nil, 0, false
			}
			addrs = append(addrs, netip.AddrFrom4(r.A))
		case dnsmessage.TypeAAAA:
			r, err := p.AAAAResource()
			if err != nil {
				return nil, 0, false
			}
			// Unmap so a v4-mapped AAAA dials as plain IPv4.
			addr := netip.AddrFrom16(r.AAAA).Unmap()
			if addr.Is6() && addr.IsLinkLocalUnicast() {
				// A link-local address without a zone is not a safe dial
				// target: the kernel cannot know which interface to use.
				if iface.Name == "" {
					continue
				}
				addr = addr.WithZone(iface.Name)
			}
			addrs = append(addrs, addr)
		default:
			if err := p.SkipAnswer(); err != nil {
				return nil, 0, false
			}
			continue
		}
		if d := time.Duration(h.TTL) * time.Second; d < ttl {
			ttl = d
		}
	}
	return finishAnswer(addrs, ttl)
}

func answerTypeMatchesNetwork(typ dnsmessage.Type, network string) bool {
	switch network {
	case "udp4":
		return typ == dnsmessage.TypeA
	case "udp6":
		return typ == dnsmessage.TypeAAAA
	default:
		return false
	}
}

func validMDNSSource(source *net.UDPAddr, network string, iface *net.Interface) bool {
	// RFC 6762 requires every mDNS response to use the well-known source
	// port, including a response sent directly to a QU query's ephemeral port.
	if source == nil || source.Port != mdnsPort || source.IP == nil || !validMulticastInterface(iface) {
		return false
	}
	ip, ok := netip.AddrFromSlice(source.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	switch network {
	case "udp4":
		if !ip.Is4() {
			return false
		}
	case "udp6":
		if !ip.Is6() {
			return false
		}
		if source.Zone != "" && source.Zone != iface.Name {
			return false
		}
	default:
		return false
	}
	return true
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

func appendUnique(dst []netip.Addr, src ...netip.Addr) []netip.Addr {
	for _, candidate := range src {
		seen := false
		for _, existing := range dst {
			if existing == candidate {
				seen = true
				break
			}
		}
		if !seen {
			dst = append(dst, candidate)
		}
	}
	return dst
}

func minAnswerTTL(values ...time.Duration) time.Duration {
	var min time.Duration
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if min == 0 || value < min {
			min = value
		}
	}
	if min == 0 {
		return minTTL
	}
	return min
}
