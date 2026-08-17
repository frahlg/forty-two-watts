package scanner

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func mustDNSName(t *testing.T, value string) dnsmessage.Name {
	t.Helper()
	name, err := dnsmessage.NewName(value)
	if err != nil {
		t.Fatalf("NewName(%q): %v", value, err)
	}
	return name
}

func TestParsePTRAnswer(t *testing.T) {
	qname := "15.1.168.192.in-addr.arpa."
	source := &net.UDPAddr{IP: net.ParseIP("192.168.1.15"), Port: mdnsPort}
	packet := packPTRAnswer(t, qname, dnsmessage.Header{Response: true}, dnsmessage.ClassINET)

	if got := parsePTRAnswer(packet, qname, source); got != "inverter.local" {
		t.Fatalf("parsePTRAnswer = %q, want inverter.local", got)
	}
	flushPacket := packPTRAnswer(t, qname, dnsmessage.Header{Response: true}, dnsmessage.ClassINET|mdnsCacheFlushBit)
	if got := parsePTRAnswer(flushPacket, qname, source); got != "inverter.local" {
		t.Fatalf("cache-flush answer = %q, want inverter.local", got)
	}
	if got := parsePTRAnswer(packet, "99.1.168.192.in-addr.arpa.", source); got != "" {
		t.Fatalf("mismatched answer = %q, want empty", got)
	}
	if got := parsePTRAnswer([]byte{1, 2, 3}, qname, source); got != "" {
		t.Fatalf("garbage answer = %q, want empty", got)
	}
	if got := parsePTRAnswer(packet, qname, nil); got != "" {
		t.Fatalf("nil source answer = %q, want empty", got)
	}
	wrongPort := &net.UDPAddr{IP: source.IP, Port: 9999}
	if got := parsePTRAnswer(packet, qname, wrongPort); got != "" {
		t.Fatalf("wrong source port answer = %q, want empty", got)
	}
	queryPacket := packPTRAnswer(t, qname, dnsmessage.Header{}, dnsmessage.ClassINET)
	if got := parsePTRAnswer(queryPacket, qname, source); got != "" {
		t.Fatalf("query packet answer = %q, want empty", got)
	}
	errorPacket := packPTRAnswer(t, qname, dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeNameError}, dnsmessage.ClassINET)
	if got := parsePTRAnswer(errorPacket, qname, source); got != "" {
		t.Fatalf("error response answer = %q, want empty", got)
	}
	wrongClassPacket := packPTRAnswer(t, qname, dnsmessage.Header{Response: true}, dnsmessage.Class(3))
	if got := parsePTRAnswer(wrongClassPacket, qname, source); got != "" {
		t.Fatalf("wrong class answer = %q, want empty", got)
	}
}

func packPTRAnswer(t *testing.T, qname string, header dnsmessage.Header, class dnsmessage.Class) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header:    header,
		Questions: []dnsmessage.Question{{Name: mustDNSName(t, qname), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: mustDNSName(t, qname), Type: dnsmessage.TypePTR, Class: class, TTL: 120},
			Body:   &dnsmessage.PTRResource{PTR: mustDNSName(t, "inverter.local.")},
		}},
	}
	packet, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}
