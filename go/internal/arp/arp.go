// Package arp resolves MAC addresses for IPv4 hosts on the local L2 segment.
// Used to give Modbus TCP / HTTP / on-LAN MQTT devices a hardware-stable
// identity even when they don't expose a serial number in their protocol.
//
// Cross-subnet (L3) addresses can't be resolved — the kernel's ARP table
// only contains entries for hosts the box has talked to on the same L2.
// In that case Lookup returns ("", false) and callers should fall back to
// an endpoint-hash identity.
package arp

import (
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Lookup resolves the MAC address for an IPv4 address. Best-effort: the
// kernel's ARP cache only knows hosts we've talked to recently, so we
// nudge the cache by sending a single TCP probe to a common port (80) on
// the host before reading. The probe is cheap (50 ms timeout) and silent
// on failure — what matters is the SYN packet itself triggering ARP.
//
// Only a bare IPv4 literal is accepted; hostnames are not resolved, so a
// driver configured with a DNS name gets no MAC.
//
// Returns (mac, true) on success or ("", false) if the host can't be
// reached on L2 or isn't on this segment at all.
func Lookup(ipStr string) (string, bool) {
	ip := net.ParseIP(ipStr)
	if ip == nil { return "", false }
	ip4 := ip.To4()
	if ip4 == nil { return "", false }
	// Skip addresses that provably cannot produce an ARP entry, rather
	// than spending 150 ms of dialing to learn nothing. Deliberately NOT
	// limited to RFC1918: a public address can legitimately sit on the
	// local segment (an ISP routing a small block onto the LAN), and
	// excluding it would silently re-key those devices off their MAC.
	if !canHaveARPEntry(ip4) { return "", false }
	// Nudge the ARP cache. We don't care if connect succeeds — the kernel
	// resolves ARP regardless when it tries to send the SYN.
	for _, port := range []string{"80", "502", "1883"} {
		c, _ := net.DialTimeout("tcp", net.JoinHostPort(ipStr, port), 50*time.Millisecond)
		if c != nil { c.Close(); break }
	}
	switch runtime.GOOS {
	case "linux":
		return lookupLinux(ipStr)
	case "darwin":
		return lookupDarwin(ipStr)
	}
	return "", false
}

// canHaveARPEntry reports whether an IPv4 address could ever appear in the
// kernel's ARP table. Only the provably-impossible cases are excluded, so
// that a device on an unusual-but-real local segment keeps its MAC-derived
// identity. ip4 must be the 4-byte form from net.IP.To4.
func canHaveARPEntry(ip4 net.IP) bool {
	switch {
	case ip4[0] == 0: // "this network", including 0.0.0.0
		return false
	case ip4[0] == 127: // loopback never resolves to a hardware address
		return false
	case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
		// Carrier-grade NAT space, where Tailscale and similar tunnels
		// live. Reached over an L3 tun device, which has no L2 to ARP on.
		return false
	case ip4[0] >= 224: // multicast, reserved space, broadcast
		return false
	}
	return true
}

// lookupLinux parses /proc/net/arp.
//
//	IP address       HW type     Flags       HW address            Mask     Device
//	192.168.1.42     0x1         0x2         aa:bb:cc:dd:ee:ff     *        eth0
func lookupLinux(ipStr string) (string, bool) {
	data, err := readFile("/proc/net/arp")
	if err != nil { return "", false }
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 || line == "" { continue }
		fields := strings.Fields(line)
		if len(fields) < 4 { continue }
		if fields[0] != ipStr { continue }
		mac := strings.ToLower(fields[3])
		// Incomplete entries show as "00:00:00:00:00:00".
		if mac == "00:00:00:00:00:00" { return "", false }
		return mac, true
	}
	return "", false
}

// lookupDarwin shells out to /usr/sbin/arp. macOS doesn't expose /proc.
func lookupDarwin(ipStr string) (string, bool) {
	out, err := exec.Command("/usr/sbin/arp", "-n", ipStr).CombinedOutput()
	if err != nil { return "", false }
	return parseDarwinARP(string(out))
}

// parseDarwinARP extracts the MAC from `arp -n <ip>` output:
//
//	? (192.168.1.42) at aa:bb:cc:dd:ee:ff on en0 ifscope [ethernet]
//
// A miss prints "(incomplete)" or "no entry" in the same slot; both fail
// the six-octet check. macOS drops leading zeros ("1:2:3:4:5:6"), so
// octets are padded back to the canonical form callers compare against.
func parseDarwinARP(s string) (string, bool) {
	idx := strings.Index(s, " at ")
	if idx < 0 { return "", false }
	fields := strings.Fields(s[idx+len(" at "):])
	if len(fields) == 0 { return "", false }
	parts := strings.Split(strings.ToLower(fields[0]), ":")
	if len(parts) != 6 { return "", false }
	for i, p := range parts {
		if len(p) == 0 || len(p) > 2 { return "", false }
		if len(p) == 1 { parts[i] = "0" + p }
	}
	mac := strings.Join(parts, ":")
	if mac == "00:00:00:00:00:00" { return "", false }
	return mac, true
}

// readFile is a tiny indirection so tests can stub /proc/net/arp.
var readFile = os.ReadFile
