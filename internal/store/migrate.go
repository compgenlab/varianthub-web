// Package store holds schema migration and (from Chunk 2) the Postgres catalog.
package store

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-web/migrations"
)

// Migrate applies every migration not yet recorded, in filename order, each in
// its own transaction alongside the row that records it. A migration and its
// bookkeeping therefore commit together — a crash mid-run cannot leave a
// migration applied but unrecorded, which is what makes re-running safe.
func Migrate(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migration (
		  name       TEXT PRIMARY KEY,
		  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migration: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := pool.Query(ctx, `SELECT name FROM schema_migration`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		applied[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var ran int
	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migration (name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
		log.Printf("migrate: applied %s", name)
		ran++
	}
	if ran == 0 {
		log.Printf("migrate: already up to date (%d migration(s))", len(names))
	}
	return nil
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
