package limit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ParseCIDRs parses a list of CIDR strings into networks, skipping any that don't
// parse (they are simply not trusted).
func ParseCIDRs(cidrs []string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ClientIP resolves the request's client IP for fairness/throttling. When the
// immediate peer (RemoteAddr) is a trusted proxy, it trusts the rightmost
// X-Forwarded-For entry — the address that proxy actually observed — which a
// client cannot spoof past a correctly-configured Caddy/Traefik. Otherwise it uses
// the peer address directly. Returns "" only if nothing parses.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && ipInAny(peer, trusted) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// Rightmost entry is the address the trusted proxy saw.
			cand := strings.TrimSpace(parts[len(parts)-1])
			if net.ParseIP(cand) != nil {
				return cand
			}
		}
	}
	return host
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Limiter is a per-IP token-bucket rate limiter. Tokens refill at ratePerSec up
// to burst; each allowed request consumes one. A ratePerSec <= 0 disables limiting
// (allow always). Stale buckets are evicted periodically.
type Limiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	ratePerSec float64
	burst      float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a limiter from a per-minute rate and burst. ratePerMin <= 0
// yields a no-op limiter (allow always).
func New(ratePerMin, burst int) *Limiter {
	l := &Limiter{buckets: map[string]*bucket{}}
	if ratePerMin > 0 {
		l.ratePerSec = float64(ratePerMin) / 60
		l.burst = float64(burst)
		if l.burst < 1 {
			l.burst = 1
		}
	}
	return l
}

// allow reports whether a request from ip may proceed now, consuming a token.
func (l *Limiter) Allow(ip string) bool {
	return l.AllowAt(ip, l.ratePerMin(), int(l.burst))
}

// AllowAt is Allow against a rate given per call rather than at construction,
// for a limit that differs by who is asking.
//
// One limiter rather than one per rate, because the bucket belongs to the
// caller and the rate is a property of their tier: someone moved from one tier
// to another should carry their bucket with them, not be handed a fresh full
// one by landing in a different map. Changing the rate re-scales the same
// bucket, which is what makes a tier change take effect immediately without
// also granting a burst.
//
// ratePerMin <= 0 means unlimited, matching New and the 0-is-unbounded
// convention the cache budget already uses.
func (l *Limiter) AllowAt(key string, ratePerMin, burst int) bool {
	if ratePerMin <= 0 {
		return true
	}
	ratePerSec := float64(ratePerMin) / 60
	cap := float64(burst)
	if cap < 1 {
		cap = 1
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: cap, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * ratePerSec
	if b.tokens > cap {
		b.tokens = cap
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// ratePerMin recovers the configured rate, for Allow's fixed-rate path.
func (l *Limiter) ratePerMin() int {
	if l.ratePerSec <= 0 {
		return 0
	}
	return int(l.ratePerSec * 60)
}

// gc evicts buckets untouched for longer than maxIdle, bounding memory under a
// churn of distinct IPs.
func (l *Limiter) GC(maxIdle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-maxIdle)
	for ip, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
}
