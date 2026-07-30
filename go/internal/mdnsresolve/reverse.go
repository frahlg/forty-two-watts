package mdnsresolve

import (
	"context"
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"
)

// ReverseLookup asks the device at ip to name itself, so a discovered device
// can be shown and stored by its self-broadcast ".local" name rather than a
// DHCP-assigned address. Returns "" when nothing answers — callers treat a
// missing name as ordinary, not as an error.
func ReverseLookup(ctx context.Context, ip string) string {
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
		Name: name, Type: dnsmessage.TypePTR, Class: classQU,
	}}}
	packed, err := msg.Pack()
	if err != nil {
		return ""
	}

	var host string
	_ = exchange(ctx, packed, func(packet []byte) bool {
		host = parsePTRAnswer(packet, qname)
		return host != ""
	})
	return host
}

func parsePTRAnswer(packet []byte, qname string) string {
	var p dnsmessage.Parser
	if _, err := p.Start(packet); err != nil {
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
		if h.Type == dnsmessage.TypePTR && strings.EqualFold(h.Name.String(), qname) {
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
