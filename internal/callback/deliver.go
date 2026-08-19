package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Making the request, once it is known where it may go.

// Notification is the body posted when a job reaches a terminal status.
//
// Two fields. The job id says which submission this is about, the status says
// how it ended, and anything else a receiver wants it asks for with its own
// credential from the endpoint that already answers authoritatively.
//
// It is deliberately unsigned, and the payload is small *because* of that. The
// job id is a 128-bit value known only to the submitter and this service, so it
// already carries as much proof of origin as a shared key would for a message
// that says no more than "job X ended this way" — Marcus's point, and it holds.
// What a signature would have protected is contents worth lying about: variant
// counts, error text, anything a receiver might record without asking again. The
// answer to that is to stop sending them rather than to sign them.
//
// So the status is a convenience, not an authority. A receiver that must be
// certain calls GET /jobs/{id}, which is the one answer nobody who has merely
// seen a job id in a log can forge.
type Notification struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// Headers a receiver reads.
const (
	// HeaderEvent is job.done | job.error | job.cancelled, so a receiver can
	// route without parsing the body.
	HeaderEvent = "X-VariantHub-Event"
	// HeaderDelivery is the job id again, as an idempotency key. At-least-once
	// delivery means a receiver can see the same notification twice — a slow 200
	// this gave up waiting for is indistinguishable from a failure — so it needs
	// something stable to deduplicate on.
	HeaderDelivery = "X-VariantHub-Delivery"
)

// EventFor names the event a terminal status is.
func EventFor(status string) string {
	switch status {
	case "done", "error", "cancelled":
		return "job." + status
	}
	return "job.finished"
}

// Result is what one delivery attempt did, for the log.
type Result struct {
	URL      string   // what was asked for
	Servers  []string // the addresses actually reached, in order
	Status   int
	Attempts int
	Err      error
}

// Log renders the audit line.
//
// The URL and the address are both here because neither is the whole story: a
// name says what somebody configured, and an address says where the bytes went.
// A callback that was redirected, or whose DNS answers differently than
// expected, is only visible in the second.
func (r Result) Log() string {
	var b strings.Builder
	fmt.Fprintf(&b, "callback to: %s", r.URL)
	if len(r.Servers) > 0 {
		fmt.Fprintf(&b, ", callback server/IP: %s", strings.Join(r.Servers, " -> "))
	} else {
		b.WriteString(", callback server/IP: (never connected)")
	}
	if r.Status != 0 {
		fmt.Fprintf(&b, ", status: %d", r.Status)
	}
	if r.Attempts > 1 {
		fmt.Fprintf(&b, ", attempts: %d", r.Attempts)
	}
	if r.Err != nil {
		fmt.Fprintf(&b, ", error: %v", r.Err)
	}
	return b.String()
}

// Sender delivers notifications.
type Sender struct {
	// RequireHTTPS refuses plain http, which is the default. A notification is
	// signed but not encrypted, so over http its contents — a job id, a label
	// that may name a study — are readable by anything on the path.
	RequireHTTPS bool

	// MaxAttempts is how many times one notification is tried.
	MaxAttempts int

	// Backoff is the wait after attempt n (1-based).
	Backoff func(attempt int) time.Duration

	// Allow overrides the address rule. Tests only: a receiver they can start
	// is on loopback, which is exactly what the rule forbids.
	Allow func(net.IP) bool

	// Timeout bounds one attempt.
	Timeout time.Duration
}

// NewSender returns a Sender with the defaults a deployment wants.
func NewSender() *Sender {
	return &Sender{
		RequireHTTPS: true,
		MaxAttempts:  4,
		Timeout:      15 * time.Second,
		// 2s, 8s, 32s. Short enough that a receiver restarting is covered,
		// bounded so a permanently broken URL stops costing anything.
		Backoff: func(attempt int) time.Duration {
			return time.Duration(2<<uint(2*(attempt-1))) * time.Second
		},
	}
}

// ValidateURL checks a callback URL at the moment it is submitted.
//
// A first pass only, and it cannot be the whole check: it says the URL is
// well-formed and not obviously local, which catches a typo while somebody is
// watching. What it cannot do is decide where the name will point when the job
// finishes hours later, which is why the real check is in the dialer.
func (s *Sender) ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("callback_url is not a URL: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if s.RequireHTTPS {
			return errors.New("callback_url must be https")
		}
	default:
		return fmt.Errorf("callback_url must be http(s), not %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("callback_url has no host")
	}
	if u.User != nil {
		// Credentials in the URL would be sent to whatever it resolves to, and
		// would sit in the job row and in every log line that names it.
		return errors.New("callback_url must not carry credentials")
	}
	return nil
}

// Send delivers a notification, retrying a few times, and reports what happened.
//
// Errors are returned rather than raised: a callback that cannot be delivered
// is not a job that failed. The job ran, its results are where they always are,
// and the only thing lost is a message somebody asked to be sent. So this
// reports and the caller logs.
func (s *Sender) Send(ctx context.Context, rawURL string, n Notification) Result {
	res := Result{URL: rawURL}
	if err := s.ValidateURL(rawURL); err != nil {
		res.Err = err
		return res
	}

	attempts := s.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	for i := 1; i <= attempts; i++ {
		res.Attempts = i
		status, servers, err := s.attempt(ctx, rawURL, n)
		res.Servers, res.Status, res.Err = servers, status, err

		if err == nil && status >= 200 && status < 300 {
			return res
		}
		// A refusal to connect is final: the address is not one this may reach,
		// and waiting will not change that. Retrying would only repeat the
		// lookup and the log line.
		var blocked ErrBlocked
		if errors.As(err, &blocked) {
			return res
		}
		// 4xx other than 429 is the receiver saying the request is wrong, which
		// a second identical request does not fix.
		if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
			return res
		}
		if i == attempts {
			return res
		}
		select {
		case <-ctx.Done():
			res.Err = ctx.Err()
			return res
		case <-time.After(s.Backoff(i)):
		}
	}
	return res
}

// attempt makes one delivery.
func (s *Sender) attempt(ctx context.Context, rawURL string,
	n Notification) (int, []string, error) {

	body, err := json.Marshal(n)
	if err != nil {
		return 0, nil, err
	}

	d := &Dialer{Allow: s.Allow}
	client := &http.Client{
		Timeout:       s.Timeout,
		Transport:     &http.Transport{DialContext: d.DialContext},
		CheckRedirect: s.redirect(body),
	}

	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, d.Seen, err
	}
	setHeaders(req, body, n)

	resp, err := client.Do(req)
	if err != nil {
		return 0, d.Seen, err
	}
	defer resp.Body.Close()
	// Read and discarded, so the connection can be reused and so a receiver
	// that answers with a page of HTML does not hold it open.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, d.Seen, nil
}

func setHeaders(req *http.Request, body []byte, n Notification) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "VariantHub-Callback/1")
	req.Header.Set(HeaderEvent, EventFor(n.Status))
	req.Header.Set(HeaderDelivery, n.JobID)
	req.ContentLength = int64(len(body))
}

// redirect follows a redirect while keeping the request a POST.
//
// Go turns a POST into a bodiless GET across 301, 302 and 303 — which is what
// the specification says for 303 and what every implementation does for the
// other two. For a webhook that is silent data loss: the receiver gets an empty
// GET, there is no body to verify the signature over, and the sender counts it
// as delivered. Verified against the standard library rather than assumed.
//
// So the method and body are put back. That departs from the letter of 303, and
// deliberately: a redirect on a notification endpoint means "it moved", and
// delivering an empty GET to the new location satisfies nobody.
//
// The address of each hop is still checked, because each hop dials, and the
// dialer is what decides. Nothing about redirects needs to repeat that.
func (s *Sender) redirect(body []byte) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if s.RequireHTTPS && req.URL.Scheme != "https" {
			return fmt.Errorf("callback redirected to %s, which is not https", req.URL.Scheme)
		}
		req.Method = "POST"
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		// Re-set because Go drops some headers across hosts, and a receiver at
		// the new location needs them as much as the first one did.
		req.Header.Set("Content-Type", "application/json")
		return nil
	}
}
