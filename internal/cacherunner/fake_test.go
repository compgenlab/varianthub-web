package cacherunner

import (
	"compress/gzip"
	"context"
	"os"
	"sync"

	"github.com/compgenlab/varianthub-web/internal/runner"
)

// fakeEngine stands in for varhub: it writes an annotated VCF where it was told
// to and records how it was asked.
//
// A file rather than a value, because that is what the engine produces now and
// what this decorator has to work with — a fake that returned values would let a
// test pass against an interface the real thing does not have.
type fakeEngine struct {
	mu    sync.Mutex
	calls []runner.Request
	err   error
}

func (f *fakeEngine) Annotate(_ context.Context, req runner.Request) (runner.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.err != nil {
		return runner.Result{}, f.err
	}
	if req.OutputPath == "" {
		return runner.Result{}, os.ErrInvalid
	}
	out, err := os.Create(req.OutputPath)
	if err != nil {
		return runner.Result{}, err
	}
	defer out.Close()
	zw := gzip.NewWriter(out)
	if _, err := zw.Write([]byte("##fileformat=VCFv4.2\n" +
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n" +
		"chr1\t100\t.\tA\tG\t.\t.\t.\n")); err != nil {
		zw.Close()
		return runner.Result{}, err
	}
	if err := zw.Close(); err != nil {
		return runner.Result{}, err
	}
	return runner.Result{VCFPath: req.OutputPath, N: 1}, nil
}
