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
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/compgenlab/varianthub-web/internal/anncache"
	"github.com/compgenlab/varianthub-web/internal/api"
	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/cacherunner"
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
	// Before any storage is touched, in whichever subcommand this is.
	registerS3Sites(cfg)

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
	// The same reclaim, for what those jobs left on disk. varhub removes its own
	// scratch on every path it can return from, and on none where it is killed —
	// which is what an OOM and a rolling restart both do to a build in progress.
	//
	// Job-scoped scratch first, and exactly: the queue says which jobs are
	// running, so a directory named after one that is not can go immediately.
	// This is the case age could never handle — a job OOM-killed thirty seconds
	// ago leaves a workdir that is both very recent and certainly dead.
	if live, lErr := q.RunningIDs(ctx); lErr != nil {
		log.Printf("worker: could not list running jobs, skipping scratch sweep: %v", lErr)
	} else if n, freed, sErr := runner.SweepJobScratch(os.TempDir(), live, log.Printf); sErr != nil {
		log.Printf("worker: could not sweep job scratch: %v", sErr)
	} else if n > 0 {
		log.Printf("worker: reclaimed scratch for %d finished job(s), %.1f GB", n, float64(freed)/(1<<30))
	}
	// Then the age-based sweep, which is now only a backstop: it catches work
	// staged outside a job's own directory, and anything left by a version that
	// did not name its scratch.
	if n, freed, sErr := runner.SweepScratch(os.TempDir(), runner.ScratchMaxAge, log.Printf); sErr != nil {
		log.Printf("worker: could not sweep scratch: %v", sErr)
	} else if n > 0 {
		log.Printf("worker: reclaimed %d abandoned workdir(s), %.1f GB", n, float64(freed)/(1<<30))
	}
	// Keep this worker's own claims alive, and keep watching for others going
	// quiet: a peer can die at any point, not only before we start.
	q.StartLeaseKeeper(ctx)

	q.SetMaxJobsPerIP(cfg.MaxJobsPerIP)
	// What removes an expiring job's stored input. In the worker and not the
	// API because only one process should be doing the collecting, and this is
	// the one already running the sweep.
	q.SetObjectDisposer(disposeJobObjects)
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
		// The declared site's credentials, for the CLI this execs.
		ExtraEnv: blob.DefaultEnv(),
	}

	// The download path records what it fetched, so it needs the catalog too.
	cat, err := openCatalog(ctx, cfg)
	if err != nil {
		return err
	}
	defer cat.Close()

	annotator, closeCache, err := withCache(ctx, cfg, cat, exec)
	if err != nil {
		return err
	}
	defer closeCache()

	log.Printf("worker: %d worker(s), varhub=%s", cfg.Workers, cfg.VarhubBin)
	q.SetSlots(cfg.JobSlots)
	q.StartWorkers(ctx, cfg.Workers, adapt(q, annotator, cat, cfg.DataDir, cfg.VarhubBin))

	<-ctx.Done()
	log.Printf("worker: shutting down")
	q.Wait()
	return nil
}

// withCache puts the shared annotation cache in front of the engine, where the
// deployment wants one.
//
// The cache is here rather than inside varhub because varhub runs a source's
// tool steps as bash: a DSN in the job's home would be readable by any code a
// registered manifest chooses to run. Here it also gets to skip the process
// entirely, which is most of the saving.
//
// Returns the runner to use and a function to release the cache. Both are always
// usable — a deployment with no cache gets the engine unchanged.
func withCache(ctx context.Context, cfg *config.Config, cat *catalog.Store,
	exec *runner.ExecRunner) (runner.Runner, func(), error) {

	noop := func() {}
	if cfg.NoCache {
		// Loud, because it is a diagnostic mode with a real cost: every value is
		// recomputed and nothing is kept, so a repeated query pays full price. It
		// is also the only way to tell "asked and got nothing" from "replaying an
		// older, emptier answer", so it has to bypass this cache as well as
		// varhub's own.
		log.Printf("worker: annotation cache DISABLED (VHW_NO_CACHE) — every value " +
			"recomputed, nothing persisted")
		return exec, noop, nil
	}
	if cfg.DatabaseURL == "" {
		return exec, noop, nil
	}

	// Settings that cannot be read are not a reason to refuse to start. A rollout
	// applies the new deployments before the migration job runs, so a worker on a
	// new release routinely comes up against a schema that is a minute behind it
	// — and on a fresh cluster, against no schema at all. Failing here turns that
	// window into CrashLoopBackOff, which reads as a broken image rather than as
	// a migration that has not finished yet.
	//
	// The configured defaults are the right thing to fall back on, and the per-job
	// read below picks the stored values up as soon as they exist, without a
	// restart.
	site := catalog.SiteFromConfig(cfg)
	if stored, err := cat.EffectiveSite(ctx, site); err != nil {
		log.Printf("worker: site settings unavailable, using the configured defaults "+
			"(migrations may not have run yet): %v", err)
	} else {
		site = stored
	}
	if !site.CacheEnabled {
		log.Printf("worker: annotation cache off by setting; jobs will compute every value")
		// Still wrapped: the setting is read per job, so turning it back on takes
		// effect on the next job rather than on the next restart.
	}

	store, err := anncache.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	log.Printf("worker: shared annotation cache on %s (max %d unit(s), max age %s)",
		"postgres", site.CacheMaxEntries, orNever(site.CacheMaxAgeText))
	return &cacherunner.Runner{
			Inner:   exec,
			Cache:   store,
			Catalog: cat,
			// Read per job rather than captured, so an administrator's change
			// reaches a running worker.
			Site: func(ctx context.Context) catalog.Site {
				s, err := cat.EffectiveSite(ctx, catalog.SiteFromConfig(cfg))
				if err != nil {
					// The configured default rather than a guess: a database blip
					// should not silently change the cache policy.
					return catalog.SiteFromConfig(cfg)
				}
				return s
			},
		}, func() { store.Close() }, nil
}

func orNever(s string) string {
	if s == "" {
		return "unbounded"
	}
	return s
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

// registerS3Sites hands the declared [[s3]] blocks to the blob layer.
//
// Before anything can reach a bucket, and in every process that might: the API
// stores assets at registration and the worker reads them back, so a site known
// to one and not the other is a credential error in whichever was missed.
func registerS3Sites(cfg *config.Config) {
	if len(cfg.S3Sites) == 0 {
		return
	}
	sites := make([]blob.Site, 0, len(cfg.S3Sites))
	for _, s := range cfg.S3Sites {
		sites = append(sites, blob.Site{
			Name: s.Name, URI: s.URI, Endpoint: s.Endpoint, Region: s.Region,
			AccessKey: s.AccessKey, SecretKey: s.SecretKey, Default: s.Default,
		})
	}
	blob.RegisterSites(sites)
	log.Printf("config: %d S3 site(s) declared with their own credentials", len(sites))
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
		// Same streaming as provisioning. An annotation is usually short enough
		// that the log written at the end is sufficient — but the ones that are
		// not are exactly the ones that get killed, and those wrote nothing.
		alw := queue.NewLogWriter(ctx, q, job.ID)
		defer alw.Close(context.WithoutCancel(ctx))
		alw.Note("starting on worker " + q.WorkerID())

		// A stored input is fetched to a local file, because that is what the
		// engine takes. It is never read into this process: a chromosome's VCF
		// is hundreds of megabytes, and holding it here only to write it
		// straight back out is the copy this whole path exists to remove.
		var inputPath string
		if job.InputURI != "" {
			dir, mkErr := os.MkdirTemp("", "vhw-input-")
			if mkErr != nil {
				return queue.Outcome{}, fmt.Errorf("staging directory: %w", mkErr)
			}
			// Removed however this returns, including the error paths below.
			// Otherwise a worker that fails a few large jobs fills its disk and
			// then fails every job after them, for a reason that looks nothing
			// like the first failure.
			defer os.RemoveAll(dir)

			// Named as it is stored, ".gz" and all. What the file is called is
			// how a later reader knows whether it is compressed, rather than
			// each one deciding for itself from the bytes.
			inputPath = filepath.Join(dir, path.Base(job.InputURI))
			alw.Note("staging input from " + job.InputURI)
			n, dlErr := blob.Download(ctx, job.InputURI, inputPath)
			if dlErr != nil {
				return queue.Outcome{}, fmt.Errorf("stage input: %w", dlErr)
			}
			alw.Note(fmt.Sprintf("staged %d bytes to %s", n, inputPath))
		}

		res, err := r.Annotate(ctx, runner.Request{
			Sink:      alw.Line,
			Kind:      job.Kind,
			Snapshot:  job.Snapshot,
			Selection: job.Selection,
			Body:      input,
			InputPath: inputPath,
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

// disposeJobObjects removes the stored files of jobs the sweep has collected.
//
// Best effort, one at a time, and a failure on one does not stop the rest: the
// rows are already gone by the time this runs, so the only thing left to do is
// remove as much as possible and say what could not be. What survives is
// recoverable — the layout is jobs/<job-id>/, so a listing can find prefixes
// with no job — but it will not be found by anything keyed on the database.
//
// A background context because this runs during shutdown as often as not, and
// an object left behind because the process was stopping is exactly the kind
// that nothing later goes looking for.
func disposeJobObjects(ctx context.Context, uris []string) {
	var failed int
	for _, uri := range uris {
		if err := blob.Remove(context.WithoutCancel(ctx), uri); err != nil {
			failed++
			log.Printf("worker: could not remove %s from job storage: %v", uri, err)
		}
	}
	if n := len(uris) - failed; n > 0 {
		log.Printf("worker: removed %d expired job input(s) from job storage", n)
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

	// By capability, not by concrete type. The worker's runner is wrapped in the
	// annotation cache, and asking for *ExecRunner refused every download on any
	// installation that had one.
	exec, ok := r.(runner.Downloader)
	if !ok {
		return queue.Outcome{}, errors.New("this runner cannot download sources")
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

	// Stream this run's output into the job log as it happens.
	//
	// storeLog at the end covers a run that returns. Provisioning is dominated
	// by runs that do not: an OOM or a rolling restart kills the process, the
	// buffered output goes with it, and the job is left saying it was abandoned
	// with nothing about what it was doing. Flushed every few seconds, the log
	// holds everything up to the moment it died.
	lw := queue.NewLogWriter(ctx, q, job.ID)
	defer lw.Close(context.WithoutCancel(ctx))
	lw.Note("starting on worker " + q.WorkerID())

	res, err := exec.Download(ctx, runner.DownloadRequest{
		JobID:    job.ID,
		Sink:     lw.Line,
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

		cacheGTFGenes(ctx, r, cat, job.ID, src, lw.Note)
	}

	// The job's "result" is the manifest of what landed, so the UI can show it
	// without a second call.
	body, err := json.Marshal(res.Files)
	if err != nil {
		return queue.Outcome{}, err
	}
	return queue.Outcome{Result: body, N: len(res.Files)}, nil
}

// cacheGTFGenes records which genes a freshly provisioned GTF source knows, so a
// gene list can be validated against it without reading the file.
//
// Here, in the worker, because only the worker has the data volume: the API
// server mounts the config alone, so a form handler cannot read a GTF even in
// principle. This is the moment the file is known to be present and correct, and
// it is the moment it can have changed — a re-provisioned source is a different
// GTF, and a stale gene set would validate a list against genes that are no
// longer there.
//
// Never fails the job. The download succeeded, the files are on disk, and the
// source is usable for annotation; a gene cache that could not be built is a
// missing convenience, not a broken provision. It is reported to the job log
// rather than swallowed, because the gene-list form will otherwise refuse every
// gene with no explanation, and this is the only place that says why.
func cacheGTFGenes(ctx context.Context, r runner.Runner, cat *catalog.Store,
	jobID string, src catalog.Source, note func(string)) {

	if cat == nil || !src.IsGTF() {
		return
	}
	lister, ok := r.(runner.GeneLister)
	if !ok {
		return
	}
	// WithoutCancel for the same reason the state updates use it: a job that was
	// cancelled after the files landed still provisioned them, and the gene cache
	// belongs with the files.
	ctx = context.WithoutCancel(ctx)

	genes, err := lister.Genes(ctx, src.ID, src.Ref())
	if err != nil {
		log.Printf("worker: download job %s: read genes for %s: %v", jobID, src.Ref(), err)
		note(fmt.Sprintf("could not read %s's genes, so gene lists cannot be validated "+
			"against it yet: %v", src.Ref(), err))
		return
	}
	out := make([]catalog.Gene, 0, len(genes))
	for _, g := range genes {
		out = append(out, catalog.Gene{GeneID: g.GeneID, GeneName: g.GeneName})
	}
	if err := cat.ReplaceGTFGenes(ctx, src.ID, out); err != nil {
		log.Printf("worker: download job %s: store genes for %s: %v", jobID, src.Ref(), err)
		note(fmt.Sprintf("could not store %s's genes: %v", src.Ref(), err))
		return
	}
	log.Printf("worker: download job %s: %s → %d gene(s)", jobID, src.Ref(), len(out))
	note(fmt.Sprintf("%s: %d gene(s) available for gene lists", src.Ref(), len(out)))
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
