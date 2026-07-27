// Package migrations embeds the SQL schema migrations.
//
// This directory is a Go package purely so go:embed can reach the .sql files —
// embed cannot look outside its own package directory, and the migrations are
// worth keeping at the repo root where they are easy to find and review.
package migrations

import "embed"

// FS holds every migration, named NNNN_description.sql and applied in filename
// order. Migrations are append-only: never edit one that has shipped.
//
//go:embed *.sql
var FS embed.FS
