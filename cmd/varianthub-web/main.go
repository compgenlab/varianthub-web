// Command varianthub-web is the VariantHub API server, its chunk worker, and
// its migration runner — one binary, three subcommands, so a deployment ships
// a single image and picks a role by argv.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/compgenlab/varianthub-web/internal/fanout"
	"github.com/compgenlab/varianthub-web/internal/identity"
	"github.com/compgenlab/varianthub-web/internal/jobstore"
	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/runner"
	"github.com/compgenlab/varianthub-web/internal/store"
	"github.com/compgenlab/varianthub-web/internal/vcfmerge"
	webui "github.com/compgenlab/varianthub-web/web/embed"

	"github.com/compgenlab/cghts/vcf"
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
	case "sweep-storage":
		return sweepStorage(ctx, cfg, args[1:])
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// serve runs the HTTP API. It does not process chunks.
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

// worker runs the chunk pool. It serves no HTTP.
func worker(ctx context.Context, cfg *config.Config) error {
	q, err := queue.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer q.Close()

	// Take back chunks whose holder stopped renewing — a worker that crashed,
	// or one killed mid-run. Safe to do while other workers are busy, because
	// a live one keeps its leases fresh; see ReclaimExpired.
	if n, rErr := q.ReclaimExpired(ctx); rErr != nil {
		return rErr
	} else if n > 0 {
		log.Printf("worker: reclaimed %d abandoned chunk(s)", n)
	}
	// The same reclaim, for what those chunks left on disk. varhub removes its
	// own scratch on every path it can return from, and on none where it is
	// killed — which is what an OOM and a rolling restart both do to a build
	// in progress.
	//
	// Chunk-scoped scratch first, and exactly: the queue says which chunks are
	// running, so a directory named after one that is not can go immediately.
	// This is the case age could never handle — a chunk OOM-killed thirty
	// seconds ago leaves a workdir that is both very recent and certainly
	// dead.
	if live, lErr := q.RunningIDs(ctx); lErr != nil {
		log.Printf("worker: could not list running chunks, skipping scratch sweep: %v", lErr)
	} else if n, freed, sErr := runner.SweepChunkScratch(os.TempDir(), live, log.Printf); sErr != nil {
		log.Printf("worker: could not sweep chunk scratch: %v", sErr)
	} else if n > 0 {
		log.Printf("worker: reclaimed scratch for %d finished chunk(s), %.1f GB", n, float64(freed)/(1<<30))
	}
	// Then the age-based sweep, which is now only a backstop: it catches work
	// staged outside a chunk's own directory, and anything left by a version
	// that did not name its scratch.
	if n, freed, sErr := runner.SweepScratch(os.TempDir(), runner.ScratchMaxAge, log.Printf); sErr != nil {
		log.Printf("worker: could not sweep scratch: %v", sErr)
	} else if n > 0 {
		log.Printf("worker: reclaimed %d abandoned workdir(s), %.1f GB", n, float64(freed)/(1<<30))
	}
	// Keep this worker's own claims alive, and keep watching for others going
	// quiet: a peer can die at any point, not only before we start.
	q.StartLeaseKeeper(ctx)

	q.SetMaxJobsPerIP(cfg.MaxJobsPerIP)
	// What removes an expiring chunk's stored input. In the worker and not the
	// API because only one process should be doing the collecting, and this is
	// the one already running the sweep.
	q.SetObjectDisposer(disposeChunkObjects)
	q.StartListener(ctx)
	q.StartSweeper(ctx, cfg.JobTTL, sweepInterval(cfg.JobTTL))
	// And the files nothing in the database points at. Daily, because it lists
	// the whole of job storage to find them and the things it collects are
	// leftovers from a crash rather than a steady product.
	jobstore.Start(ctx, q, cfg.JobStorage, 24*time.Hour)

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
	q.StartWorkers(ctx, cfg.Workers, adapt(q, annotator, cat, cfg.JobStorage, cfg.Version, chunkSizeFor(ctx, cfg, cat)))

	<-ctx.Done()
	log.Printf("worker: shutting down")
	q.Wait()
	return nil
}

// withCache puts the shared annotation cache in front of the engine, where the
// deployment wants one.
//
// The cache is here rather than inside varhub because varhub runs a source's
// tool steps as bash: a DSN in the chunk's home would be readable by any code
// a registered manifest chooses to run. Here it also gets to skip the process
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

	// Settings that cannot be read are not a reason to refuse to start. A
	// rollout applies the new deployments before the migration chunk runs, so
	// a worker on a new release routinely comes up against a schema that is a
	// minute behind it — and on a fresh cluster, against no schema at all.
	// Failing here turns that window into CrashLoopBackOff, which reads as a
	// broken image rather than as a migration that has not finished yet.
	//
	// The configured defaults are the right thing to fall back on, and the
	// per-chunk read below picks the stored values up as soon as they exist,
	// without a restart.
	site := catalog.SiteFromConfig(cfg)
	if stored, err := cat.EffectiveSite(ctx, site); err != nil {
		log.Printf("worker: site settings unavailable, using the configured defaults "+
			"(migrations may not have run yet): %v", err)
	} else {
		site = stored
	}
	if !site.CacheEnabled {
		log.Printf("worker: annotation cache off by setting; chunks will compute every value")
		// Still wrapped: the setting is read per chunk, so turning it back on
		// takes effect on the next chunk rather than on the next restart.
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
		// Read per chunk rather than captured, so an administrator's change
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

// homeProvider chooses where a chunk's annotation config comes from.
//
// Normally it is materialized per chunk from the Postgres catalog, so the
// service holds no annotation config locally. VHW_VARHUB_HOME overrides that
// with a fixed directory on disk — useful for debugging against a hand-built
// tree, and the only mode available before the catalog existed.
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

// adapt bridges runner.Runner to queue.Runner. The two are deliberately
// separate types: the queue knows nothing about how annotation happens, and
// the runner knows nothing about chunk persistence.
//
// The queue is passed in as well, for the run's output. That is persistence,
// so it belongs to the queue rather than to either of the two types this
// bridges — and it is written outside the Outcome because an Outcome is the
// chunk's result, while a log exists for the runs that produced no result at
// all.
func adapt(q *queue.Queue, r runner.Runner, cat *catalog.Store, cfgJobStorage, cfgVersion string, chunkSize func() int) queue.Runner {
	return func(ctx context.Context, chunk queue.Chunk, input []byte) (queue.Outcome, error) {
		switch chunk.Kind {
		case queue.KindSplit:
			return runSplitChunk(ctx, q, chunk, cfgJobStorage, chunkSize())
		case queue.KindCollect:
			return runCollectChunk(ctx, q, chunk, cfgJobStorage)
		case queue.KindDownload:
			return runDownload(ctx, q, r, cat, chunk, input)
		case queue.KindCleanup:
			return runCleanup(chunk, input)
		case queue.KindMove:
			return runMove(ctx, cat, chunk, input)
		}
		// Same streaming as provisioning. An annotation is usually short enough
		// that the log written at the end is sufficient — but the ones that are
		// not are exactly the ones that get killed, and those wrote nothing.
		alw := queue.NewLogWriter(ctx, q, chunk.ID)
		defer alw.Close(context.WithoutCancel(ctx))
		alw.Note("starting on worker " + q.WorkerID())

		// A stored input is fetched to a local file, because that is what the
		// engine takes. It is never read into this process: a chromosome's VCF
		// is hundreds of megabytes, and holding it here only to write it
		// straight back out is the copy this whole path exists to remove.
		var inputPath string
		if chunk.InputURI != "" {
			dir, mkErr := os.MkdirTemp("", "vhw-input-")
			if mkErr != nil {
				return queue.Outcome{}, fmt.Errorf("staging directory: %w", mkErr)
			}
			// Removed however this returns, including the error paths below.
			// Otherwise a worker that fails a few large chunks fills its disk
			// and then fails every chunk after them, for a reason that looks
			// nothing like the first failure.
			defer os.RemoveAll(dir)

			// Named as it is stored, ".gz" and all. What the file is called is
			// how a later reader knows whether it is compressed, rather than
			// each one deciding for itself from the bytes.
			inputPath = filepath.Join(dir, path.Base(chunk.InputURI))
			alw.Note("staging input from " + chunk.InputURI)
			n, dlErr := blob.Download(ctx, chunk.InputURI, inputPath)
			if dlErr != nil {
				return queue.Outcome{}, fmt.Errorf("stage input: %w", dlErr)
			}
			alw.Note(fmt.Sprintf("staged %d bytes to %s", n, inputPath))
		}

		res, err := r.Annotate(ctx, runner.Request{
			Sink:      alw.Line,
			Kind:      chunk.Kind,
			Snapshot:  chunk.Snapshot,
			Selection: chunk.Selection,
			Body:      input,
			InputPath: inputPath,
		})
		if err != nil {
			// The full diagnostic goes to the log and to the chunk, so it can
			// be read without shell access to this container — and survives
			// the container being replaced, which the log does not.
			var ee *runner.ExitError
			if errors.As(err, &ee) {
				log.Printf("worker: chunk %s: %s", chunk.ID, ee.Detail())
				storeLog(ctx, q, chunk.ID, ee.Detail())
			}
			return queue.Outcome{}, err
		}
		// Kept for a run that worked, as downloads already do. A chunk that
		// annotated nothing succeeds by every check made here, and the
		// progress output is the only thing that says whether the sources were
		// consulted.
		storeLog(ctx, q, chunk.ID, res.Log)

		var cols []byte
		if len(res.Columns) > 0 {
			if b, mErr := json.Marshal(res.Columns); mErr == nil {
				cols = b
			} else {
				log.Printf("worker: chunk %s: encode columns: %v", chunk.ID, mErr)
			}
		}
		out := queue.Outcome{Result: res.Variants, N: res.N, Columns: cols, Variants: true}

		// Assemble the answer-as-a-VCF here, while the submitted file is still
		// staged. It used to be built on every download — the whole file parsed
		// and rewritten per request — and that is why the input had to be kept
		// for as long as the results were downloadable.
		//
		// Best effort: a build that fails must not fail a chunk whose
		// annotation succeeded. Without a stored VCF the export falls back to
		// rendering from rows, which is the same answer more slowly and with
		// less of the submitter's file in it.
		if chunk.ChunkIndex != nil {
			// A piece's output is part of a larger file: gzipped, and
			// headerless unless it is the first. Stored under the job's
			// prefix so the join can find every piece without asking about
			// each one.
			//
			// Failing to store it fails the chunk, which is what makes the
			// join refuse rather than produce a file missing a range of the
			// genome — the one wrong answer here that looks like a right one.
			if err := storePieceOutput(ctx, q, chunk, cfgJobStorage, inputPath, res, alw); err != nil {
				return queue.Outcome{}, err
			}
			return out, nil
		}
		// Every job, not only one submitted as a file. A locus list has no
		// submitted VCF to merge onto, so its answer is rendered sites-only —
		// but it is the same object, under the same name, built by the same
		// step. That is what lets every export be a conversion of one stored
		// file rather than a second rendering from a copy of the same data in
		// Postgres.
		uri, mErr := buildResultVCF(ctx, cfgJobStorage, cfgVersion, chunk, inputPath, res, alw)
		if mErr != nil {
			log.Printf("worker: chunk %s: build result VCF: %v", chunk.ID, mErr)
			alw.Note("··· could not pre-build the annotated VCF; downloads will " +
				"render from the result rows instead")
			return out, nil
		}
		out.VCFURI = uri
		if inputPath != "" {
			// The input existed to be annotated and to be merged onto. Both are
			// done and the merged file is stored, so it is scrap — keeping it
			// until expiry meant holding two copies of every submission for a
			// week.
			//
			// After the result is stored, never before. Reversed, a failure in
			// between would leave a chunk with neither the file it was sent nor
			// the answer built from it.
			dropInput(ctx, q, chunk.ID, alw)
		}
		return out, nil
	}
}

// dropInput removes a chunk's submitted file, once something else holds its
// content.
//
// Best effort and never fatal: the chunk succeeded, and failing it now over
// housekeeping would throw away work that is finished. What is left behind is
// collected by the TTL sweep, which reads the same row this deletes.
func dropInput(ctx context.Context, q *queue.Queue, id string, alw *queue.LogWriter) {
	// WithoutCancel because this often runs as a chunk is being torn down, and
	// an input left behind because the process was stopping is exactly the
	// kind nothing later goes looking for.
	bg := context.WithoutCancel(ctx)
	dropped, err := q.DropInput(bg, id)
	if err != nil {
		log.Printf("worker: chunk %s: drop input row: %v", id, err)
		return
	}
	if dropped == "" {
		return
	}
	if err := blob.Remove(bg, dropped); err != nil {
		log.Printf("worker: chunk %s: remove input %s: %v", id, dropped, err)
		return
	}
	alw.Note("··· submitted file removed; the annotated VCF stands in for it")
}

// buildResultVCF stores a chunk's answer as a VCF, and reports where it went.
//
// Two sources, one object. A submitted file is read back and the annotations set
// on its records, which is what keeps the submitter's ID, QUAL, FILTER, INFO,
// FORMAT and sample columns; a locus list, which never had a file, is rendered
// sites-only. Either way the job ends up with one object under one name, so
// nothing downstream has to ask which kind of job produced it.
//
// Streamed from the staged input straight into the upload, so the file is never
// held in memory or written to disk a second time — a chromosome's worth of VCF
// would be hundreds of megabytes of both.
func buildResultVCF(ctx context.Context, jobStorage, version string, chunk queue.Chunk,
	inputPath string, res runner.Result, alw *queue.LogWriter) (string, error) {

	// runner.Column and queue.Column are the same struct declared twice — the
	// runner describes what the engine reported, the queue describes what was
	// stored — so this converts rather than translates. If they ever stop
	// matching, this stops compiling, which is the right place to find out.
	cols := make([]queue.Column, len(res.Columns))
	for i, c := range res.Columns {
		cols[i] = queue.Column(c)
	}
	meta := vcfmerge.Meta{Version: version, JobID: chunk.JobID, Snapshot: chunk.Snapshot}

	// A pipe rather than a buffer: the write happens as the upload reads, so
	// neither side has to hold the file.
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeResultVCF(pw, meta, cols, inputPath, res))
	}()

	// Under the job's prefix, not the chunk's. Storage is laid out by job and
	// the sweep decides what to keep from the set of job ids; an object written
	// under a chunk id belongs to no job it can see, so it would be collected
	// as scrap on the next pass — a result that vanishes hours after the job
	// reported done.
	uri := queue.ObjectURI(jobStorage, chunk.JobID, queue.ResultName)
	if err := blob.PutReader(ctx, uri, pr); err != nil {
		pr.CloseWithError(err)
		return "", err
	}
	alw.Note("··· annotated VCF stored at " + uri)
	return uri, nil
}

// writeResultVCF gzips the job's answer into w.
//
// Compressed because this object is kept for the job's whole life and an
// annotated chromosome is hundreds of megabytes. The name says so — see
// queue.ResultName — so every reader is told rather than sniffing.
func writeResultVCF(w io.Writer, meta vcfmerge.Meta, cols []queue.Column,
	inputPath string, res runner.Result) error {

	zw := gzip.NewWriter(w)
	if err := writeAnnotated(zw, meta, cols, inputPath, res); err != nil {
		return err
	}
	// Closing the gzip writer is what flushes its final block. Skipped, the
	// object uploads cleanly and is truncated garbage on the way back.
	return zw.Close()
}

func writeAnnotated(w io.Writer, meta vcfmerge.Meta, cols []queue.Column,
	inputPath string, res runner.Result) error {

	if inputPath == "" {
		// No submitted file: the answer is the engine's rows, written out as a
		// sites-only VCF.
		variants, err := vcfmerge.Variants(res.Variants)
		if err != nil {
			return err
		}
		return vcfmerge.Render(w, meta, cols, vcfmerge.SliceStream(variants))
	}

	ann, err := vcfmerge.DecodeAnnotations(res.Variants)
	if err != nil {
		return err
	}
	in, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer in.Close()

	// The staged file keeps the name it was stored under, so whether it is
	// compressed is something this is told rather than something it works out.
	var src io.Reader = in
	if strings.HasSuffix(inputPath, ".gz") {
		gz, gzErr := gzip.NewReader(in)
		if gzErr != nil {
			return fmt.Errorf("%s is named .gz but is not gzip: %w", inputPath, gzErr)
		}
		defer gz.Close()
		src = gz
	}
	rd, err := vcf.NewVcfReader(src)
	if err != nil {
		return err
	}
	hdr, err := rd.Header()
	if err != nil {
		return err
	}
	_, err = vcfmerge.Merge(rd, w, hdr, cols, ann)
	return err
}

// sweepStorage removes job-storage files that no job owns, on demand.
//
// The worker does this on a timer already. It is a subcommand as well because
// the timer is invisible: an operator looking at a storage bill wants to see
// what would go before it goes, and wants to run it now rather than wait a
// day. It also makes the sweep usable as a scheduled chunk by anyone who would
// rather schedule it than have the worker do it.
func sweepStorage(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("sweep-storage", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be removed, remove nothing")
	grace := fs.Duration("grace", jobstore.DefaultGrace,
		"leave objects newer than this alone; an upload is stored before its chunk row exists")
	if err := fs.Parse(args); err != nil {
		return err
	}

	q, err := queue.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer q.Close()

	res, err := jobstore.Sweep(ctx, q, cfg.JobStorage, *grace, *dryRun)
	if err != nil {
		return err
	}
	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	log.Printf("sweep-storage: %d object(s) under %s; %d belong to no chunk, %s %d (%.1f MB)",
		res.Scanned, cfg.JobStorage, res.Orphans, verb, res.Removed, float64(res.Bytes)/(1<<20))
	if res.Skipped > 0 {
		log.Printf("sweep-storage: %d left alone as newer than %s — an upload exists "+
			"before its chunk row does, so a recent orphan may be a chunk being submitted",
			res.Skipped, *grace)
	}
	return nil
}

// disposeChunkObjects removes the stored files of chunks the sweep has
// collected.
//
// Best effort, one at a time, and a failure on one does not stop the rest: the
// rows are already gone by the time this runs, so the only thing left to do is
// remove as much as possible and say what could not be. What survives is
// recoverable — the layout is jobs/<chunk-id>/, so a listing can find prefixes
// with no chunk — but it will not be found by anything keyed on the database.
//
// A background context because this runs during shutdown as often as not, and
// an object left behind because the process was stopping is exactly the kind
// that nothing later goes looking for.
func disposeChunkObjects(ctx context.Context, uris []string) {
	var failed int
	for _, uri := range uris {
		if err := blob.Remove(context.WithoutCancel(ctx), uri); err != nil {
			failed++
			log.Printf("worker: could not remove %s from job storage: %v", uri, err)
		}
	}
	if n := len(uris) - failed; n > 0 {
		log.Printf("worker: removed %d expired chunk input(s) from job storage", n)
	}
}

// runDownload provisions a snapshot's sources and records what landed.
//
// The inventory is written here, in the worker, because only the worker is
// guaranteed to have the storage volume mounted — the API server may not.
// storeLog persists a run's output, best effort.
//
// Never fails the chunk: a log that could not be written is a worse thing to
// report than the outcome the chunk actually had, and the log is a diagnostic
// aid rather than part of the result.
func storeLog(ctx context.Context, q *queue.Queue, id, output string) {
	if q == nil || output == "" {
		return
	}
	// WithoutCancel because this often runs while the chunk's own context is
	// already cancelled — a timeout, a shutdown — and those are exactly the
	// runs whose output is most worth having.
	if err := q.SetLog(context.WithoutCancel(ctx), id, output); err != nil {
		log.Printf("worker: chunk %s: store log: %v", id, err)
	}
}

func runDownload(ctx context.Context, q *queue.Queue, r runner.Runner, cat *catalog.Store,
	chunk queue.Chunk, input []byte) (queue.Outcome, error) {

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
		return queue.Outcome{}, fmt.Errorf("malformed download chunk: %w", err)
	}

	// Mark them in flight before the work starts, so the catalog says
	// "installing" rather than "not installed" while a multi-hour tool setup
	// runs — the state a source is in for most of the time it matters.
	if cat != nil {
		if sErr := cat.SetSourceStates(context.WithoutCancel(ctx), req.Sources,
			catalog.StateInstalling, ""); sErr != nil {
			log.Printf("worker: chunk %s: mark installing: %v", chunk.ID, sErr)
		}
	}

	// Stream this run's output into the chunk log as it happens.
	//
	// storeLog at the end covers a run that returns. Provisioning is dominated
	// by runs that do not: an OOM or a rolling restart kills the process, the
	// buffered output goes with it, and the chunk is left saying it was
	// abandoned with nothing about what it was doing. Flushed every few
	// seconds, the log holds everything up to the moment it died.
	lw := queue.NewLogWriter(ctx, q, chunk.ID)
	defer lw.Close(context.WithoutCancel(ctx))
	lw.Note("starting on worker " + q.WorkerID())

	res, err := exec.Download(ctx, runner.DownloadRequest{
		JobID:    chunk.ID,
		Sink:     lw.Line,
		Sources:  req.Sources,
		CacheDir: req.CacheDir,
		Force:    req.Force,
		NoStream: req.NoStream,
	})
	if err != nil {
		var ee *runner.ExitError
		if errors.As(err, &ee) {
			log.Printf("worker: download chunk %s: %s", chunk.ID, ee.Detail())
			storeLog(ctx, q, chunk.ID, ee.Detail())
		}
		if cat != nil {
			// WithoutCancel: a cancelled or timed-out chunk still has to leave
			// the source describing itself accurately, and its context is
			// dead.
			if sErr := cat.SetSourceStates(context.WithoutCancel(ctx), req.Sources,
				catalog.StateFailed, err.Error()); sErr != nil {
				log.Printf("worker: chunk %s: mark failed: %v", chunk.ID, sErr)
			}
		}
		return queue.Outcome{}, err
	}
	if cat != nil {
		if sErr := cat.SetSourceStates(context.WithoutCancel(ctx), req.Sources,
			catalog.StateReady, ""); sErr != nil {
			log.Printf("worker: chunk %s: mark ready: %v", chunk.ID, sErr)
		}
	}
	// A successful download has output worth keeping too: which files it
	// fetched, what it skipped as already present, how long a build recipe
	// took. "It worked" is not the only question asked of a finished chunk.
	storeLog(ctx, q, chunk.ID, res.Log)

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
		log.Printf("worker: download chunk %s: %s → %d file(s)", chunk.ID, src.Ref(), len(mine))

		cacheGTFGenes(ctx, r, cat, chunk.ID, src, lw.Note)
	}

	// The chunk's "result" is the manifest of what landed, so the UI can show
	// it without a second call.
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
// Never fails the chunk. The download succeeded, the files are on disk, and
// the source is usable for annotation; a gene cache that could not be built is
// a missing convenience, not a broken provision. It is reported to the chunk
// log rather than swallowed, because the gene-list form will otherwise refuse
// every gene with no explanation, and this is the only place that says why.
func cacheGTFGenes(ctx context.Context, r runner.Runner, cat *catalog.Store,
	jobID string, src catalog.Source, note func(string)) {

	if cat == nil || !src.IsGTF() {
		return
	}
	lister, ok := r.(runner.GeneLister)
	if !ok {
		return
	}
	// WithoutCancel for the same reason the state updates use it: a chunk that
	// was cancelled after the files landed still provisioned them, and the
	// gene cache belongs with the files.
	ctx = context.WithoutCancel(ctx)

	genes, err := lister.Genes(ctx, src.ID, src.Ref())
	if err != nil {
		log.Printf("worker: download chunk %s: read genes for %s: %v", jobID, src.Ref(), err)
		note(fmt.Sprintf("could not read %s's genes, so gene lists cannot be validated "+
			"against it yet: %v", src.Ref(), err))
		return
	}
	out := make([]catalog.Gene, 0, len(genes))
	for _, g := range genes {
		out = append(out, catalog.Gene{GeneID: g.GeneID, GeneName: g.GeneName})
	}
	if err := cat.ReplaceGTFGenes(ctx, src.ID, out); err != nil {
		log.Printf("worker: download chunk %s: store genes for %s: %v", jobID, src.Ref(), err)
		note(fmt.Sprintf("could not store %s's genes: %v", src.Ref(), err))
		return
	}
	log.Printf("worker: download chunk %s: %s → %d gene(s)", jobID, src.Ref(), len(out))
	note(fmt.Sprintf("%s: %d gene(s) available for gene lists", src.Ref(), len(out)))
}

// runCleanup reclaims a removed source's files.
//
// The source row is already gone by the time this runs — the API deletes it
// and queues this — so there is nothing to reconcile afterwards; the chunk
// just frees the bytes and reports how many.
func runCleanup(chunk queue.Chunk, input []byte) (queue.Outcome, error) {
	var req struct {
		Root    string `json:"root"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return queue.Outcome{}, fmt.Errorf("malformed cleanup chunk: %w", err)
	}
	freed, err := runner.Cleanup(runner.CleanupRequest{
		Root: req.Root, Name: req.Name, Version: req.Version,
	})
	if err != nil {
		return queue.Outcome{}, err
	}
	log.Printf("worker: cleanup chunk %s: reclaimed %d bytes from %s/%s",
		chunk.ID, freed, req.Name, req.Version)
	body, err := json.Marshal(map[string]any{
		"freed_bytes": freed, "name": req.Name, "version": req.Version,
	})
	if err != nil {
		return queue.Outcome{}, err
	}
	return queue.Outcome{Result: body}, nil
}

// sweepInterval scales GC frequency to the TTL, clamped to a sane band: often
// enough that expired chunks do not linger, rarely enough that a long TTL does
// not mean a pointless hourly scan.
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
  worker    run the annotation chunk pool (execs the varhub CLI)
  migrate   apply pending SQL migrations, then exit
  seed      populate an empty catalog with a starter snapshot, then exit

  sweep-storage [--dry-run] [--grace 1h]
            remove job-storage files that no job owns. The worker does this
            daily; this is for looking now, and for deployments that would
            rather schedule it themselves.

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
func runMove(ctx context.Context, cat *catalog.Store, chunk queue.Chunk, input []byte) (queue.Outcome, error) {
	var req struct {
		SourceID string `json:"source_id"`
		FromID   string `json:"from_storage"`
		ToID     string `json:"to_storage"`
		FromURI  string `json:"from_uri"`
		ToURI    string `json:"to_uri"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return queue.Outcome{}, fmt.Errorf("malformed move chunk: %w", err)
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

// storeChunkVCF merges a chunk's annotations onto the chunk itself and stores
// the result.
//
// The same merge an unsplit chunk does, written to the job's prefix instead of
// the chunk's, and headerless unless this is the first chunk. What must not be
// stored here is the engine's JSON: it is individually well-formed, so a join
// concatenates it without complaint into a file no VCF reader accepts.
func storeChunkVCF(ctx context.Context, prefix string, chunk queue.Chunk, inputPath string,
	res runner.Result, alw *queue.LogWriter) (string, error) {

	if inputPath == "" {
		return "", errors.New("a chunk needs its staged input to merge onto")
	}
	ann, err := vcfmerge.DecodeAnnotations(res.Variants)
	if err != nil {
		return "", err
	}
	cols := make([]queue.Column, len(res.Columns))
	for i, c := range res.Columns {
		cols[i] = queue.Column(c)
	}

	in, err := os.Open(inputPath)
	if err != nil {
		return "", err
	}
	defer in.Close()
	var src io.Reader = in
	if strings.HasSuffix(inputPath, ".gz") {
		gz, gzErr := gzip.NewReader(in)
		if gzErr != nil {
			return "", fmt.Errorf("%s is named .gz but is not gzip: %w", inputPath, gzErr)
		}
		defer gz.Close()
		src = gz
	}
	rd, err := vcf.NewVcfReader(src)
	if err != nil {
		return "", err
	}
	hdr, err := rd.Header()
	if err != nil {
		return "", err
	}

	pr, pw := io.Pipe()
	go func() {
		_, mErr := vcfmerge.Merge(rd, pw, hdr, cols, ann)
		pw.CloseWithError(mErr)
	}()
	uri, err := fanout.StoreChunkResult(ctx, prefix, *chunk.ChunkIndex, pr, alw.Note)
	if err != nil {
		pr.CloseWithError(err)
		return "", err
	}
	return uri, nil
}

// runSplitChunk cuts a submitted VCF into pieces and queues a chunk for each.
//
// The first chunk of a VCF job, and the only one that exists when the caller is
// handed their job id. It produces no variants of its own: what it produces is
// the rest of the job, and the answer arrives when the collect chunk that
// follows the last piece finishes.
func runSplitChunk(ctx context.Context, q *queue.Queue, chunk queue.Chunk,
	jobStorage string, chunkSize int) (queue.Outcome, error) {

	alw := queue.NewLogWriter(ctx, q, chunk.ID)
	defer alw.Close(context.WithoutCancel(ctx))
	alw.Note("starting on worker " + q.WorkerID())

	if chunk.InputURI == "" {
		return queue.Outcome{}, errors.New("a split chunk needs a stored input")
	}
	dir, err := os.MkdirTemp("", "vhw-split-in-")
	if err != nil {
		return queue.Outcome{}, err
	}
	defer os.RemoveAll(dir)

	local := filepath.Join(dir, path.Base(chunk.InputURI))
	if _, err := blob.Download(ctx, chunk.InputURI, local); err != nil {
		return queue.Outcome{}, fmt.Errorf("stage input: %w", err)
	}

	n, err := fanout.RunSplit(ctx, q, chunk, local, jobStorage,
		fanout.DefaultCgkitBin, chunkSize, alw.Note)
	if err != nil {
		return queue.Outcome{}, err
	}
	alw.Note(fmt.Sprintf("··· %d chunk(s) queued; this chunk is done when they are", n))

	// The submitted file has been cut up and every piece stored, so it is no
	// longer the only copy of anything. Dropped here rather than at expiry,
	// for the same reason a merged result lets an ordinary VCF chunk drop its
	// input.
	dropInput(ctx, q, chunk.ID, alw)
	return queue.Outcome{}, nil
}

// runCollectChunk joins a finished job's chunks into the answer.
func runCollectChunk(ctx context.Context, q *queue.Queue, chunk queue.Chunk,
	jobStorage string) (queue.Outcome, error) {

	alw := queue.NewLogWriter(ctx, q, chunk.ID)
	defer alw.Close(context.WithoutCancel(ctx))
	alw.Note("starting on worker " + q.WorkerID())

	if chunk.JobID == "" {
		return queue.Outcome{}, errors.New("a collect chunk needs a job")
	}
	uri, err := fanout.RunCollect(ctx, q, chunk.JobID, jobStorage, alw.Note)
	if err != nil {
		return queue.Outcome{}, err
	}
	alw.Note("··· joined file stored at " + uri)

	// Returned rather than filed anywhere by hand. This chunk completes its job
	// (see queue.Chunk.CompletesJob), so recording its outcome is what points
	// the job at this file — which is how a caller holding only the job id
	// reaches an answer produced by a chunk they never saw.
	alw.Note("··· available from job " + chunk.JobID)
	return queue.Outcome{VCFURI: uri}, nil
}

// storePieceOutput stores one piece's annotated output under its job's prefix,
// where the collect will look for it.
//
// It does not have to tell anyone. The join is already queued and waiting on
// these — see fanout.RunSplit — so finishing here is the whole of this piece's
// obligation, and a worker will pick the join up once the last of them is done.
//
// A failure to store is reported by failing the chunk. The alternative is a
// joined file missing a range of the genome, which reads exactly like one where
// those variants had nothing to say.
func storePieceOutput(ctx context.Context, q *queue.Queue, chunk queue.Chunk,
	jobStorage, inputPath string, res runner.Result, alw *queue.LogWriter) error {

	bg := context.WithoutCancel(ctx)
	prefix := queue.JobPrefix(jobStorage, chunk.JobID)
	if b, ok, err := q.GetJob(bg, chunk.JobID); err == nil && ok && b.Prefix != "" {
		prefix = b.Prefix
	}
	if _, err := storeChunkVCF(bg, prefix, chunk, inputPath, res, alw); err != nil {
		alw.Note("··· could not store this chunk's output; the job cannot be joined")
		return fmt.Errorf("store chunk result: %w", err)
	}
	return nil
}

// chunkSizeFor resolves the split chunk size at the moment a split runs.
//
// Read per chunk rather than captured at boot, matching how the cache setting
// is resolved: an administrator changing it should reach a running worker
// rather than wait for a redeploy. A database that cannot be read falls back
// to the configured default instead of guessing, so a blip cannot silently
// change how a submission is cut.
func chunkSizeFor(ctx context.Context, cfg *config.Config, cat *catalog.Store) func() int {
	return func() int {
		if cat == nil {
			return catalog.SiteFromConfig(cfg).ChunkSize()
		}
		site, err := cat.EffectiveSite(ctx, catalog.SiteFromConfig(cfg))
		if err != nil {
			return catalog.SiteFromConfig(cfg).ChunkSize()
		}
		return site.ChunkSize()
	}
}
