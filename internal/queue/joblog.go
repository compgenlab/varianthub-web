package queue

import (
	"context"
	"strings"
	"sync"
	"time"
)

// logFlushEvery is how often a running job's output reaches the database.
//
// The point of flushing at all is that the runs worth reading are the ones that
// never finish: a worker killed by an OOM or a restart writes nothing on the way
// out, so whatever is already stored is all there will ever be. A few seconds
// bounds how much of the story is lost, and costs one small write per job per
// interval.
const logFlushEvery = 3 * time.Second

// logMaxBytes caps what is retained per job.
//
// Kept from the end: a run that fails does so at the end, and the first lines of
// a provisioning job that downloaded 175 files are the least interesting bytes
// in the file. A truncation marker says what happened, so a short log is not
// mistaken for a quiet run.
const logMaxBytes = 512 * 1024

// LogWriter accumulates a job's output and flushes it periodically.
//
// Safe for concurrent use: the runner writes from its stderr goroutine while the
// flusher reads.
type LogWriter struct {
	q     *Queue
	jobID string

	mu    sync.Mutex
	buf   strings.Builder
	dirty bool

	stop chan struct{}
	done chan struct{}
}

// NewLogWriter starts flushing a job's output until Close is called.
func NewLogWriter(ctx context.Context, q *Queue, jobID string) *LogWriter {
	w := &LogWriter{q: q, jobID: jobID, stop: make(chan struct{}), done: make(chan struct{})}
	go w.loop(ctx)
	return w
}

// Line appends one line of output.
func (w *LogWriter) Line(s string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.WriteString(s)
	w.buf.WriteByte('\n')
	w.dirty = true
	if w.buf.Len() > logMaxBytes*2 {
		w.truncateLocked()
	}
}

// Note records something the job did not print itself — an attempt starting, a
// worker taking it over. Marked so it is not mistaken for the tool's own output.
func (w *LogWriter) Note(s string) { w.Line("··· " + s) }

func (w *LogWriter) truncateLocked() {
	s := w.buf.String()
	if len(s) <= logMaxBytes {
		return
	}
	cut := s[len(s)-logMaxBytes:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:] // start at a line boundary rather than mid-line
	}
	w.buf.Reset()
	w.buf.WriteString("··· earlier output dropped; this log keeps the last " +
		"512 KB\n" + cut)
}

func (w *LogWriter) loop(ctx context.Context) {
	defer close(w.done)
	t := time.NewTicker(logFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.flush(ctx)
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (w *LogWriter) flush(ctx context.Context) {
	w.mu.Lock()
	if !w.dirty {
		w.mu.Unlock()
		return
	}
	w.truncateLocked()
	out := w.buf.String()
	w.dirty = false
	w.mu.Unlock()

	// WithoutCancel: a job being cancelled or timing out is exactly when its
	// output matters, and its context is already dead by then.
	_ = w.q.SetLog(context.WithoutCancel(ctx), w.jobID, out)
}

// Close stops flushing and writes whatever is left.
func (w *LogWriter) Close(ctx context.Context) {
	if w == nil {
		return
	}
	close(w.stop)
	<-w.done
	w.flush(ctx)
}
