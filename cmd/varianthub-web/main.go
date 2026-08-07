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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/compgenlab/varianthub-web/internal/api"
	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/catalog"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/identity"
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
	case "assets":
		return cmdAssets(ctx, cfg, args[1:])
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
	cat, err := openCatalog(ctx, cfg)
	if err != nil {
		log.Printf("serve: catalog unavailable, /api/v1/snapshots and /sources will 503: %v", err)
	} else {
		defer cat.Close()
		if err := syncStorage(ctx, cfg, cat); err != nil {
			log.Printf("serve: storage config: %v", err)
		}
	}

	// Accounts share the catalog's pool, so they are available exactly when it
	// is. Without them the server still runs: it just has no way to identify
	// anyone, which the middleware treats as everybody being anonymous.
	var ids *identity.Store
	if cat != nil {
		ids = identity.NewStore(cat.Pool())
		if err := announceBootstrap(ctx, ids); err != nil {
			log.Printf("serve: bootstrap: %v", err)
		}
	}

	if cfg.AllowAnonymous {
		log.Printf("serve: anonymous annotation is ENABLED (VHW_ALLOW_ANONYMOUS=true)")
	}
	if cfg.CILogonClientID != "" {
		if len(cfg.CILogonAutoProvision) > 0 {
			log.Printf("serve: CILogon sign-in enabled; accounts auto-created for %v",
				cfg.CILogonAutoProvision)
		} else {
			log.Printf("serve: CILogon sign-in enabled (invite-only — an administrator " +
				"creates the account, the first sign-in claims it)")
		}
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

	return api.New(cfg, q, cat, ids, spa).Run(ctx)
}

// announceBootstrap issues and logs the first-administrator credential when the
// installation has no administrator yet.
//
// Printed to the log rather than written to a file or an env var because the log
// is the one place an operator is already looking on first start, and because it
// is the shortest-lived: the token stops working the moment the first
// administrator account exists.
func announceBootstrap(ctx context.Context, ids *identity.Store) error {
	needs, err := ids.NeedsBootstrap(ctx)
	if err != nil || !needs {
		return err
	}
	secret, err := ids.IssueBootstrap(ctx)
	if err != nil {
		return err
	}
	log.Printf("serve: this installation has no administrator yet.")
	log.Printf("serve: create the first one with the bootstrap token below — it stops")
	log.Printf("serve: working as soon as that account exists, and is reissued on restart.")
	log.Printf("serve:")
	log.Printf("serve:     %s", secret)
	log.Printf("serve:")
	log.Printf("serve: POST /api/v1/admin/users {\"email\":..., \"password\":..., \"role\":\"admin\"}")
	log.Printf("serve: with `Authorization: Bearer <token>`, or open the web UI and follow the prompt.")
	return nil
}

// worker runs the job pool. It serves no HTTP.
func worker(ctx context.Context, cfg *config.Config) error {
	q, err := queue.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer q.Close()

	// Take back jobs whose holder stopped renewing — a worker that crashed, or
	// one killed mid-run. Safe to do while other workers are busy, because a
	// live one keeps its leases fresh; see ReclaimExpired.
	if n, rErr := q.ReclaimExpired(ctx); rErr != nil {
		return rErr
	} else if n > 0 {
		log.Printf("worker: reclaimed %d abandoned job(s)", n)
	}
	// Keep this worker's own claims alive, and keep watching for others going
	// quiet: a peer can die at any point, not only before we start.
	q.StartLeaseKeeper(ctx)

	q.SetMaxJobsPerIP(cfg.MaxJobsPerIP)
	q.StartListener(ctx)
	q.StartSweeper(ctx, cfg.JobTTL, sweepInterval(cfg.JobTTL))

	home, err := homeProvider(ctx, cfg)
	if err != nil {
		return err
	}
	exec := &runner.ExecRunner{
		Bin:             cfg.VarhubBin,
		Home:            home,
		Timeout:         cfg.JobTimeout,
		DownloadTimeout: cfg.DownloadTimeout,
		NoCache:         cfg.NoCache,
	}

	// The download path records what it fetched, so it needs the catalog too.
	cat, err := openCatalog(ctx, cfg)
	if err != nil {
		return err
	}
	defer cat.Close()

	if cfg.NoCache {
		// Loud, because it is a diagnostic mode with a real cost: every value is
		// recomputed and nothing is kept, so a repeated query pays full price.
		log.Printf("worker: annotation cache DISABLED (VHW_NO_CACHE) — every value " +
			"recomputed, nothing persisted")
	}
	log.Printf("worker: %d worker(s), varhub=%s", cfg.Workers, cfg.VarhubBin)
	q.SetSlots(cfg.JobSlots)
	q.StartWorkers(ctx, cfg.Workers, adapt(q, exec, cat, cfg.DataDir, cfg.VarhubBin))

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
	cat, err := openCatalog(ctx, cfg)
	if err != nil {
		return nil, err
	}
	log.Printf("worker: materializing annotation config from the catalog (data=%s cache=%s)",
		cfg.DataDir, cfg.CacheDir)
	return &catalog.Materializer{
		Store:      cat,
		DataDir:    cfg.DataDir,
		CacheDir:   cfg.CacheDir,
		References: cfg.References,
	}, nil
}

// syncStorage reconciles the deployment's declared locations into the catalog,
// so the config file stays authoritative for them and a target the deployment
// no longer provides stops being offered.
func syncStorage(ctx context.Context, cfg *config.Config, cat *catalog.Store) error {
	decl, err := cfg.StorageLocations()
	if err != nil {
		return err
	}
	locs := make([]catalog.StorageLocation, 0, len(decl))
	for _, d := range decl {
		kind := catalog.StoragePath
		if d.Kind == "s3" {
			kind = catalog.StorageS3
		}
		locs = append(locs, catalog.StorageLocation{
			ID: d.ID, Name: d.Name, Kind: kind,
			URI: d.Path, FromConfig: true, IsDefault: d.Default,
		})
	}
	return cat.SyncConfigStorage(ctx, locs)
}

// seed populates an empty catalog with a starter snapshot.
func seed(ctx context.Context, cfg *config.Config) error {
	cat, err := openCatalog(ctx, cfg)
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
//
// The queue is passed in as well, for the run's output. That is persistence, so
// it belongs to the queue rather than to either of the two types this bridges —
// and it is written outside the Outcome because an Outcome is the job's result,
// while a log exists for the runs that produced no result at all.
func adapt(q *queue.Queue, r runner.Runner, cat *catalog.Store, cfgDataDir, cfgVarhubBin string) queue.Runner {
	return func(ctx context.Context, job queue.Job, input []byte) (queue.Outcome, error) {
		switch job.Kind {
		case queue.KindDownload:
			return runDownload(ctx, q, r, cat, job, input)
		case queue.KindCleanup:
			return runCleanup(job, input)
		case queue.KindMove:
			return runMove(ctx, cat, job, input)
		}
		res, err := r.Annotate(ctx, runner.Request{
			Kind:      job.Kind,
			Snapshot:  job.Snapshot,
			Selection: job.Selection,
			Body:      input,
		})
		if err != nil {
			// The full diagnostic goes to the log and to the job, so it can be
			// read without shell access to this container — and survives the
			// container being replaced, which the log does not.
			var ee *runner.ExitError
			if errors.As(err, &ee) {
				log.Printf("worker: job %s: %s", job.ID, ee.Detail())
				storeLog(ctx, q, job.ID, ee.Detail())
			}
			return queue.Outcome{}, err
		}
		// Kept for a run that worked, as downloads already do. A job that
		// annotated nothing succeeds by every check made here, and the progress
		// output is the only thing that says whether the sources were consulted.
		storeLog(ctx, q, job.ID, res.Log)

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
// storeLog persists a run's output, best effort.
//
// Never fails the job: a log that could not be written is a worse thing to
// report than the outcome the job actually had, and the log is a diagnostic aid
// rather than part of the result.
func storeLog(ctx context.Context, q *queue.Queue, id, output string) {
	if q == nil || output == "" {
		return
	}
	// WithoutCancel because this often runs while the job's own context is
	// already cancelled — a timeout, a shutdown — and those are exactly the runs
	// whose output is most worth having.
	if err := q.SetLog(context.WithoutCancel(ctx), id, output); err != nil {
		log.Printf("worker: job %s: store log: %v", id, err)
	}
}

func runDownload(ctx context.Context, q *queue.Queue, r runner.Runner, cat *catalog.Store,
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
		NoStream  bool     `json:"no_stream"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return queue.Outcome{}, fmt.Errorf("malformed download job: %w", err)
	}

	// Mark them in flight before the work starts, so the catalog says
	// "installing" rather than "not installed" while a multi-hour tool setup
	// runs — the state a source is in for most of the time it matters.
	if cat != nil {
		if sErr := cat.SetSourceStates(context.WithoutCancel(ctx), req.Sources,
			catalog.StateInstalling, ""); sErr != nil {
			log.Printf("worker: job %s: mark installing: %v", job.ID, sErr)
		}
	}

	res, err := exec.Download(ctx, runner.DownloadRequest{
		Sources:  req.Sources,
		CacheDir: req.CacheDir,
		Force:    req.Force,
		NoStream: req.NoStream,
	})
	if err != nil {
		var ee *runner.ExitError
		if errors.As(err, &ee) {
			log.Printf("worker: download job %s: %s", job.ID, ee.Detail())
			storeLog(ctx, q, job.ID, ee.Detail())
		}
		if cat != nil {
			// WithoutCancel: a cancelled or timed-out job still has to leave the
			// source describing itself accurately, and its context is dead.
			if sErr := cat.SetSourceStates(context.WithoutCancel(ctx), req.Sources,
				catalog.StateFailed, err.Error()); sErr != nil {
				log.Printf("worker: job %s: mark failed: %v", job.ID, sErr)
			}
		}
		return queue.Outcome{}, err
	}
	if cat != nil {
		if sErr := cat.SetSourceStates(context.WithoutCancel(ctx), req.Sources,
			catalog.StateReady, ""); sErr != nil {
			log.Printf("worker: job %s: mark ready: %v", job.ID, sErr)
		}
	}
	// A successful download has output worth keeping too: which files it
	// fetched, what it skipped as already present, how long a build recipe
	// took. "It worked" is not the only question asked of a finished job.
	storeLog(ctx, q, job.ID, res.Log)

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
  assets list                        show helper files still held in Postgres
  assets backfill                    move them into the configured storage
  version   print the version

Configuration is read from varianthub-web.toml (or $VHW_CONFIG, or
/etc/varianthub-web/config.toml), with environment variables overriding it.
Copy varianthub-web.example.toml to start; see README.md. A database URL is
always required.
`, "\n"))
}

// runMove relocates a source's files from one storage location to another.
//
// Copy, verify, record, then delete. Never the other order: a move that removes
// the original first turns a transient failure into data that has to be fetched
// again from an upstream that may be slow, rate-limited, or gone. The catalog is
// updated only once every file has arrived, so a half-finished move leaves the
// source readable where it already was.
func runMove(ctx context.Context, cat *catalog.Store, job queue.Job, input []byte) (queue.Outcome, error) {
	var req struct {
		SourceID string `json:"source_id"`
		FromID   string `json:"from_storage"`
		ToID     string `json:"to_storage"`
		FromURI  string `json:"from_uri"`
		ToURI    string `json:"to_uri"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return queue.Outcome{}, fmt.Errorf("malformed move job: %w", err)
	}
	if cat == nil {
		return queue.Outcome{}, errors.New("catalog unavailable")
	}

	files, err := cat.SourceFiles(ctx, req.SourceID, req.FromID)
	if err != nil {
		return queue.Outcome{}, err
	}
	if len(files) == 0 {
		return queue.Outcome{}, fmt.Errorf("source %s has no files in %s", req.SourceID, req.FromID)
	}

	moved := make([]catalog.SourceFile, 0, len(files))
	var bytes int64
	for _, f := range files {
		src := joinLoc(req.FromURI, f.Path)
		dst := joinLoc(req.ToURI, f.Path)
		log.Printf("worker: move %s: %s -> %s", req.SourceID, src, dst)
		n, err := blob.Transfer(ctx, src, dst)
		if err != nil {
			// Nothing is deleted and the catalog is untouched, so the source is
			// still readable where it was.
			return queue.Outcome{}, fmt.Errorf("copy %s: %w", f.Path, err)
		}
		moved = append(moved, catalog.SourceFile{
			Path: f.Path, SizeBytes: n, ModifiedAt: f.ModifiedAt,
		})
		bytes += n
	}

	// Record the new location before removing the old, so an interruption here
	// leaves two copies rather than none.
	if err := cat.ReplaceSourceFiles(ctx, req.SourceID, req.ToID, moved); err != nil {
		return queue.Outcome{}, err
	}
	for _, f := range files {
		if err := blob.Remove(ctx, joinLoc(req.FromURI, f.Path)); err != nil {
			// The move succeeded; the leftovers are wasted space, not a failure.
			log.Printf("worker: move %s: could not remove %s: %v", req.SourceID, f.Path, err)
		}
	}
	if err := cat.ReplaceSourceFiles(ctx, req.SourceID, req.FromID, nil); err != nil {
		return queue.Outcome{}, err
	}

	log.Printf("worker: move %s: %d file(s), %d bytes now in %s",
		req.SourceID, len(moved), bytes, req.ToID)
	body, _ := json.Marshal(map[string]any{
		"source_id": req.SourceID, "files": len(moved), "size_bytes": bytes,
		"storage_id": req.ToID,
	})
	return queue.Outcome{Result: body, N: len(moved)}, nil
}

// joinLoc appends a source-relative path to a storage root, for either a
// filesystem path or an object locator.
func joinLoc(root, rel string) string {
	root = strings.TrimSuffix(root, "/")
	if strings.HasPrefix(root, "s3://") {
		return root + "/" + rel
	}
	return filepath.Join(root, rel)
}
