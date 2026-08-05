package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// remoteTTL is how long a measured remote size is reused.
//
// A streamed source's file does not change without its version changing, so the
// figure is stable; the TTL exists to pick up a re-published file eventually,
// not to track a moving number. It is long because the alternative is issuing
// HEAD requests to somebody else's server on every dashboard load.
const remoteTTL = 6 * time.Hour

// remoteProbeTimeout bounds one HEAD. An origin that is slow or down must not
// hold the whole page.
const remoteProbeTimeout = 10 * time.Second

// remoteProbeConcurrency caps parallel HEADs, mostly to stay polite to origins
// that serve several of our sources.
const remoteProbeConcurrency = 8

// RemoteUsage is what one streamed source costs to read, and where from.
type RemoteUsage struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Files    int    `json:"files"`
	// Bytes covers only the files whose size the origin reported.
	Bytes int64 `json:"bytes"`
	// Unmeasured counts files whose origin gave no Content-Length, so the
	// total is a floor rather than a wrong number stated confidently.
	Unmeasured int `json:"unmeasured,omitempty"`
}

// remoteSizer measures streamed sources, caching per URL.
type remoteSizer struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]remoteEntry
}

type remoteEntry struct {
	size int64
	ok   bool
	at   time.Time
}

func newRemoteSizer() *remoteSizer {
	return &remoteSizer{
		client: &http.Client{Timeout: remoteProbeTimeout},
		cache:  map[string]remoteEntry{},
	}
}

// sizes measures a set of URLs, returning total bytes and how many could not be
// measured.
func (rs *remoteSizer) sizes(ctx context.Context, urls []string) (int64, int) {
	type result struct {
		size int64
		ok   bool
	}
	results := make([]result, len(urls))

	sem := make(chan struct{}, remoteProbeConcurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			size, ok := rs.size(ctx, u)
			results[i] = result{size, ok}
		}()
	}
	wg.Wait()

	var total int64
	var unmeasured int
	for _, r := range results {
		if r.ok {
			total += r.size
			continue
		}
		unmeasured++
	}
	return total, unmeasured
}

func (rs *remoteSizer) size(ctx context.Context, url string) (int64, bool) {
	rs.mu.Lock()
	e, hit := rs.cache[url]
	rs.mu.Unlock()
	if hit && time.Since(e.at) < remoteTTL {
		return e.size, e.ok
	}

	size, ok := rs.probe(ctx, url)
	rs.mu.Lock()
	rs.cache[url] = remoteEntry{size: size, ok: ok, at: time.Now()}
	rs.mu.Unlock()
	return size, ok
}

// probe HEADs a URL for its length.
//
// A HEAD that fails falls back to a ranged GET of one byte: some origins refuse
// HEAD but answer a range with a Content-Range naming the full length — which is
// the same request the annotator makes anyway, so an origin that fails both is
// one we could not stream from either.
func (rs *remoteSizer) probe(ctx context.Context, url string) (int64, bool) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false
	}
	resp, err := rs.client.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
			return resp.ContentLength, true
		}
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err = rs.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, false
	}
	// "bytes 0-0/12345" — the total is after the slash.
	cr := resp.Header.Get("Content-Range")
	i := strings.LastIndexByte(cr, '/')
	if i < 0 {
		return 0, false
	}
	var total int64
	for _, c := range cr[i+1:] {
		if c < '0' || c > '9' {
			return 0, false // "*" — the origin knows the range but not the length
		}
		total = total*10 + int64(c-'0')
	}
	return total, total > 0
}

// metricsResponse is the admin dashboard's payload.
type metricsResponse struct {
	Jobs    queue.Stats            `json:"jobs"`
	Sources catalog.SourceCounts   `json:"sources"`
	Storage []catalog.StorageUsage `json:"storage"`
	// StorageBytes totals the locations we hold data in. Remote bytes are
	// deliberately not folded in: they are somebody else's disk, and adding
	// them would misstate what this deployment stores.
	StorageBytes int64         `json:"storage_bytes"`
	Remote       []RemoteUsage `json:"remote"`
	RemoteBytes  int64         `json:"remote_bytes"`
	// RemoteMeasured is false when at least one streamed file could not be
	// sized, making RemoteBytes a floor.
	RemoteMeasured bool  `json:"remote_measured"`
	GeneratedAt    int64 `json:"generated_at"`
}

// handleMetrics reports queue throughput and storage usage.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := metricsResponse{GeneratedAt: time.Now().Unix(), RemoteMeasured: true}

	jobs, err := s.queue.Stats(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.Jobs = jobs

	if s.catalog == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	if out.Sources, err = s.catalog.CountSources(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out.Storage, err = s.catalog.StorageUsage(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, u := range out.Storage {
		out.StorageBytes += u.Bytes
	}

	// Sizing a streamed source means reaching its origin, so it is opt-out:
	// ?remote=0 gives the local figures without waiting on anybody else.
	if r.URL.Query().Get("remote") != "0" {
		remote, complete, err := s.remoteUsage(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out.Remote = remote
		out.RemoteMeasured = complete
		for _, u := range remote {
			out.RemoteBytes += u.Bytes
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// remoteUsage measures every streamed source.
func (s *Server) remoteUsage(ctx context.Context) ([]RemoteUsage, bool, error) {
	sources, err := s.catalog.ListSources(ctx)
	if err != nil {
		return nil, false, err
	}
	out := []RemoteUsage{}
	complete := true
	for _, src := range sources {
		if !src.Stream {
			continue
		}
		urls := catalog.SourceURLs(src.TOML)
		if len(urls) == 0 {
			continue
		}
		bytes, unmeasured := s.remote.sizes(ctx, urls)
		if unmeasured > 0 {
			complete = false
		}
		out = append(out, RemoteUsage{
			SourceID:   src.ID,
			Name:       src.DisplayName(),
			Host:       hostOf(urls[0]),
			Files:      len(urls),
			Bytes:      bytes,
			Unmeasured: unmeasured,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, complete, nil
}

// hostOf is the origin a source streams from, for display.
func hostOf(url string) string {
	rest := url
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
