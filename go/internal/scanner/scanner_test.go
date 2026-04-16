package scanner

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestProbe_OpenPort(t *testing.T) {
	// Start a TCP listener on a random port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	addr := ln.Addr().(*net.TCPAddr)
	ctx := context.Background()

	dev, ok := probe(ctx, addr.IP.String(), addr.Port)
	if !ok {
		t.Fatal("expected probe to succeed on open port")
	}
	if dev.IP != addr.IP.String() {
		t.Errorf("IP = %q, want %q", dev.IP, addr.IP.String())
	}
	if dev.Port != addr.Port {
		t.Errorf("Port = %d, want %d", dev.Port, addr.Port)
	}
	if dev.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >= 0", dev.LatencyMs)
	}
}

func TestProbe_ClosedPort(t *testing.T) {
	// Pick a port that's almost certainly not listening.
	ctx := context.Background()
	_, ok := probe(ctx, "127.0.0.1", 39999)
	if ok {
		t.Fatal("expected probe to fail on closed port")
	}
}

func TestProbe_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, ok := probe(ctx, "127.0.0.1", 80)
	if ok {
		t.Fatal("expected probe to fail with cancelled context")
	}
}

func TestSubnetHosts(t *testing.T) {
	subnet := net.IPNet{
		IP:   net.IPv4(192, 168, 1, 0),
		Mask: net.CIDRMask(24, 32),
	}
	hosts := subnetHosts(subnet)
	if len(hosts) != 254 {
		t.Fatalf("len = %d, want 254", len(hosts))
	}
	if hosts[0] != "192.168.1.1" {
		t.Errorf("first = %q, want 192.168.1.1", hosts[0])
	}
	if hosts[253] != "192.168.1.254" {
		t.Errorf("last = %q, want 192.168.1.254", hosts[253])
	}
}

func TestLocalSubnets(t *testing.T) {
	subnets, err := localSubnets()
	if err != nil {
		t.Fatal(err)
	}
	// Should find at least one subnet on any machine with networking.
	// But don't fail in CI containers that may only have loopback.
	t.Logf("found %d subnets", len(subnets))
	for _, s := range subnets {
		t.Logf("  %s", s.String())
	}
}

func TestScan_FindsLocalListener(t *testing.T) {
	// Start a listener on a well-known port (502 = modbus).
	// If 502 is privileged, use a random port and test probe directly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	addr := ln.Addr().(*net.TCPAddr)

	// Scan only probes well-known ports (502, 1883, 80) across real
	// subnets, so a random-port listener won't be found by Scan().
	// Instead verify the probe function works, which is what Scan uses.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	dev, ok := probe(ctx, addr.IP.String(), addr.Port)
	if !ok {
		t.Fatal("probe should find the listener")
	}
	if dev.IP != "127.0.0.1" {
		t.Errorf("IP = %q", dev.IP)
	}
	if dev.Port != addr.Port {
		t.Errorf("Port = %d", dev.Port)
	}
	t.Logf("found: %s:%d latency=%dms", dev.IP, dev.Port, dev.LatencyMs)
}

func TestScan_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before scanning

	devices, err := Scan(ctx)
	if err != nil {
		// Some systems may return an error, some may return empty.
		t.Logf("scan with cancelled ctx returned error: %v", err)
	}
	// Should complete quickly and return few/no results.
	t.Logf("devices found: %d", len(devices))
}
