package fanout

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Queue is what the phases need from the job queue.
//
// An interface so the phases can be tested without a database. They are the
// part of this that decides how a submission becomes many jobs, and that is
// worth being able to check directly.
type Queue interface {
	CreateBatch(ctx context.Context, jobID, prefix string) (string, error)
	SetChunkCount(ctx context.Context, batchID string, n int) error
	Enqueue(ctx context.Context, j queue.NewJob) (string, error)
	BatchChunks(ctx context.Context, batchID string) ([]queue.Job, error)
	GetBatch(ctx context.Context, id string) (queue.Batch, bool, error)
}

// Note is a progress line for the job's own log; nil is fine.
type Note func(string)

func (n Note) say(format string, args ...any) {
	if n != nil {
		n(fmt.Sprintf(format, args...))
	}
}

// RunSplit cuts a staged VCF into chunks, stores them, and queues one
// annotation job for each.
//
// The chunks live under the split job's own prefix rather than each chunk job's,
// so one prefix names everything the batch owns and the storage sweep needs no
// special case for a chunk whose job was collected.
func RunSplit(ctx context.Context, q Queue, job queue.Job, inputPath, jobStorage,
	cgkitBin string, chunkSize int, note Note) (batchID string, chunks int, err error) {

	work, err := os.MkdirTemp("", "vhw-split-")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(work)

	note.say("··· splitting into chunks of %d record(s)", chunkSize)
	paths, err := Split(ctx, cgkitBin, inputPath, SplitBase(work), chunkSize)
	if err != nil {
		return "", 0, err
	}
	note.say("··· split into %d chunk(s)", len(paths))

	prefix := queue.JobPrefix(jobStorage, job.ID)
	batchID, err = q.CreateBatch(ctx, job.ID, prefix)
	if err != nil {
		return "", 0, err
	}

	// Every chunk is stored and queued before the count is written. The count
	// is what lets a chunk's completion complete the batch, so writing it early
	// would let the first chunk to finish decide the batch was done while the
	// rest were still being queued.
	for i, p := range paths {
		uri := prefix + "/" + ChunkName(i+1)
		f, oErr := os.Open(p)
		if oErr != nil {
			return "", 0, oErr
		}
		putErr := blob.PutReader(ctx, uri, f)
		f.Close()
		if putErr != nil {
			return "", 0, fmt.Errorf("store chunk %d: %w", i+1, putErr)
		}

		idx := i
		if _, eErr := q.Enqueue(ctx, queue.NewJob{
			Kind:          queue.KindVCF,
			Snapshot:      job.Snapshot,
			Selection:     job.Selection,
			ClientIP:      job.ClientIP,
			Session:       job.Session,
			UserID:        job.UserID,
			Label:         fmt.Sprintf("%s (chunk %d of %d)", job.Label, i+1, len(paths)),
			MaxConcurrent: 0,
			Origin:        job.Origin,
			InputURI:      uri,
			BatchID:       batchID,
			ChunkIndex:    &idx,
		}); eErr != nil {
			return "", 0, fmt.Errorf("queue chunk %d: %w", i+1, eErr)
		}
	}

	if err := q.SetChunkCount(ctx, batchID, len(paths)); err != nil {
		return "", 0, err
	}
	return batchID, len(paths), nil
}

// RunCollect joins a finished batch's chunks into the answer.
//
// Refuses a batch with a failed chunk. The alternative is a VCF missing a range
// of the genome, which is indistinguishable from one where those variants
// simply had nothing to say — a wrong answer that looks like a right one, and
// the only kind worth failing a job over.
func RunCollect(ctx context.Context, q Queue, batchID, jobStorage string,
	note Note) (string, error) {

	b, ok, err := q.GetBatch(ctx, batchID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no such batch %s", batchID)
	}
	if b.Failed > 0 {
		return "", fmt.Errorf("%d of %d chunk(s) failed; refusing to join a file "+
			"with a gap in it", b.Failed, b.Chunks)
	}

	jobs, err := q.BatchChunks(ctx, batchID)
	if err != nil {
		return "", err
	}
	if len(jobs) != b.Chunks {
		return "", fmt.Errorf("batch says %d chunks, found %d", b.Chunks, len(jobs))
	}

	uris := make([]string, 0, len(jobs))
	for _, j := range jobs {
		uris = append(uris, b.Prefix+"/"+ChunkResultName(*j.ChunkIndex))
	}

	dest := b.Prefix + "/" + queue.ResultName + ".gz"
	note.say("··· joining %d chunk(s) into %s", len(uris), dest)
	if err := Join(ctx, uris, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// ChunkResultName is where a chunk's annotated output is stored.
//
// Beside the chunk it came from, under the batch's prefix, so the whole batch —
// inputs and outputs — is one prefix to list and one to delete.
func ChunkResultName(index int) string {
	return fmt.Sprintf("annotated.%04d.vcf.gz", index+1)
}

// StoreChunkResult writes one chunk's annotated VCF, stripping the header from
// every chunk but the first.
//
// Takes the VCF itself — the submitted chunk with annotations merged onto it —
// not the engine's JSON. Passing the JSON here produced gzipped JSON that the
// join happily concatenated into something no reader would accept, and nothing
// noticed because each piece was individually well-formed.
//
// The stripping happens on the way to storage rather than at the join. Doing it
// at the join would mean reading and rewriting every record, which is the cost
// the byte concatenation exists to avoid.
func StoreChunkResult(ctx context.Context, prefix string, index int, vcfIn io.Reader,
	note Note) (string, error) {

	uri := prefix + "/" + ChunkResultName(index)

	pr, pw := io.Pipe()
	go func() {
		zw := gzip.NewWriter(pw)
		out := bufio.NewWriterSize(zw, 1<<20)
		sc := bufio.NewScanner(vcfIn)
		// A cohort record grows with its sample count; the 64 KB default would
		// report a long one as end of input and truncate the chunk silently.
		sc.Buffer(make([]byte, 0, 256*1024), maxLine)
		n := 0
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			if line[0] == '#' {
				// Only the first chunk's header survives; the rest would land
				// in the middle of the joined file, where no reader expects one.
				if index != 0 {
					continue
				}
			} else {
				n++
			}
			if _, err := out.Write(line); err != nil {
				pw.CloseWithError(err)
				return
			}
			if err := out.WriteByte('\n'); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		if err := sc.Err(); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := out.Flush(); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := zw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		note.say("··· chunk %d stored (%d record(s))", index+1, n)
		pw.Close()
	}()

	if err := blob.PutReader(ctx, uri, pr); err != nil {
		pr.CloseWithError(err)
		return "", err
	}
	return uri, nil
}

// LocalChunkPath is where a chunk is staged for annotation.
func LocalChunkPath(dir string, index int) string {
	return filepath.Join(dir, ChunkName(index+1))
}
