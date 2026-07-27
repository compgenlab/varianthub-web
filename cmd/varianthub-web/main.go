// Command varianthub-web is the VariantHub API server, its job worker, and its
// migration runner — one binary, three subcommands, so a deployment ships a
// single image and picks a role by argv.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/compgenlab/varianthub-web/internal/api"
	"github.com/compgenlab/varianthub-web/internal/config"
	"github.com/compgenlab/varianthub-web/internal/queue"
	"github.com/compgenlab/varianthub-web/internal/runner"
	"github.com/compgenlab/varianthub-web/internal/store"
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

	if !cfg.RequireToken {
		log.Printf("serve: /api/v1 is OPEN (VHW_REQUIRE_TOKEN=false)")
	}
	return api.New(cfg, q).Run(ctx)
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

	// Chunk 2 replaces FixedHome with a provider that materializes the annotation
	// tree from the Postgres catalog per job.
	exec := &runner.ExecRunner{
		Bin:     cfg.VarhubBin,
		Home:    runner.FixedHome(cfg.VarhubHome),
		Timeout: cfg.JobTimeout,
	}

	log.Printf("worker: %d worker(s), varhub=%s home=%s", cfg.Workers, cfg.VarhubBin, cfg.VarhubHome)
	q.StartWorkers(ctx, cfg.Workers, adapt(exec))

	<-ctx.Done()
	log.Printf("worker: shutting down")
	q.Wait()
	return nil
}

// adapt bridges runner.Runner to queue.Runner. The two are deliberately separate
// types: the queue knows nothing about how annotation happens, and the runner
// knows nothing about job persistence.
func adapt(r runner.Runner) queue.Runner {
	return func(ctx context.Context, job queue.Job, input []byte) ([]byte, int, error) {
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
			return nil, 0, err
		}
		return res.Variants, res.N, nil
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
  version   print the version

Configuration is read from the environment; see README.md. VHW_DATABASE_URL is
always required.
`, "\n"))
}
