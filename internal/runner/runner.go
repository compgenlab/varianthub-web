// Package runner is the annotation seam.
//
// This service does not annotate. The engine — snapshots, sources, tabix queries,
// external tools like VEP — lives in varianthub-cli, whose transitive closure is
// 13 packages of filesystem- and container-bound code. Rather than publish that as
// a library to serve one consumer, a worker shells out to `varhub`.
//
// The boundary is well-defined: `varhub annotate --format json` emits the same
// []Variant JSON the old embedded REST server returned, because the CLI and the
// server deliberately shared that type. So the process edge costs nothing in
// fidelity — the bytes a worker stores are the bytes the CLI produced.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kinds of job input.
const (
	KindLocus = "locus"
	KindVCF   = "vcf"
)

// Request is one annotation job's input.
type Request struct {
	Kind      string // KindLocus | KindVCF
	Snapshot  string // snapshot name; "" uses the config default
	Selection string // "" (snapshot defaults) | "all" | "a,b,c"
	Body      []byte // the locus string, or the VCF file's bytes
}

// Result is a completed annotation.
type Result struct {
	Variants []byte   // the raw JSON array, stored and served verbatim
	N        int      // number of variants
	Columns  []Column // the column model for these results, in snapshot order
}

// Column describes one annotation column of a result set: what to label it, how
// to render it, and which source produced it. The design's results table tags
// each column with its source, so a reader can tell which dataset a value came
// from.
type Column struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Type      string `json:"type,omitempty"`   // categorical|text|numeric|flag
	Source    string `json:"source,omitempty"` // producing source name
	SourceRef string `json:"source_ref,omitempty"`
	Default   bool   `json:"default"`
}

// annotationListing mirrors `varhub annotation list --format json`.
type annotationListing struct {
	Annotations []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Description string `json:"description"`
		Default     bool   `json:"default"`
		Source      string `json:"source"`
		SourceRef   string `json:"source_ref"`
		SourceTitle string `json:"source_title"`
	} `json:"annotations"`
}

// Runner annotates one request.
type Runner interface {
	Annotate(ctx context.Context, req Request) (Result, error)
}

// HomeProvider supplies a VARHUB_HOME for a job: a directory holding config.toml
// and an annotations/ tree.
//
// Chunk 1 ships FixedHome, which points at a directory already on disk. Chunk 2
// replaces it with a provider that materializes the tree from the Postgres
// catalog per job, which is what lets a server hold no annotation config locally.
type HomeProvider interface {
	Home(ctx context.Context, snapshot string) (dir string, cleanup func(), err error)
}

// FixedHome is a HomeProvider backed by an existing directory. Cleanup is a no-op
// — it must never delete a home it did not create.
type FixedHome string

func (h FixedHome) Home(context.Context, string) (string, func(), error) {
	if h == "" {
		return "", nil, errors.New("no VARHUB_HOME configured")
	}
	if _, err := os.Stat(filepath.Join(string(h), "config.toml")); err != nil {
		return "", nil, fmt.Errorf("VARHUB_HOME %s has no config.toml: %w", string(h), err)
	}
	return string(h), func() {}, nil
}

// ExecRunner annotates by executing the varhub CLI.
type ExecRunner struct {
	Bin     string        // path to the varhub binary (default "varhub")
	Home    HomeProvider  // supplies VARHUB_HOME per job
	Timeout time.Duration // per-job wall clock (0 = no limit beyond ctx)

	// OnProgress, if set, receives the CLI's progress lines as they arrive.
	// varhub -v logs to stderr with a "varhub: " prefix; this is what will drive
	// the job stage/percent the design's Running screen wants.
	OnProgress func(line string)
}

var _ Runner = (*ExecRunner)(nil)

// Annotate runs one job through the CLI.
func (r *ExecRunner) Annotate(ctx context.Context, req Request) (Result, error) {
	bin := r.Bin
	if bin == "" {
		bin = "varhub"
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	home, cleanup, err := r.Home.Home(ctx, req.Snapshot)
	if err != nil {
		return Result{}, fmt.Errorf("prepare annotation home: %w", err)
	}
	defer cleanup()

	// Work dir for a VCF upload, kept out of the home so cleanup is unambiguous.
	work, err := os.MkdirTemp("", "varhub-job-")
	if err != nil {
		return Result{}, fmt.Errorf("scratch dir: %w", err)
	}
	defer os.RemoveAll(work)

	if err := safeArg(req.Snapshot); err != nil {
		return Result{}, fmt.Errorf("snapshot: %w", err)
	}
	if err := safeArg(req.Selection); err != nil {
		return Result{}, fmt.Errorf("annotation selection: %w", err)
	}

	args := []string{"-home", home}
	if req.Snapshot != "" {
		args = append(args, "-snapshot", req.Snapshot)
	}
	args = append(args, "annotate", "--format", "json", "-v")
	switch strings.TrimSpace(req.Selection) {
	case "":
		// snapshot defaults
	case "all":
		args = append(args, "--all")
	default:
		args = append(args, "-a", req.Selection)
	}

	// "--" terminates flag parsing. Job input is user-controlled and lands in
	// argv, so without it a locus beginning with "-" is read as a flag: the CLI
	// exits with "flag provided but not defined" and the job fails for a reason
	// that has nothing to do with the variant.
	args = append(args, "--")

	switch req.Kind {
	case KindVCF:
		in := filepath.Join(work, "input.vcf")
		if err := os.WriteFile(in, req.Body, 0o600); err != nil {
			return Result{}, fmt.Errorf("stage VCF: %w", err)
		}
		args = append(args, in)
	case KindLocus:
		loci := strings.Fields(string(req.Body))
		if len(loci) == 0 {
			return Result{}, errors.New("empty locus input")
		}
		for _, l := range loci {
			if err := safeArg(l); err != nil {
				return Result{}, fmt.Errorf("locus %q: %w", truncate(l, 40), err)
			}
		}
		args = append(args, loci...)
	default:
		return Result{}, fmt.Errorf("unknown job kind %q", req.Kind)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "VARHUB_HOME="+home)
	cmd.Dir = work

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start %s: %w", bin, err)
	}

	// Drain stderr concurrently: it carries progress, and letting it fill the pipe
	// buffer would deadlock a long-running annotation.
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		tailLines []string
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if r.OnProgress != nil {
				r.OnProgress(line)
			}
			mu.Lock()
			tailLines = append(tailLines, line)
			if len(tailLines) > 20 {
				tailLines = tailLines[len(tailLines)-20:]
			}
			mu.Unlock()
		}
		_, _ = io.Copy(io.Discard, stderrPipe)
	}()

	runErr := cmd.Wait()
	wg.Wait()

	mu.Lock()
	tail := strings.Join(tailLines, "\n")
	mu.Unlock()

	if runErr != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("annotation cancelled: %w", ctx.Err())
		}
		return Result{}, &ExitError{Err: runErr, Stderr: tail, Home: home}
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return Result{}, &ExitError{Err: errors.New("no output"), Stderr: tail, Home: home}
	}
	// Decode just enough to count rows and learn which annotation keys the engine
	// actually emitted. The blob itself is stored verbatim.
	var probe []struct {
		Annotations map[string]json.RawMessage `json:"annotations"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return Result{}, &ExitError{Err: fmt.Errorf("parse annotation output: %w", err), Stderr: tail, Home: home}
	}

	present := map[string]bool{}
	for _, v := range probe {
		for k := range v.Annotations {
			present[k] = true
		}
	}
	cols, err := r.columns(ctx, bin, home, req.Snapshot, present)
	if err != nil {
		// Columns are presentation metadata; a job that annotated successfully
		// should not fail because labelling them did. Fall back to bare keys.
		log.Printf("runner: column metadata unavailable (%v); falling back to keys", err)
		cols = fallbackColumns(present)
	}
	return Result{Variants: out, N: len(probe), Columns: cols}, nil
}

// columns asks the CLI for the snapshot's annotation catalog and keeps the
// entries the engine actually emitted, in snapshot order.
//
// The CLI is the authority on annotation types and which source produces each
// one — that lives in the source fragments it already parses. Re-deriving it
// here would be a second implementation of that model, free to drift.
func (r *ExecRunner) columns(ctx context.Context, bin, home, snapshot string, present map[string]bool) ([]Column, error) {
	args := []string{"-home", home}
	if snapshot != "" {
		args = append(args, "-snapshot", snapshot)
	}
	args = append(args, "annotation", "list", "--format", "json", "--", snapshot)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "VARHUB_HOME="+home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var listing annotationListing
	if err := json.Unmarshal(stdout.Bytes(), &listing); err != nil {
		return nil, fmt.Errorf("parse annotation listing: %w", err)
	}

	var cols []Column
	seen := map[string]bool{}
	for _, a := range listing.Annotations {
		if !present[a.Name] || seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		label := a.Name
		if a.Description != "" {
			label = a.Description
		}
		source := a.Source
		if a.SourceTitle != "" {
			source = a.SourceTitle
		}
		cols = append(cols, Column{
			Key: a.Name, Label: label, Type: a.Type,
			Source: source, SourceRef: a.SourceRef, Default: a.Default,
		})
	}
	// Anything the engine emitted that the catalog did not describe still needs a
	// column, or the value would be invisible in the table.
	for k := range present {
		if !seen[k] {
			cols = append(cols, Column{Key: k, Label: k})
		}
	}
	return cols, nil
}

// fallbackColumns builds bare columns from the emitted keys, sorted for a stable
// order.
func fallbackColumns(present map[string]bool) []Column {
	keys := make([]string, 0, len(present))
	for k := range present {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cols := make([]Column, len(keys))
	for i, k := range keys {
		cols[i] = Column{Key: k, Label: k}
	}
	return cols
}

// maxArgLen bounds a single argv entry. Real loci and HGVS strings are far
// shorter; anything longer is malformed input, and argv as a whole is capped by
// the kernel.
const maxArgLen = 4096

// safeArg rejects a string that cannot be passed safely as an argv entry.
//
// A NUL byte is the sharp edge: execve rejects the whole call with EINVAL, and
// the resulting "fork/exec: invalid argument" says nothing about which input was
// at fault. Other control characters are rejected too — they have no place in a
// locus and only corrupt logs.
func safeArg(s string) error {
	if len(s) > maxArgLen {
		return fmt.Errorf("too long (%d bytes, max %d)", len(s), maxArgLen)
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return fmt.Errorf("contains a control character (0x%02x)", c)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ExitError is a failed CLI invocation. It separates the operator-facing detail
// (the CLI's full stderr) from the caller-facing message, so a job's stored error
// does not leak internal paths and command lines to API clients — a gap the
// previous server had, where raw Go error strings went straight into the response.
type ExitError struct {
	Err    error
	Stderr string
	// Home is the annotation home this job ran in, redacted out of the
	// caller-facing message. It is a per-job temp path that means nothing to a
	// client and reveals server layout.
	Home string
}

// Error is the caller-facing message, stored on the job and served to clients.
//
// The CLI writes its failures as "error: <message>", and those messages are
// written for humans — "genelist X: needs gtf = ..." tells a user exactly what to
// fix. Hiding them behind a blanket "annotation failed" leaves someone with a
// misconfigured source no way to diagnose it. So the CLI's own message is
// surfaced, with the ephemeral home path redacted; anything unrecognized stays
// opaque.
func (e *ExitError) Error() string {
	if msg := cliMessage(e.Stderr, e.Home); msg != "" {
		return msg
	}
	return "annotation failed"
}

func (e *ExitError) Unwrap() error { return e.Err }

// Detail is the full diagnostic, for logs only.
func (e *ExitError) Detail() string {
	if e.Stderr == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Stderr
}

// cliMessage extracts the last "error: ..." line the CLI wrote, with home
// redacted. Returns "" when stderr carries no such line, so an unexpected
// failure mode cannot leak whatever it happened to print.
func cliMessage(stderr, home string) string {
	var found string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "error: "); ok {
			found = strings.TrimSpace(after)
		}
	}
	if found == "" {
		return ""
	}
	if home != "" {
		found = strings.ReplaceAll(found, home, "<config>")
	}
	// A message still carrying an absolute path is describing server layout, not
	// the user's input. Withhold it rather than guess at redaction.
	if strings.Contains(found, "/tmp/") || strings.Contains(found, "/var/") {
		return ""
	}
	if len(found) > 400 {
		found = found[:400] + "\u2026"
	}
	return found
}
