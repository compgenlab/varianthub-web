package runner

import (
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FetchFile downloads a URI to a local path, verifying a checksum if given, and
// returns the number of bytes written.
//
// Written through a temp file and renamed, so an interrupted fetch cannot leave
// something that later looks complete. A digest that disagrees leaves nothing at
// all: an unverified reference is worse than a missing one, because a tool will
// happily annotate against the wrong genome and say nothing.
//
// http(s) only. s3:// needs either an SDK dependency here or a fetch subcommand
// in varhub, which already speaks it — the latter is the better answer and
// belongs with the scratch-space work, where S3 is the intended home.
func FetchFile(ctx context.Context, uri, dest, checksum string) (int64, error) {
	if strings.HasPrefix(uri, "s3://") {
		return 0, fmt.Errorf("s3:// references are not supported yet; " +
			"publish the file over https, or wait for scratch-space staging")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET %s: %s", uri, resp.Status)
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp)

	var h hash.Hash
	var want string
	if algo, value, ok := strings.Cut(checksum, ":"); ok {
		switch strings.ToLower(algo) {
		case "md5":
			h, want = md5.New(), strings.ToLower(value)
		case "sha256":
			h, want = sha256.New(), strings.ToLower(value)
		default:
			f.Close()
			return 0, fmt.Errorf("unsupported checksum algorithm %q (want md5 or sha256)", algo)
		}
	}
	var w io.Writer = f
	if h != nil {
		w = io.MultiWriter(f, h)
	}

	n, err := io.Copy(w, resp.Body)
	if err != nil {
		f.Close()
		return 0, fmt.Errorf("download %s: %w", uri, err)
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if h != nil {
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			return 0, fmt.Errorf("%s: checksum mismatch: got %s, want %s", uri, got, want)
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return 0, err
	}
	return n, nil
}

// NormalizeFasta makes a fetched FASTA indexable, returning the path to use.
//
// htslib's faidx can index an uncompressed FASTA or a bgzip one, and nothing
// else. Ensembl publishes the primary assembly as plain gzip, so handing it
// straight to a tool fails with "Cannot index files compressed with gzip,
// please use bgzip" from inside the container — a long way from the fetch that
// chose the file.
//
// Plain gzip is therefore decompressed. Uncompressing rather than recompressing
// to bgzip costs disk (about 3 GB against 900 MB) and saves a dependency on an
// external bgzip; a reference lives on a volume sized for tool data, where that
// is not the scarce resource. A bgzip file is already indexable and is left
// alone.
func NormalizeFasta(path string) (string, error) {
	if !strings.HasSuffix(path, ".gz") {
		return path, nil // already plain
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// BGZF is gzip with an "BC" extra subfield; gzip.Reader exposes it as Extra.
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("%s: not gzip: %w", filepath.Base(path), err)
	}
	if isBGZF(zr.Header.Extra) {
		zr.Close()
		return path, nil // bgzip: faidx handles it
	}

	plain := strings.TrimSuffix(path, ".gz")
	out, err := os.Create(plain)
	if err != nil {
		zr.Close()
		return "", err
	}
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		zr.Close()
		os.Remove(plain)
		return "", fmt.Errorf("decompress %s: %w", filepath.Base(path), err)
	}
	zr.Close()
	if err := out.Close(); err != nil {
		os.Remove(plain)
		return "", err
	}
	// The original is redundant once decompressed, and a reference is large
	// enough that keeping both is a real cost rather than a tidy safety net.
	_ = os.Remove(path)
	return plain, nil
}

// isBGZF reports whether a gzip extra field carries the BGZF "BC" subfield.
func isBGZF(extra []byte) bool {
	for i := 0; i+3 < len(extra); {
		si1, si2 := extra[i], extra[i+1]
		slen := int(extra[i+2]) | int(extra[i+3])<<8
		if si1 == 'B' && si2 == 'C' {
			return true
		}
		i += 4 + slen
	}
	return false
}
