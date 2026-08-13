// Package pgtest gives a test its own Postgres database, migrated and torn down
// with the test.
//
// Shared rather than copied into each package that needs it. The harness has one
// detail that is easy to get subtly wrong — every migration applied in numeric
// order, into a database the connection actually uses — and a copy that drifts
// fails as "relation does not exist" in whichever test happened to touch the
// table, which says nothing about the harness.
//
// # Why a template
//
// Each test used to create a schema and replay every migration into it. That was
// 2.54 seconds per test against 33 migrations, and it was 93% of the setup cost:
// creating 31 tables and their indexes is simply slow, and it got slower with
// every migration added. Across the suite it was around eight minutes of doing
// the same work over and over.
//
// Measured alternatives, per test:
//
//	replay every migration       2540ms
//	squash them into one file    no better — the objects created are the same
//	TRUNCATE every table         2470ms — rewrites each table *and index* file
//	DELETE FROM every table        32ms — but one shared schema, and FK ordering
//	CREATE DATABASE ... TEMPLATE  204ms — a file copy, isolation unchanged
//
// So: build the schema once into a template database, then clone it per test.
// Twelve times faster, and every test still gets a genuinely pristine database
// rather than one another test has been living in.
package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN names the environment variable holding a test database URL.
//
//	docker run -d --name vhw-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=varianthub \
//	  -p 55440:5432 postgres:16
//	export VHW_TEST_DATABASE_URL='postgres://postgres:test@localhost:55440/varianthub?sslmode=disable'
const EnvDSN = "VHW_TEST_DATABASE_URL"

// Pool returns a pool bound to a fresh, fully migrated database, and skips the
// test when no database is configured.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), DSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// DSN is Pool for a caller that needs the connection string rather than a pool
// — anything that opens its own, as the queue does.
//
// The database is created and migrated before this returns, because a store that
// runs a statement on connect (the queue's crash recovery does) would otherwise
// meet a schema that is not there yet.
func DSN(t *testing.T) string {
	t.Helper()
	admin := os.Getenv(EnvDSN)
	if admin == "" {
		t.Skipf("%s not set; skipping tests that need Postgres", EnvDSN)
	}
	ctx := context.Background()

	tmpl := ensureTemplate(t, admin)

	// Unique per test. The timestamp is enough within one process and the pid
	// separates the packages, which `go test ./...` runs concurrently.
	name := fmt.Sprintf("vhw_t%d_%d", os.Getpid(), time.Now().UnixNano())

	setup, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// A template cannot be cloned while anything is connected to it, and every
	// package cloning at once is exactly when that race shows up. Retry rather
	// than fail: the window is the moment another process is finishing its own
	// build, and it closes on its own.
	var cloneErr error
	for attempt := 0; attempt < 20; attempt++ {
		_, cloneErr = setup.Exec(ctx,
			fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, tmpl))
		if cloneErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	setup.Close()
	if cloneErr != nil {
		t.Fatalf("clone template %s: %v", tmpl, cloneErr)
	}

	t.Cleanup(func() {
		drop, err := pgxpool.New(context.Background(), admin)
		if err != nil {
			return
		}
		defer drop.Close()
		// FORCE because a pool that has not finished closing still holds a
		// connection, and a database left behind is one more thing the next run
		// has to step around.
		_, _ = drop.Exec(context.Background(),
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
	return withDatabase(t, admin, name)
}

// templateOnce guards the build within one process; the advisory lock below
// guards it across the several that `go test ./...` starts.
var templateOnce sync.Map // template name -> *sync.Once

// ensureTemplate builds the template database if it is not already there, and
// returns its name.
//
// The name carries a hash of every migration's contents, so a change to any of
// them yields a different template and the old one is simply unused. That is
// what makes staleness impossible: there is no cache to invalidate, because a
// stale template is a template nobody asks for.
func ensureTemplate(t *testing.T, admin string) string {
	t.Helper()
	ddl, sum := migrationDDL(t)
	name := "vhw_tmpl_" + sum

	once, _ := templateOnce.LoadOrStore(name, &sync.Once{})
	once.(*sync.Once).Do(func() { buildTemplate(t, admin, name, ddl) })
	return name
}

// buildTemplate creates and migrates the template, serialized across processes.
func buildTemplate(t *testing.T, admin, name, ddl string) {
	t.Helper()
	ctx := context.Background()

	// One connection, not a pool: the advisory lock is held by a session, and a
	// pool is free to hand the unlock to a different one.
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Serialize the build across the packages `go test ./...` runs at once.
	// Without it several would create the same database and all but one would
	// fail — or worse, one would start cloning a template another was still
	// migrating.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, name); err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, name) //nolint:errcheck

	var exists bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).
		Scan(&exists); err != nil {
		t.Fatalf("look for template: %v", err)
	}
	if exists {
		return
	}

	// Built under a working name and renamed at the end, so the finished name
	// only ever appears on a database that is fully migrated. A crash midway
	// leaves scrap rather than a half-built template that later runs would
	// happily clone.
	building := fmt.Sprintf("%s_building_%d", name, os.Getpid())
	if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, building)); err != nil {
		t.Fatalf("clear working database: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+building); err != nil {
		t.Fatalf("create template: %v", err)
	}

	// Migrate it on its own connection, then close that connection: a template
	// cannot be cloned while anything is connected to it.
	buildDSN := withDatabase(t, admin, building)
	bc, err := pgx.Connect(ctx, buildDSN)
	if err != nil {
		t.Fatalf("connect to template: %v", err)
	}
	_, execErr := bc.Exec(ctx, ddl)
	bc.Close(ctx)
	if execErr != nil {
		conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, building)) //nolint:errcheck
		t.Fatalf("apply migrations: %v", execErr)
	}

	if _, err := conn.Exec(ctx,
		fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s`, building, name)); err != nil {
		// Another process finished first, which is a race this is allowed to
		// lose: its template is built from the same migrations and is therefore
		// the same database.
		conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, building)) //nolint:errcheck
		var exists bool
		if qErr := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).
			Scan(&exists); qErr != nil || !exists {
			t.Fatalf("name the template: %v", err)
		}
	}
}

// withDatabase rewrites a DSN to point at a different database on the same
// server.
func withDatabase(t *testing.T, dsn, db string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvDSN, err)
	}
	u.Path = "/" + db
	return u.String()
}

// migrationDDL is every migration concatenated, with a hash of the result.
//
// The hash covers the contents rather than the filenames, so editing a
// migration during development builds a new template instead of reusing one
// created from the previous version — the kind of staleness that shows up as a
// missing column in a test that never touched the file.
func migrationDDL(t *testing.T) (ddl, sum string) {
	t.Helper()
	var b strings.Builder
	for _, f := range migrations(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		b.Write(raw)
		b.WriteString("\n")
	}
	h := sha256.Sum256([]byte(b.String()))
	return b.String(), hex.EncodeToString(h[:])[:12]
}

// migrations are every migration, discovered rather than listed.
//
// The list used to be written by hand, so adding a table meant remembering to
// add it here too, and forgetting showed up as a missing relation somewhere
// unrelated. Globbing means a new migration is exercised by the existing tests
// the moment it lands.
//
// Sorted, because migrations are ordered by their numeric prefix and a later one
// alters what an earlier one created.
func migrations(t *testing.T) []string {
	t.Helper()
	// Every caller is a package one level under internal/, which is the layout
	// this asserts by failing loudly rather than by finding nothing.
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migrations found; the glob or the repository layout has moved")
	}
	sort.Strings(files)
	return files
}

// ErrNoDatabase is returned by helpers that report rather than skip.
var ErrNoDatabase = errors.New("no test database configured")
