#!/bin/sh
# Seed a starter VARHUB_HOME so a fresh stack can annotate immediately.
#
# This seeds *files*, not database rows: the annotation catalog still lives in
# TOML on disk. Chunk 2 moves it into Postgres, at which point this script is
# replaced by a catalog seed and the varhub-home volume disappears.
#
# Idempotent — if config.toml already exists the volume is left alone, so a
# `docker compose up` on an existing stack never clobbers local edits.
set -eu

HOME_DIR="${VARHUB_HOME:-/varhub-home}"

if [ -f "$HOME_DIR/config.toml" ]; then
  echo "seed: $HOME_DIR already initialized, leaving it alone"
  exit 0
fi

echo "seed: initializing $HOME_DIR"
mkdir -p "$HOME_DIR/annotations/snapshots" "$HOME_DIR/annotations/sources/builtins/1"

cat > "$HOME_DIR/config.toml" <<'EOF'
# Starter config for the dev stack. The built-in annotators need no downloaded
# data, so this works offline; add real sources with `varhub source add`.
data_dir        = "$VARHUB_HOME/data"
cache_dir       = "$VARHUB_HOME/data/cache"
annotations_dir = "./annotations"
default_snapshot = "dev"
EOF

# A snapshot's name comes from its filename; the manifest key for defaults is
# `default_annotations` (not `defaults`).
cat > "$HOME_DIR/annotations/snapshots/dev.toml" <<'EOF'
title       = "Dev starter snapshot"
description = "Built-in annotators only — no downloaded data required."
assembly    = "GRCh38"
sources     = ["builtins:1"]
default_annotations = ["auto_id", "tstv", "indel"]
EOF

cat > "$HOME_DIR/annotations/sources/builtins/1/builtins-1.toml" <<'EOF'
[[sources]]
  type    = "builtin"
  name    = "builtins"
  version = "1"

  [[sources.annotations]]
    builtin = "auto_id"
    name    = "auto_id"

  [[sources.annotations]]
    builtin = "tstv"
    name    = "tstv"

  [[sources.annotations]]
    builtin = "indel"
    name    = "indel"
EOF

mkdir -p "$HOME_DIR/data/cache"

# Prove the config parses before the worker depends on it: a broken seed should
# fail here, loudly, not later as a confusing job failure.
varhub -home "$HOME_DIR" -snapshot dev annotation list dev

echo "seed: ready"
