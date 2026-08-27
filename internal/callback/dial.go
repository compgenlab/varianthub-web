// Package callback delivers a job's terminal status to a URL the caller gave.
//
// The whole feature is "this service makes an HTTP request to an address
// somebody supplied", which is the textbook server-side request forgery shape.
// Left open it turns the API pod into a proxy for whoever can submit a job:
// they name http://169.254.169.254/ and get the cloud instance's credentials,
// or http://localhost:5432/ and reach a database that trusts the network, or
// anything inside the private ranges that is only unreachable from outside.
//
// So the interesting code here is not the POST. It is deciding what may be
// connected to, and doing that at the only moment the answer is knowable.
package callback

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// blockedPrefixes are the ranges Go's own classifiers do not cover.
//
// net.IP already answers for loopback (all of 127.0.0.0/8, and ::1), the
// unspecified address, link-local (169.254.0.0/16 — which is both the cloud
// metadata address and the ECS one — and fe80::/10), the RFC1918 private
// ranges, IPv6 unique-local, and multicast. It also unwraps IPv4-mapped IPv6,
// so ::ffff:127.0.0.1 reads as loopback rather than as an ordinary v6 address.
//
// These are the rest, and they are not academic: carrier-grade NAT is what a
// cloud VPC or a Kubernetes pod network is often numbered out of, so 100.64/10
// is an interior range in exactly the deployments this runs in.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64, a v4 address wearing a v6 hat
	netip.MustParsePrefix("::/128"),          // unspecified, v6 spelling
}

// Reachable reports whether an address may be connected to.
//
// Public unicast only. The default is refusal: anything this cannot positively
// classify as an ordinary routable address is turned away, because the cost of
// being wrong in one direction is a callback that does not fire and in the
// other is an internal service reached from outside.
func Reachable(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	// Unmapped, so an IPv4-mapped IPv6 address is compared against the v4
	// prefixes above rather than failing to match any of them and being allowed.
	addr = addr.Unmap()
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// ErrBlocked is returned when a callback URL resolves somewhere it may not go.
type ErrBlocked struct {
	Host string
	IP   string
}

func (e ErrBlocked) Error() string {
	return fmt.Sprintf("callback host %s resolves to %s, which is not a public address",
		e.Host, e.IP)
}

// Dialer is a net.Dialer that refuses to connect to non-public addresses, and
// records the ones it does connect to.
//
// Checking here rather than by resolving the URL first is the whole design, and
// it buys three things that a pre-flight check cannot:
//
// It closes DNS rebinding. A pre-flight resolve asks the name for an address,
// approves it, and then makes a second, independent resolution when it actually
// connects — and nothing says the two agree. An attacker who controls the name
// answers with a public address for the first and 127.0.0.1 for the second. The
// check has to be on the address being connected to, and this is the only place
// that holds it.
//
// It covers redirects for free. Every hop dials, so every hop is checked, and
// the redirect policy does not have to re-implement any of this.
//
// And it is where the audit line comes from: the URL is what somebody asked
// for, and this is the only thing that knows what was actually reached.
type Dialer struct {
	// Seen records each address connected to, in order, for the log.
	Seen []string

	// Allow overrides the reachability rule. Only tests set it — a loopback
	// server is the only way to exercise delivery, and it is exactly what the
	// rule exists to forbid.
	Allow func(net.IP) bool
}

// DialContext connects, refusing any address that is not publicly routable.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Resolved here so the addresses checked are the ones dialled. A literal
	// address resolves to itself, so there is no separate path for it.
	ips, err := net.DefaultResolver.LookupIP(ctx, ipNetwork(network), host)
	if err != nil {
		return nil, err
	}

	allow := d.Allow
	if allow == nil {
		allow = Reachable
	}

	var firstBlocked net.IP
	for _, ip := range ips {
		if !allow(ip) {
			if firstBlocked == nil {
				firstBlocked = ip
			}
			continue
		}
		// One IP at a time, to the address just approved. Handing the host name
		// back to a dialer would resolve it again — the rebinding window this
		// exists to close.
		conn, err := (&net.Dialer{
			Timeout: 10 * time.Second,
			Control: refuseNonTCP,
		}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			continue
		}
		d.Seen = append(d.Seen, ip.String())
		return conn, nil
	}
	if firstBlocked != nil {
		return nil, ErrBlocked{Host: host, IP: firstBlocked.String()}
	}
	return nil, fmt.Errorf("callback host %s has no address that can be reached", host)
}

// refuseNonTCP rejects anything that is not a TCP socket.
//
// Belt and braces against a transport that could be pointed at a unix socket or
// a raw protocol: this is an HTTP client and TCP is the only thing it needs.
func refuseNonTCP(network, _ string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return nil
	}
	return fmt.Errorf("callbacks are made over TCP, not %s", network)
}

// ipNetwork maps a dial network to the address family to resolve for.
func ipNetwork(network string) string {
	switch network {
	case "tcp4":
		return "ip4"
	case "tcp6":
		return "ip6"
	}
	return "ip"
}
