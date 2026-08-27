package callback

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testSender delivers to loopback, which the address rule exists to forbid — so
// the rule is overridden here and nowhere else. Everything else is production.
func testSender() *Sender {
	s := NewSender()
	s.RequireHTTPS = false
	s.Allow = func(net.IP) bool { return true }
	s.Backoff = func(int) time.Duration { return time.Millisecond }
	return s
}

func note() Notification {
	return Notification{JobID: "j-1", Status: "done"}
}

func TestADeliveryIsAPostCarryingTheJobAndItsStatus(t *testing.T) {
	var gotEvent, gotDelivery, gotMethod, gotCT string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotEvent = r.Header.Get(HeaderEvent)
		gotDelivery = r.Header.Get(HeaderDelivery)
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	res := testSender().Send(context.Background(), srv.URL, note())
	if res.Err != nil || res.Status != 200 {
		t.Fatalf("send: status=%d err=%v", res.Status, res.Err)
	}
	if gotMethod != "POST" || gotCT != "application/json" {
		t.Errorf("method=%s content-type=%s", gotMethod, gotCT)
	}
	if gotEvent != "job.done" {
		t.Errorf("event = %q, want job.done", gotEvent)
	}
	// The idempotency key, which at-least-once delivery makes load-bearing.
	if gotDelivery != "j-1" {
		t.Errorf("delivery header = %q, want the job id", gotDelivery)
	}

	// Two fields and no more. Anything else here is something a receiver might
	// record without asking again, and nothing in this body is authenticated.
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if got["job_id"] != "j-1" || got["status"] != "done" {
		t.Errorf("body = %v", got)
	}
	if len(got) != 2 {
		t.Errorf("body carries %v; only the job id and status belong there", got)
	}
}

// Nothing is signed, so nothing may be sent that a receiver would be tempted to
// trust. This is the guard on that decision rather than on the wire format.
func TestTheBodyCarriesNothingWorthForging(t *testing.T) {
	b, err := json.Marshal(Notification{JobID: "j-1", Status: "error"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// Field names, not a substring search: "error" is a legitimate *status*,
	// and matching on the rendered text would flag the value it is meant to
	// carry.
	for k := range got {
		if k != "job_id" && k != "status" {
			t.Errorf("the notification carries the field %q; nothing here is "+
				"authenticated, so nothing may invite a receiver to trust it", k)
		}
	}
}

// The case verified against the standard library: a POST through 301/302/303
// arrives as a bodiless GET. For a webhook that is silent data loss — no body
// to verify, and the sender counts it delivered.
func TestARedirectedDeliveryIsStillAPostWithItsBody(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		var gotMethod string
		var gotBody []byte
		final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotBody, _ = io.ReadAll(r.Body)
		}))
		redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, final.URL, code)
		}))

		res := testSender().Send(context.Background(), redir.URL, note())
		if res.Err != nil || res.Status != 200 {
			t.Errorf("%d: status=%d err=%v", code, res.Status, res.Err)
		}
		if gotMethod != "POST" {
			t.Errorf("%d: receiver saw %s, want POST", code, gotMethod)
		}
		var n Notification
		if err := json.Unmarshal(gotBody, &n); err != nil || n.JobID != "j-1" {
			t.Errorf("%d: the body did not survive the redirect: %q", code, gotBody)
		}
		final.Close()
		redir.Close()
	}
}

// Every hop dials, so every hop is checked — the redirect policy does not have
// to re-implement the address rule. A redirect to a blocked address is refused
// even though the first hop was fine, which is the SSRF bypass this closes.
func TestARedirectToABlockedAddressIsRefused(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "instance credentials")
	}))
	defer internal.Close()

	// Public for the first hop, real rule for the rest: the redirect target is
	// loopback, which Reachable refuses.
	s := NewSender()
	s.RequireHTTPS = false
	s.Backoff = func(int) time.Duration { return time.Millisecond }
	first := true
	s.Allow = func(ip net.IP) bool {
		if first {
			first = false
			return true
		}
		return Reachable(ip)
	}

	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redir.Close()

	res := s.Send(context.Background(), redir.URL, note())
	if res.Err == nil && res.Status >= 200 && res.Status < 300 {
		t.Fatal("a redirect reached a blocked address")
	}
}

// A 5xx is worth another go; the receiver may be restarting.
func TestATemporaryFailureIsRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := testSender().Send(context.Background(), srv.URL, note())
	if res.Err != nil || res.Status != 200 {
		t.Fatalf("status=%d err=%v after %d attempts", res.Status, res.Err, res.Attempts)
	}
	if res.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", res.Attempts)
	}
}

// A 4xx is the receiver saying the request is wrong. Repeating it identically
// cannot fix that, and retrying would just multiply the noise in their logs.
func TestAPermanentRejectionIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	res := testSender().Send(context.Background(), srv.URL, note())
	if res.Status != 400 {
		t.Fatalf("status = %d", res.Status)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("the receiver was called %d times for a 400", got)
	}
}

// A blocked address is final: waiting does not make it routable, and retrying
// only repeats the lookup and the log line.
func TestABlockedAddressIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	s := NewSender()
	s.RequireHTTPS = false
	s.Backoff = func(int) time.Duration { return time.Millisecond }

	res := s.Send(context.Background(), srv.URL, note())
	if res.Err == nil {
		t.Fatal("a loopback callback was delivered")
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — a blocked address is not worth retrying", res.Attempts)
	}
}

// The audit line names both what was asked for and what was reached, because
// neither alone shows a callback that resolved somewhere unexpected.
func TestTheAuditLineNamesTheURLAndTheAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	res := testSender().Send(context.Background(), srv.URL, note())
	line := res.Log()
	if !strings.Contains(line, "callback to: "+srv.URL) {
		t.Errorf("the log does not name the URL: %s", line)
	}
	if !strings.Contains(line, "callback server/IP: 127.") && !strings.Contains(line, "::1") {
		t.Errorf("the log does not name the address reached: %s", line)
	}
}

// Submitted URLs that should never be accepted at all.
func TestValidateURLRefusesWhatItShould(t *testing.T) {
	s := NewSender()
	for _, bad := range []string{
		"http://example.org/hook",         // plain http, with https required
		"ftp://example.org/hook",          // not http at all
		"file:///etc/passwd",              // nor a file
		"https://user:pass@example.org/x", // credentials that would be sent onward
		"https://",                        // no host
		"not a url at all",                // no scheme
	} {
		if err := s.ValidateURL(bad); err == nil {
			t.Errorf("ValidateURL(%q) accepted it", bad)
		}
	}
	if err := s.ValidateURL("https://example.org/hook"); err != nil {
		t.Errorf("an ordinary https URL was refused: %v", err)
	}
	s.RequireHTTPS = false
	if err := s.ValidateURL("http://example.org/hook"); err != nil {
		t.Errorf("http was refused with RequireHTTPS off: %v", err)
	}
}
