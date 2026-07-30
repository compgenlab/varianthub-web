// Command varianthub-web is the VariantHub API server, its job worker, and its
// migration runner — one binary, three subcommands, so a deployment ships a
// single image and picks a role by argv.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/compgenlab/varianthub-web/internal/api"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/runner"
	"github.com/compgenlab/varianthub-web/internal/store"
	webui "github.com/compgenlab/varianthub-web/web/embed"
)

// version is stamped at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}
	cmd := args[0]
	if cmd == "version" {
		fmt.Println("varianthub-web", version)
		return nil
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "migrate":
		return store.Migrate(ctx, cfg.DatabaseURL)
	case "seed":
		return seed(ctx, cfg)
	case "source":
		return cmdSource(ctx, cfg, args[1:])
	case "snapshot":
		return cmdSnapshot(ctx, cfg, args[1:])
	case "serve":
		return serve(ctx, cfg)
	case "worker":
		return worker(ctx, cfg)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// serve runs the HTTP API. It does not process jobs.
func serve(ctx context.Context, cfg *config.Config) error {
	q, err := queue.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer q.Close()

	// The API listens so ?wait= can be woken by a worker in another replica.
	q.StartListener(ctx)

	// The catalog backs the snapshot/source endpoints. It shares the database, so
	// a failure here is not survivable in practice -- but the server is still
	// useful for submitting and polling, so report it rather than refusing to
	// start, and let those endpoints answer 503.
	cat, err := catalog.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Printf("serve: catalog unavailable, /api/v1/snapshots and /sources will 503: %v", err)
	} else {
		defer cat.Close()
		if err := syncStorage(ctx, cfg, cat); err != nil {
			log.Printf("serve: storage config: %v", err)
		}
	}

	if !cfg.RequireToken {
		log.Printf("serve: /api/v1 is OPEN (VHW_REQUIRE_TOKEN=false)")
	}

	// The web UI is embedded at build time. A binary built without running the
	// frontend build still serves the API, which is what CI and API-only
	// deployments want — so this is a log line, not a fatal.
	var spa *api.SPA
	if files := webui.FS(); files != nil {
		spa = api.NewSPA(files)
		log.Printf("serve: web UI embedded")
	} else {
		log.Printf("serve: no web UI embedded (run `npm --prefix web run build`)")
	}

	return api.New(cfg, q, cat, spa).Run(ctx)
}

// worker runs the job pool. It serves no HTTP.
func worker(ctx context.Context, cfg *config.Config) error {
	q, err := queue.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer q.Close()

	q.SetMaxJobsPerIP(cfg.MaxJobsPerIP)
	q.StartListener(ctx)
	q.StartSweeper(ctx, cfg.JobTTL, sweepInterval(cfg.JobTTL))

	home, err := homeProvider(ctx, cfg)
	if err != nil {
		return err
	}
	exec := &runner.ExecRunner{
		Bin:     cfg.VarhubBin,
		Home:    home,
		Timeout: cfg.JobTimeout,
	}

	// The download path records what it fetched, so it needs the catalog too.
	cat, err := catalog.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer cat.Close()

	log.Printf("worker: %d worker(s), varhub=%s", cfg.Workers, cfg.VarhubBin)
	q.StartWorkers(ctx, cfg.Workers, adapt(exec, cat))

	<-ctx.Done()
	log.Printf("worker: shutting down")
	q.Wait()
	return nil
}

// homeProvider chooses where a job's annotation config comes from.
//
// Normally it is materialized per job from the Postgres catalog, so the service
// holds no annotation config locally. VHW_VARHUB_HOME overrides that with a
// fixed directory on disk — useful for debugging against a hand-built tree, and
// the only mode available before the catalog existed.
func homeProvider(ctx context.Context, cfg *config.Config) (runner.HomeProvider, error) {
	if cfg.VarhubHome != "" {
		log.Printf("worker: using fixed annotation home %s (catalog bypassed)", cfg.VarhubHome)
		return runner.FixedHome(cfg.VarhubHome), nil
	}
	cat, err := catalog.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	log.Printf("worker: materializing annotation config from the catalog (data=%s cache=%s)",
		cfg.DataDir, cfg.CacheDir)
	return &catalog.Materializer{
		Store:    cat,
		DataDir:  cfg.DataDir,
		CacheDir: cfg.CacheDir,
	}, nil
}

// syncStorage reconciles the deployment's declared filesystem locations into the
// catalog, so the config file stays authoritative for them and a path the
// deployment no longer mounts stops being offered.
func syncStorage(ctx context.Context, cfg *config.Config, cat *catalog.Store) error {
	decl, err := cfg.StorageLocations()
	if err != nil {
		return err
	}
	locs := make([]catalog.StorageLocation, 0, len(decl))
	for _, d := range decl {
		locs = append(locs, catalog.StorageLocation{
			ID: d.ID, Name: d.Name, Kind: catalog.StoragePath,
			URI: d.Path, FromConfig: true, IsDefault: d.Default,
		})
	}
	return cat.SyncConfigStorage(ctx, locs)
}

// seed populates an empty catalog with a starter snapshot.
func seed(ctx context.Context, cfg *config.Config) error {
	cat, err := catalog.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer cat.Close()
	if err := syncStorage(ctx, cfg, cat); err != nil {
		return err
	}
	return cat.Seed(ctx)
}

// adapt bridges runner.Runner to queue.Runner. The two are deliberately separate
// types: the queue knows nothing about how annotation happens, and the runner
// knows nothing about job persistence.
func adapt(r runner.Runner, cat *catalog.Store) queue.Runner {
	return func(ctx context.Context, job queue.Job, input []byte) (queue.Outcome, error) {
		switch job.Kind {
		case queue.KindDownload:
			return runDownload(ctx, r, cat, job, input)
		case queue.KindCleanup:
			return runCleanup(job, input)
		}
		res, err := r.Annotate(ctx, runner.Request{
			Kind:      job.Kind,
			Snapshot:  job.Snapshot,
			Selection: job.Selection,
			Body:      input,
		})
		if err != nil {
			// Log the full diagnostic; return only the safe message, which is what
			// gets stored on the job and served to clients.
			var ee *runner.ExitError
			if errors.As(err, &ee) {
				log.Printf("worker: job %s: %s", job.ID, ee.Detail())
			}
			return queue.Outcome{}, err
		}
		var cols []byte
		if len(res.Columns) > 0 {
			if b, mErr := json.Marshal(res.Columns); mErr == nil {
				cols = b
			} else {
				log.Printf("worker: job %s: encode columns: %v", job.ID, mErr)
			}
		}
		return queue.Outcome{Result: res.Variants, N: res.N, Columns: cols, Variants: true}, nil
	}
}

// runDownload provisions a snapshot's sources and records what landed.
//
// The inventory is written here, in the worker, because only the worker is
// guaranteed to have the storage volume mounted — the API server may not.
func runDownload(ctx context.Context, r runner.Runner, cat *catalog.Store,
	job queue.Job, input []byte) (queue.Outcome, error) {

	exec, ok := r.(*runner.ExecRunner)
	if !ok {
		return queue.Outcome{}, errors.New("download requires the exec runner")
	}
	var req struct {
		StorageID string   `json:"storage_id"`
		CacheDir  string   `json:"cache_dir"`
		Sources   []string `json:"sources"`
		Force     bool     `json:"force"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return queue.Outcome{}, fmt.Errorf("malformed download job: %w", err)
	}

	res, err := exec.Download(ctx, runner.DownloadRequest{
		Sources:  req.Sources,
		CacheDir: req.CacheDir,
		Force:    req.Force,
	})
	if err != nil {
		var ee *runner.ExitError
		if errors.As(err, &ee) {
			log.Printf("worker: download job %s: %s", job.ID, ee.Detail())
		}
		return queue.Outcome{}, err
	}

	// Attribute files to their source by the directory varhub lays out per
	// source: <cache>/<name>/<version>/... A file outside that shape belongs to no
	// source we can name, so it is inventoried but not attributed.
	all, err := cat.ListSources(ctx)
	if err != nil {
		return queue.Outcome{}, err
	}
	byID := map[string]catalog.Source{}
	for _, s := range all {
		byID[s.ID] = s
	}
	for _, id := range req.Sources {
		src, ok := byID[id]
		if !ok {
			continue // removed between enqueue and run
		}
		prefix := src.Name + string(os.PathSeparator) + src.Version + string(os.PathSeparator)
		var mine []catalog.SourceFile
		for _, f := range res.Files {
			if !strings.HasPrefix(f.Path, prefix) {
				continue
			}
			mine = append(mine, catalog.SourceFile{
				Path: f.Path, SizeBytes: f.SizeBytes, ModifiedAt: f.ModifiedAt,
			})
		}
		if err := cat.ReplaceSourceFiles(ctx, src.ID, req.StorageID, mine); err != nil {
			return queue.Outcome{}, fmt.Errorf("record files for %s: %w", src.Ref(), err)
		}
		log.Printf("worker: download job %s: %s → %d file(s)", job.ID, src.Ref(), len(mine))
	}

	// The job's "result" is the manifest of what landed, so the UI can show it
	// without a second call.
	body, err := json.Marshal(res.Files)
	if err != nil {
		return queue.Outcome{}, err
	}
	return queue.Outcome{Result: body, N: len(res.Files)}, nil
}

// runCleanup reclaims a removed source's files.
//
// The source row is already gone by the time this runs — the API deletes it and
// queues this — so there is nothing to reconcile afterwards; the job just frees
// the bytes and reports how many.
func runCleanup(job queue.Job, input []byte) (queue.Outcome, error) {
	var req struct {
		Root    string `json:"root"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return queue.Outcome{}, fmt.Errorf("malformed cleanup job: %w", err)
	}
	freed, err := runner.Cleanup(runner.CleanupRequest{
		Root: req.Root, Name: req.Name, Version: req.Version,
	})
	if err != nil {
		return queue.Outcome{}, err
	}
	log.Printf("worker: cleanup job %s: reclaimed %d bytes from %s/%s",
		job.ID, freed, req.Name, req.Version)
	body, err := json.Marshal(map[string]any{
		"freed_bytes": freed, "name": req.Name, "version": req.Version,
	})
	if err != nil {
		return queue.Outcome{}, err
	}
	return queue.Outcome{Result: body}, nil
}

// sweepInterval scales GC frequency to the TTL, clamped to a sane band: often
// enough that expired jobs do not linger, rarely enough that a long TTL does not
// mean a pointless hourly scan.
func sweepInterval(ttl time.Duration) time.Duration {
	const (
		minSweep = time.Minute
		maxSweep = time.Hour
	)
	switch iv := ttl / 10; {
	case iv < minSweep:
		return minSweep
	case iv > maxSweep:
		return maxSweep
	default:
		return iv
	}
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
varianthub-web — VariantHub API server

usage: varianthub-web <command>

commands:
  serve     run the HTTP API (/api/v1 plus /healthz and /version)
  worker    run the annotation job pool (execs the varhub CLI)
  migrate   apply pending SQL migrations, then exit
  seed      populate an empty catalog with a starter snapshot, then exit

catalog administration:
  source add [flags] <source.toml>   register a source from a varhub fragment
  source list                        list registered sources
  snapshot add <name> --build B --source S [--publish]
                                     create/update a snapshot pinning sources
  snapshot list                      list snapshots
  version   print the version

Configuration is read from the environment; see README.md. VHW_DATABASE_URL is
always required.
`, "\n"))
}
