package limit

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPTrustedProxy(t *testing.T) {
	trusted := ParseCIDRs([]string{"10.0.0.0/8"})
	// Peer is the trusted proxy → trust the rightmost X-Forwarded-For entry.
	r := httptest.NewRequest("POST", "/api/v1/annotate", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.7")
	if got := ClientIP(r, trusted); got != "203.0.113.7" {
		t.Errorf("trusted proxy: ClientIP = %q, want 203.0.113.7", got)
	}

	// Peer is NOT trusted → ignore the (possibly spoofed) header, use the peer.
	r2 := httptest.NewRequest("POST", "/api/v1/annotate", nil)
	r2.RemoteAddr = "8.8.8.8:1234"
	r2.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := ClientIP(r2, trusted); got != "8.8.8.8" {
		t.Errorf("untrusted peer: ClientIP = %q, want 8.8.8.8", got)
	}
}

func TestIPLimiterBurstThenThrottle(t *testing.T) {
	l := New(60, 3) // 1/sec, burst 3
	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Errorf("4th request should be throttled")
	}
	// A different IP has its own bucket.
	if !l.Allow("5.6.7.8") {
		t.Errorf("distinct IP should be allowed")
	}
	// Disabled limiter allows everything.
	off := New(0, 0)
	for i := 0; i < 100; i++ {
		if !off.Allow("x") {
			t.Fatalf("disabled limiter should allow all")
		}
	}
}
