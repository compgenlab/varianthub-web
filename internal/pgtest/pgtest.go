// Package pgtest gives a test its own Postgres schema, migrated and torn down
// with the test.
//
// Shared rather than copied into each package that needs it. The harness has one
// detail that is easy to get subtly wrong — every migration applied in numeric
// order, into a schema the connection actually searches — and a copy that drifts
// fails as "relation does not exist" in whichever test happened to touch the
// table, which says nothing about the harness.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN names the environment variable holding a test database URL.
//
//	docker run -d --name vhw-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=varianthub \
//	  -p 55440:5432 postgres:16
//	export VHW_TEST_DATABASE_URL='postgres://postgres:test@localhost:55440/varianthub?sslmode=disable'
const EnvDSN = "VHW_TEST_DATABASE_URL"

// Pool returns a pool bound to a fresh, fully migrated schema, and skips the
// test when no database is configured.
//
// A schema per test rather than a database per test: creating one is cheap
// enough to do in every test, which is what makes the tests independent of the
// order they run in.
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
// The schema is created and migrated before this returns, because a store that
// runs a statement on connect (the queue's crash recovery does) would otherwise
// meet a schema that is not there yet.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("%s not set; skipping tests that need Postgres", EnvDSN)
	}
	ctx := context.Background()

	var ddl strings.Builder
	for _, f := range migrations(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		ddl.Write(b)
		ddl.WriteString("\n")
	}

	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	setup, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := setup.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		setup.Close()
		t.Fatalf("create schema: %v", err)
	}
	if _, err := setup.Exec(ctx, `SET search_path TO `+schema+`; `+ddl.String()); err != nil {
		setup.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	setup.Close()

	t.Cleanup(func() {
		drop, err := pgxpool.New(context.Background(), dsn)
		if err == nil {
			_, _ = drop.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			drop.Close()
		}
	})
	return dsn + "&search_path=" + schema
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
