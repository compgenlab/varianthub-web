package runner

import (
	"bytes"
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
	"os/exec"
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

// PrepareFasta makes a fetched FASTA usable by a tool, returning the path to use.
//
// A tool random-accesses the reference, which needs a .fai — and htslib can only
// index an uncompressed or BGZF file. Ensembl publishes the GRCh38 primary
// assembly as plain gzip, which is neither, so handing it straight to VEP fails
// with "Cannot index files compressed with gzip, please use bgzip" from inside
// the container, a long way from the fetch that chose it.
//
// So: recompress to BGZF when it is not already, then index. Both are varhub
// subcommands, which keeps htslib out of this image and means one
// implementation of these formats rather than two that can disagree.
//
// Plain gzip is streamed through the decompressor into bgzip rather than landing
// as an intermediate file — a decompressed GRCh38 is about 3 GB that would exist
// only to be read once.
func PrepareFasta(ctx context.Context, varhubBin, path string) (string, error) {
	kind, err := fastaKind(path)
	if err != nil {
		return "", err
	}

	final := path
	switch kind {
	case fastaBGZF:
		// Already indexable.
	case fastaPlain:
		final = path + ".gz"
		if err := runBgzip(ctx, varhubBin, final, func(w io.Writer) error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		}); err != nil {
			return "", err
		}
		_ = os.Remove(path)
	case fastaGzip:
		final = strings.TrimSuffix(path, ".gz") + ".bgz.gz"
		if err := runBgzip(ctx, varhubBin, final, func(w io.Writer) error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			zr, err := gzip.NewReader(f)
			if err != nil {
				return err
			}
			defer zr.Close()
			_, err = io.Copy(w, zr)
			return err
		}); err != nil {
			return "", err
		}
		_ = os.Remove(path)
		// Named plainly now that the original is gone.
		renamed := strings.TrimSuffix(path, ".gz") + ".gz"
		if err := os.Rename(final, renamed); err == nil {
			final = renamed
		}
	}

	cmd := exec.CommandContext(ctx, varhubBin, "faidx", final)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("faidx %s: %w: %s", filepath.Base(final), err,
			strings.TrimSpace(string(out)))
	}
	return final, nil
}

// runBgzip pipes src into `varhub bgzip`, writing BGZF to dest.
func runBgzip(ctx context.Context, varhubBin, dest string, src func(io.Writer) error) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := exec.CommandContext(ctx, varhubBin, "bgzip")
	cmd.Stdout = out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bgzip: %w", err)
	}
	copyErr := src(in)
	in.Close()
	if err := cmd.Wait(); err != nil {
		os.Remove(dest)
		return fmt.Errorf("bgzip: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	if copyErr != nil {
		os.Remove(dest)
		return fmt.Errorf("bgzip input: %w", copyErr)
	}
	return out.Close()
}

type fastaCompression int

const (
	fastaPlain fastaCompression = iota
	fastaGzip
	fastaBGZF
)

// fastaKind reports how a file is compressed. BGZF is gzip carrying a "BC"
// extra subfield; plain gzip has no extra field at all.
func fastaKind(path string) (fastaCompression, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hdr := make([]byte, 18)
	n, err := io.ReadFull(f, hdr)
	if err != nil && n < 2 {
		return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if hdr[0] != 0x1f || hdr[1] != 0x8b {
		return fastaPlain, nil
	}
	if n >= 14 && hdr[3]&0x04 != 0 && hdr[12] == 'B' && hdr[13] == 'C' {
		return fastaBGZF, nil
	}
	return fastaGzip, nil
}

// CopyTo writes a local file to a destination that may be a filesystem path or
// an object locator.
//
// s3:// is delegated to varhub, which already speaks it — keeping object-store
// access in one place rather than adding a second SDK and a second set of
// credentials to reason about.
func CopyTo(ctx context.Context, src, dst string) error {
	if strings.HasPrefix(dst, "s3://") {
		return fmt.Errorf("s3 durable copies are not wired up yet (%s)", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
