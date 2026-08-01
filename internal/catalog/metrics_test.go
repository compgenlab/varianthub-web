package catalog

import (
	"context"
	"testing"
)

func TestSourceURLsExpandsTemplates(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want []string
	}{
		{
			name: "single url",
			toml: `[[sources]]
name = "clinvar"
version = "2026-01"
url = "https://example.org/clinvar.vcf.gz"
url_index = "https://example.org/clinvar.vcf.gz.tbi"
`,
			// The index is excluded: it is fetched whole either way, and the
			// figure is about how much data sits behind a network hop.
			want: []string{"https://example.org/clinvar.vcf.gz"},
		},
		{
			name: "per-chromosome template",
			toml: `[[sources]]
name = "gnomad"
version = "4.1"
url = "https://example.org/gnomad.{chrom}.vcf.bgz"
chroms = ["chr1", "chr2", "chrX"]
`,
			want: []string{
				"https://example.org/gnomad.chr1.vcf.bgz",
				"https://example.org/gnomad.chr2.vcf.bgz",
				"https://example.org/gnomad.chrX.vcf.bgz",
			},
		},
		{
			// CADD's shape: several file blocks under one source, which an
			// earlier version of the stream check got wrong by demanding a
			// single top-level url.
			name: "explicit file list",
			toml: `[[sources]]
name = "cadd"
version = "1.7"
  [[sources.files]]
  url = "https://example.org/whole_genome_SNVs.tsv.gz"
  [[sources.files]]
  url = "https://example.org/gnomad.genomes.indel.tsv.gz"
`,
			want: []string{
				"https://example.org/whole_genome_SNVs.tsv.gz",
				"https://example.org/gnomad.genomes.indel.tsv.gz",
			},
		},
		{
			name: "per-alt bigwig set defaults to acgt",
			toml: `[[sources]]
name = "am"
version = "1"
url = "https://example.org/am_{alt}.bw"
`,
			want: []string{
				"https://example.org/am_a.bw",
				"https://example.org/am_c.bw",
				"https://example.org/am_g.bw",
				"https://example.org/am_t.bw",
			},
		},
		{
			name: "a local source has no urls",
			toml: `[[sources]]
name = "local"
version = "1"
localpath = "/data/local.vcf.gz"
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SourceURLs(tc.toml)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d urls %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("url %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestBucketOf(t *testing.T) {
	for in, want := range map[string]string{
		"s3://vh-sources/prod": "vh-sources",
		"s3://vh-sources":      "vh-sources",
		"s3://a/b/c":           "a",
	} {
		if got := bucketOf(in); got != want {
			t.Errorf("bucketOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStorageUsage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, l := range []StorageLocation{
		{ID: "disk", Name: "Local disk", Kind: StoragePath, URI: "/var/lib/varhub", IsDefault: true},
		{ID: "bkt-a", Name: "Bucket A", Kind: StorageS3, URI: "s3://vh-a/prod"},
		{ID: "bkt-b", Name: "Bucket B", Kind: StorageS3, URI: "s3://vh-b"},
	} {
		if err := s.PutStorage(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	for _, src := range []string{"one", "two"} {
		if err := s.PutSource(ctx, Source{
			ID: src, Name: src, Version: "1", Kind: "vcf",
			TOML: "[[sources]]\nname = \"" + src + "\"\nversion = \"1\"\n",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Two sources on disk, one of them also copied into bucket A.
	if err := s.ReplaceSourceFiles(ctx, "one", "disk", []SourceFile{
		{Path: "one.vcf.gz", SizeBytes: 1000}, {Path: "one.vcf.gz.tbi", SizeBytes: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceSourceFiles(ctx, "two", "disk", []SourceFile{
		{Path: "two.vcf.gz", SizeBytes: 500},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceSourceFiles(ctx, "one", "bkt-a", []SourceFile{
		{Path: "one.vcf.gz", SizeBytes: 1000},
	}); err != nil {
		t.Fatal(err)
	}

	usage, err := s.StorageUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]StorageUsage{}
	for _, u := range usage {
		by[u.StorageID] = u
	}
	if len(by) != 3 {
		t.Fatalf("got %d locations, want 3 — an empty location must still appear", len(by))
	}
	if got := by["disk"]; got.Bytes != 1510 || got.Files != 3 || got.Sources != 2 {
		t.Errorf("disk = %+v; want 1510 bytes, 3 files, 2 sources", got)
	}
	if got := by["bkt-a"]; got.Bytes != 1000 || got.Bucket != "vh-a" {
		t.Errorf("bucket A = %+v; want 1000 bytes in bucket vh-a", got)
	}
	// A configured-but-empty bucket is a real answer, not a missing row.
	if got := by["bkt-b"]; got.Bytes != 0 || got.Files != 0 || got.Bucket != "vh-b" {
		t.Errorf("bucket B = %+v; want an empty vh-b", got)
	}
	// Buckets are reported apart, so two locations never merge into one figure.
	if by["bkt-a"].Bucket == by["bkt-b"].Bucket {
		t.Error("the two buckets collapsed into one")
	}
}

func TestCountSources(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.PutStorage(ctx, StorageLocation{
		ID: "disk", Name: "disk", Kind: StoragePath, URI: "/data",
	}); err != nil {
		t.Fatal(err)
	}
	put := func(id, kind, toml string) {
		t.Helper()
		if err := s.PutSource(ctx, Source{ID: id, Name: id, Version: "1", Kind: kind, TOML: toml}); err != nil {
			t.Fatal(err)
		}
	}
	put("down", "vcf", "[[sources]]\nname=\"down\"\nversion=\"1\"\nurl=\"https://x/d.gz\"\n")
	put("streamed", "vcf", "[[sources]]\nname=\"streamed\"\nversion=\"1\"\nstream=true\nurl=\"https://x/s.gz\"\n")
	put("builtins", "builtin", "[[sources]]\nname=\"builtins\"\nversion=\"1\"\ntype=\"builtin\"\n")
	put("waiting", "vcf", "[[sources]]\nname=\"waiting\"\nversion=\"1\"\nurl=\"https://x/w.gz\"\n")

	if err := s.ReplaceSourceFiles(ctx, "down", "disk", []SourceFile{
		{Path: "d.gz", SizeBytes: 1},
	}); err != nil {
		t.Fatal(err)
	}

	c, err := s.CountSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := SourceCounts{Total: 4, Provisioned: 1, Streamed: 1, Builtin: 1, Pending: 1}
	if c != want {
		t.Errorf("counts = %+v, want %+v", c, want)
	}
}
