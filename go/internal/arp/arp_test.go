package arp

import (
	"net"
	"os"
	"testing"
)

// stubARPTable replaces the /proc/net/arp reader for one test.
func stubARPTable(t *testing.T, contents string) {
	t.Helper()
	prev := readFile
	readFile = func(string) ([]byte, error) { return []byte(contents), nil }
	t.Cleanup(func() { readFile = prev })
}

// Every case here must bail before the ARP-cache nudge, which is what
// keeps this test hermetic: no case may touch the network. A hostname is
// never resolved, IPv6 has no ARP at all, and the rest are addresses that
// provably cannot appear in an ARP table.
func TestLookupRejectsAddressesThatCannotHaveAnARPEntry(t *testing.T) {
	cases := []struct{ name, host string }{
		{"empty", ""},
		{"hostname", "inverter.local"},
		{"host:port", "192.168.1.5:502"},
		{"ipv6 loopback", "::1"},
		{"ipv6 link-local", "fe80::1"},
		{"ipv6 ula", "fd00::5"},
		{"ipv4 loopback", "127.0.0.1"},
		{"unspecified", "0.0.0.0"},
		{"cgnat tunnel", "100.96.146.117"},
		{"multicast", "224.0.0.251"},
		{"broadcast", "255.255.255.255"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if mac, ok := Lookup(tc.host); ok {
				t.Fatalf("Lookup(%q) = (%q, true), want no match", tc.host, mac)
			}
		})
	}
}

// The gate excludes only what provably cannot resolve. A public address
// stays eligible on purpose: an ISP can route a small block straight onto
// the LAN, and excluding it would silently re-key those devices off their
// MAC and orphan their persistent state.
func TestCanHaveARPEntry(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.42", true},
		{"169.254.10.1", true}, // link-local: no DHCP, still on our segment
		{"8.8.8.8", true},      // public, but could be on our segment
		{"172.32.0.1", true},   // outside RFC1918, still a routable unicast host
		{"0.0.0.0", false},
		{"127.0.0.1", false},
		{"100.64.0.1", false},
		{"100.127.255.254", false},
		{"100.128.0.1", true}, // just past CGNAT, ordinary unicast again
		{"224.0.0.251", false},
		{"239.255.255.250", false},
		{"255.255.255.255", false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip4 := net.ParseIP(tc.ip).To4()
			if ip4 == nil {
				t.Fatalf("ParseIP(%q).To4() = nil", tc.ip)
			}
			if got := canHaveARPEntry(ip4); got != tc.want {
				t.Errorf("canHaveARPEntry(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

const procNetARP = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.42     0x1         0x2         AA:BB:CC:DD:EE:FF     *        eth0
192.168.1.43     0x1         0x0         00:00:00:00:00:00     *        eth0
192.168.1.44     0x1         0x2         aa:bb:cc:dd:ee:01     *        wlan0
`

func TestLookupLinuxParsesProcNetARP(t *testing.T) {
	stubARPTable(t, procNetARP)
	cases := []struct {
		name    string
		ip      string
		wantMAC string
		wantOK  bool
	}{
		// The kernel prints uppercase; device_id comparisons downstream
		// assume lowercase, so normalization here is load-bearing.
		{"match is lowercased", "192.168.1.42", "aa:bb:cc:dd:ee:ff", true},
		{"match on second interface", "192.168.1.44", "aa:bb:cc:dd:ee:01", true},
		{"incomplete entry is not a match", "192.168.1.43", "", false},
		{"absent host", "192.168.1.99", "", false},
		{"header row is not matched", "IP", "", false},
		{"prefix of a listed ip", "192.168.1.4", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mac, ok := lookupLinux(tc.ip)
			if mac != tc.wantMAC || ok != tc.wantOK {
				t.Errorf("lookupLinux(%q) = (%q, %v), want (%q, %v)",
					tc.ip, mac, ok, tc.wantMAC, tc.wantOK)
			}
		})
	}
}

func TestLookupLinuxHandlesUnreadableTable(t *testing.T) {
	prev := readFile
	readFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() { readFile = prev })

	if mac, ok := lookupLinux("192.168.1.42"); ok {
		t.Fatalf("lookupLinux with unreadable table = (%q, true), want no match", mac)
	}
}

func TestParseDarwinARP(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		wantMAC string
		wantOK  bool
	}{
		{
			"typical entry",
			"? (192.168.1.42) at aa:bb:cc:dd:ee:ff on en0 ifscope [ethernet]\n",
			"aa:bb:cc:dd:ee:ff", true,
		},
		{
			// macOS drops leading zeros; the padded form is what the
			// devices table and every comparison downstream expect.
			"single-digit octets are padded",
			"? (192.168.1.42) at 1:2:3:4:5:6 on en0 ifscope [ethernet]\n",
			"01:02:03:04:05:06", true,
		},
		{
			"uppercase is normalized",
			"? (192.168.1.42) at AA:BB:CC:DD:EE:FF on en0\n",
			"aa:bb:cc:dd:ee:ff", true,
		},
		{
			"mac at end of output without trailing field",
			"? (192.168.1.42) at aa:bb:cc:dd:ee:ff",
			"aa:bb:cc:dd:ee:ff", true,
		},
		{
			"incomplete entry",
			"? (192.168.1.42) at (incomplete) on en0 ifscope [ethernet]\n",
			"", false,
		},
		{
			"no entry at all",
			"192.168.1.99 (192.168.1.99) -- no entry\n",
			"", false,
		},
		{"all-zero mac", "? (192.168.1.42) at 00:00:00:00:00:00 on en0\n", "", false},
		{"too few octets", "? (192.168.1.42) at aa:bb:cc on en0\n", "", false},
		{"too many octets", "? (192.168.1.42) at aa:bb:cc:dd:ee:ff:11 on en0\n", "", false},
		{"oversized octet", "? (192.168.1.42) at aaa:bb:cc:dd:ee:ff on en0\n", "", false},
		{"empty octet", "? (192.168.1.42) at aa::cc:dd:ee:ff on en0\n", "", false},
		{"empty output", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mac, ok := parseDarwinARP(tc.out)
			if mac != tc.wantMAC || ok != tc.wantOK {
				t.Errorf("parseDarwinARP(%q) = (%q, %v), want (%q, %v)",
					tc.out, mac, ok, tc.wantMAC, tc.wantOK)
			}
		})
	}
}
