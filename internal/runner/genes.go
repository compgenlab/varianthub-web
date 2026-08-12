package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Gene is one gene's identity, as a GTF source reports it.
type Gene struct {
	GeneID   string
	GeneName string
}

// GeneLister lists the genes a GTF source knows about.
//
// Named as its own capability for the same reason Downloader is: the worker holds
// one runner and asks it for several unrelated things, and a decorator that has
// an opinion about one of them must not silently remove the rest. See
// cacherunner's capability test.
type GeneLister interface {
	Genes(ctx context.Context, sourceID, ref string) ([]Gene, error)
}

var _ GeneLister = (*ExecRunner)(nil)

// Genes asks the CLI for every gene in a GTF source.
//
// Shelled out rather than parsed here because this service has no business
// reading genomics files: varhub owns GTF parsing, knows where a provisioned
// source actually landed (which may be an object store), and prefers the bgzipped
// copy — the raw download is pruned after indexing, so a naive read of the
// manifest's path would find nothing.
//
// Bounded by the download timeout rather than the annotation one. A linear scan
// of a 1.5 GB GENCODE GTF is minutes of work, on the same scale as provisioning
// and nothing like a query.
func (r *ExecRunner) Genes(ctx context.Context, sourceID, ref string) ([]Gene, error) {
	bin := r.Bin
	if bin == "" {
		bin = "varhub"
	}
	if d := r.downloadTimeout(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	if sourceID == "" || ref == "" {
		return nil, errors.New("no source to list genes for")
	}
	for _, a := range []string{sourceID, ref} {
		if err := safeArg(a); err != nil {
			return nil, err
		}
	}

	provider, ok := r.Home.(SourceHomeProvider)
	if !ok {
		return nil, errors.New(
			"this deployment cannot read a source's genes (no catalog-backed home provider)")
	}
	// The same synthesized single-source home provisioning uses, so the cache root
	// this source actually landed in is the one varhub reads.
	home, cleanup, err := provider.HomeForSources(ctx, []string{sourceID})
	if err != nil {
		return nil, fmt.Errorf("prepare gene-listing home: %w", err)
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, bin,
		"-home", home, "-snapshot", "provision", "genes", "--format", "tsv", ref)
	cmd.Env = append(os.Environ(), "VARHUB_HOME="+home)
	cmd.Env = append(cmd.Env, r.ExtraEnv...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	// Read as it is written rather than buffering the whole output first. GENCODE
	// is ~78k lines; holding the text and then the parsed genes is twice the peak
	// for no reason, on a worker that is also running annotations.
	var genes []Gene
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		id, name, ok := strings.Cut(sc.Text(), "\t")
		if !ok || id == "" {
			continue
		}
		genes = append(genes, Gene{GeneID: id, GeneName: name})
	}
	scanErr := sc.Err()

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if scanErr != nil {
		return nil, fmt.Errorf("read genes: %w", scanErr)
	}
	// An empty result is a failure, not an answer. A GTF with no genes in it is
	// not a thing; what it really means is that the file was not where varhub
	// looked, or is not the format it was declared as — and storing zero genes
	// would make every list built against it fail validation on every gene, with
	// nothing to say why.
	if len(genes) == 0 {
		return nil, fmt.Errorf("%s reported no genes at all — is it really a GTF?", ref)
	}
	return genes, nil
}
