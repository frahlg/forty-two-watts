package scanner

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/srcfl/ftw/go/internal/mdnsresolve"
)

const (
	mdnsPort          = 5353
	mdnsCacheFlushBit = dnsmessage.Class(0x8000)
)

var mdnsAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

// reverseMDNS sends a reverse PTR query with the QU bit set so devices reply
// directly to our ephemeral socket instead of requiring a bind to port 5353.
func reverseMDNS(ctx context.Context, ip string) string {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return ""
	}
	qname := fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", v4[3], v4[2], v4[1], v4[0])
	name, err := dnsmessage.NewName(qname)
	if err != nil {
		return ""
	}
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{{
		Name: name, Type: dnsmessage.TypePTR, Class: dnsmessage.Class(0x8001),
	}}}
	packed, err := msg.Pack()
	if err != nil {
		return ""
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return ""
	}
	defer conn.Close()
	deadline := time.Now().Add(900 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.WriteToUDP(packed, mdnsAddr); err != nil {
		return ""
	}
	buf := make([]byte, 1500)
	for {
		n, source, err := conn.ReadFromUDP(buf)
		if err != nil {
			return ""
		}
		if host := parsePTRAnswer(buf[:n], qname, source); host != "" {
			return host
		}
	}
}

// isLocalName reports whether name is an RFC 6762 ".local" name.
func isLocalName(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(name, ".")), ".local")
}

// hostLabel returns the first DNS label of name, lowercased.
func hostLabel(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return strings.ToLower(name)
}

// verifiedLocalName turns a host label into a ".local" name the device has
// confirmed resolves back to its scanned address.
//
// A reverse PTR query is the obvious way to learn a device's mDNS name, and
// an unreliable one: RFC 6762 makes in-addr.arpa mapping optional, and plenty
// of responders publish a forward A record without ever answering a reverse
// query. A Sourceful Zap is one — it answers "zap.local" all day and returns
// nothing for 141.1.168.192.in-addr.arpa.
//
// Take the label from whatever name we have, including a reverse mDNS PTR,
// ask for "<label>.local" forward, and keep the answer only when it resolves
// back to the address we started from. That last check is the point of the
// function: without it an unrelated responder could decide what FTW dials.
func verifiedLocalName(ctx context.Context, label, ip string) string {
	want, err := netip.ParseAddr(ip)
	if err != nil || label == "" {
		return ""
	}
	candidate := label + ".local"
	addrs, err := mdnsresolve.Lookup(ctx, candidate)
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if a.Unmap() == want.Unmap() {
			return candidate
		}
	}
	return ""
}

func parsePTRAnswer(packet []byte, qname string, source *net.UDPAddr) string {
	if source == nil || source.Port != mdnsPort {
		return ""
	}
	var p dnsmessage.Parser
	header, err := p.Start(packet)
	if err != nil || !header.Response || header.RCode != dnsmessage.RCodeSuccess {
		return ""
	}
	if err := p.SkipAllQuestions(); err != nil {
		return ""
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			return ""
		}
		if h.Type == dnsmessage.TypePTR &&
			h.Class&^mdnsCacheFlushBit == dnsmessage.ClassINET &&
			strings.EqualFold(h.Name.String(), qname) {
			ptr, err := p.PTRResource()
			if err != nil {
				return ""
			}
			return strings.TrimSuffix(ptr.PTR.String(), ".")
		}
		if err := p.SkipAnswer(); err != nil {
			return ""
		}
	}
}
