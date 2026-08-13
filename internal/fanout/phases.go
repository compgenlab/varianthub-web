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

// Queue is what the phases need from the queue.
//
// An interface so the phases can be tested without a database. They are the
// part of this that decides how a submission becomes many chunks, and that is
// worth being able to check directly.
type Queue interface {
	SetPrefix(ctx context.Context, jobID, prefix string) error
	Enqueue(ctx context.Context, j queue.NewChunk) (string, error)
	SplitChunks(ctx context.Context, jobID string) ([]queue.Chunk, error)
	GetJob(ctx context.Context, id string) (queue.Job, bool, error)
}

// Note is a progress line for the chunk's own log; nil is fine.
type Note func(string)

func (n Note) say(format string, args ...any) {
	if n != nil {
		n(fmt.Sprintf(format, args...))
	}
}

// RunSplit cuts a staged VCF into pieces, stores them, and queues one
// annotation chunk for each.
//
// The pieces live under the job's prefix rather than each chunk's, so one
// prefix names everything the submission owns and the storage sweep needs no
// special case for a piece whose chunk was collected.
func RunSplit(ctx context.Context, q Queue, chunk queue.Chunk, inputPath, jobStorage,
	cgkitBin string, chunkSize int, note Note) (chunks int, err error) {

	work, err := os.MkdirTemp("", "vhw-split-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(work)

	note.say("··· splitting into chunks of %d record(s)", chunkSize)
	paths, err := Split(ctx, cgkitBin, inputPath, SplitBase(work), chunkSize)
	if err != nil {
		return 0, err
	}
	note.say("··· split into %d chunk(s)", len(paths))

	jobID := chunk.JobID
	prefix := queue.JobPrefix(jobStorage, jobID)
	if err := q.SetPrefix(ctx, jobID, prefix); err != nil {
		return 0, err
	}

	for i, p := range paths {
		uri := prefix + "/" + ChunkName(i+1)
		f, oErr := os.Open(p)
		if oErr != nil {
			return 0, oErr
		}
		putErr := blob.PutReader(ctx, uri, f)
		f.Close()
		if putErr != nil {
			return 0, fmt.Errorf("store chunk %d: %w", i+1, putErr)
		}

		idx := i
		if _, eErr := q.Enqueue(ctx, queue.NewChunk{
			Kind:          queue.KindVCF,
			Snapshot:      chunk.Snapshot,
			Selection:     chunk.Selection,
			ClientIP:      chunk.ClientIP,
			Session:       chunk.Session,
			UserID:        chunk.UserID,
			Label:         fmt.Sprintf("%s (chunk %d of %d)", chunk.Label, i+1, len(paths)),
			MaxConcurrent: 0,
			Origin:        chunk.Origin,
			InputURI:      uri,
			JobID:         jobID,
			ChunkIndex:    &idx,
		}); eErr != nil {
			return 0, fmt.Errorf("queue chunk %d: %w", i+1, eErr)
		}
	}

	// The join, queued here rather than by whichever piece finishes last.
	//
	// It waits for them — see queue.Chunk.AwaitsPieces — so a worker cannot
	// start it early, and exactly one worker starts it in the end because
	// exactly one worker claims anything. Queued last so it never waits on a
	// set of pieces that is still being written.
	//
	// Doing it here is what makes a job's state readable from its chunks: the
	// work still owed is a row from the moment there is any, instead of an
	// intention held by whichever worker gets there.
	if _, err := q.Enqueue(ctx, queue.NewChunk{
		Kind:         queue.KindCollect,
		Snapshot:     chunk.Snapshot,
		Selection:    chunk.Selection,
		ClientIP:     chunk.ClientIP,
		Session:      chunk.Session,
		UserID:       chunk.UserID,
		Label:        fmt.Sprintf("%s (joining %d chunks)", chunk.Label, len(paths)),
		Origin:       chunk.Origin,
		JobID:        jobID,
		AwaitsPieces: true,
		CompletesJob: true,
	}); err != nil {
		return 0, fmt.Errorf("queue the join: %w", err)
	}
	return len(paths), nil
}

// RunCollect joins a finished job's chunks into the answer.
//
// Refuses a job with a failed chunk. The alternative is a VCF missing a range
// of the genome, which is indistinguishable from one where those variants
// simply had nothing to say — a wrong answer that looks like a right one, and
// the only kind worth failing a submission over.
func RunCollect(ctx context.Context, q Queue, jobID, jobStorage string,
	note Note) (string, error) {

	b, ok, err := q.GetJob(ctx, jobID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no such job %s", jobID)
	}
	if b.Failed > 0 {
		return "", fmt.Errorf("%d of %d chunk(s) failed; refusing to join a file "+
			"with a gap in it", b.Failed, b.Chunks)
	}

	chunks, err := q.SplitChunks(ctx, jobID)
	if err != nil {
		return "", err
	}
	if len(chunks) == 0 {
		return "", fmt.Errorf("job %s has no pieces to join", jobID)
	}

	uris := make([]string, 0, len(chunks))
	for _, c := range chunks {
		uris = append(uris, b.Prefix+"/"+ChunkResultName(*c.ChunkIndex))
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
// Beside the chunk it came from, under the job's prefix, so the whole job —
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
