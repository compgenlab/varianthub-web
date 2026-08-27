package callback

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The addresses a user-supplied URL must never be allowed to reach. Each of
// these is a real way to turn this service into a proxy for its own network.
func TestReachableRefusesTheInsideOfTheNetwork(t *testing.T) {
	blocked := []struct{ ip, why string }{
		{"127.0.0.1", "loopback"},
		{"127.1.2.3", "loopback is the whole of 127.0.0.0/8, not one address"},
		{"0.0.0.0", "unspecified, which routes to localhost on most stacks"},
		{"169.254.169.254", "cloud instance metadata — credentials"},
		{"169.254.170.2", "ECS task metadata, the one people forget"},
		{"10.1.2.3", "RFC1918"},
		{"172.16.5.4", "RFC1918"},
		{"192.168.1.1", "RFC1918"},
		{"100.64.0.1", "carrier-grade NAT — a VPC or pod network is often numbered here"},
		{"198.18.0.1", "benchmarking"},
		{"192.0.2.5", "TEST-NET-1"},
		{"240.0.0.1", "reserved"},
		{"::1", "IPv6 loopback"},
		{"fe80::1", "IPv6 link-local"},
		{"fc00::1", "IPv6 unique-local"},
		{"::ffff:127.0.0.1", "IPv4-mapped loopback, which must not slip past as a v6 address"},
		{"::ffff:169.254.169.254", "IPv4-mapped metadata"},
		{"::ffff:10.0.0.1", "IPv4-mapped RFC1918"},
		{"64:ff9b::7f00:1", "NAT64 — a v4 address wearing a v6 hat"},
		{"224.0.0.1", "multicast"},
	}
	for _, c := range blocked {
		if Reachable(net.ParseIP(c.ip)) {
			t.Errorf("%s is reachable; it must not be (%s)", c.ip, c.why)
		}
	}

	for _, ip := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1::1"} {
		if !Reachable(net.ParseIP(ip)) {
			t.Errorf("%s is not reachable; ordinary public addresses must be", ip)
		}
	}
	if Reachable(nil) {
		t.Error("a nil address is reachable")
	}
}

// The dialer is where the rule is enforced, and refusing has to name what
// happened — an operator reading "connection refused" would go looking at the
// network.
func TestDialerRefusesALoopbackTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	d := &Dialer{}
	_, err := d.DialContext(context.Background(), "tcp",
		strings.TrimPrefix(srv.URL, "http://"))
	if err == nil {
		t.Fatal("the dialer connected to a loopback server")
	}
	var blocked ErrBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want an ErrBlocked naming the address", err)
	}
	if !strings.Contains(blocked.Error(), "not a public address") {
		t.Errorf("the refusal does not say why: %v", blocked)
	}
}

// What the audit line is built from. The URL says what was asked for; only the
// dialer knows what was actually reached.
func TestDialerRecordsWhatItConnectedTo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	d := &Dialer{Allow: func(net.IP) bool { return true }}
	conn, err := d.DialContext(context.Background(), "tcp",
		strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if len(d.Seen) != 1 {
		t.Fatalf("recorded %v, want one address", d.Seen)
	}
	if !strings.HasPrefix(d.Seen[0], "127.") && d.Seen[0] != "::1" {
		t.Errorf("recorded %q, which is not the address it dialled", d.Seen[0])
	}
}

// A name resolving to several addresses must be refused when the one it would
// have used is internal, rather than quietly trying the next.
func TestAHostWithOnlyBlockedAddressesIsRefused(t *testing.T) {
	d := &Dialer{}
	_, err := d.DialContext(context.Background(), "tcp", "localhost:9")
	if err == nil {
		t.Fatal("localhost was dialled")
	}
	var blocked ErrBlocked
	if !errors.As(err, &blocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
}
