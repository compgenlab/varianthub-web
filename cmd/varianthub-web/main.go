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

	log.Printf("worker: %d worker(s), varhub=%s", cfg.Workers, cfg.VarhubBin)
	q.StartWorkers(ctx, cfg.Workers, adapt(exec))

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

// seed populates an empty catalog with a starter snapshot.
func seed(ctx context.Context, cfg *config.Config) error {
	cat, err := catalog.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer cat.Close()
	return cat.Seed(ctx)
}

// adapt bridges runner.Runner to queue.Runner. The two are deliberately separate
// types: the queue knows nothing about how annotation happens, and the runner
// knows nothing about job persistence.
func adapt(r runner.Runner) queue.Runner {
	return func(ctx context.Context, job queue.Job, input []byte) (queue.Outcome, error) {
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
		return queue.Outcome{Result: res.Variants, N: res.N, Columns: cols}, nil
	}
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
  version   print the version

Configuration is read from the environment; see README.md. VHW_DATABASE_URL is
always required.
`, "\n"))
}
