package queue

import "strings"

// Where a submission's files live under the configured job storage.
//
// One implementation, used by the API that writes an input and by the worker
// that reads it. Two would be two spellings of one location, and a chunk whose
// input is written under one and looked for under the other is a chunk whose
// input has vanished — with nothing to see, because both processes would be
// behaving exactly as written.
//
// The layout is jobs/<id>/<name>, so a bucket listing says which submission
// every object belongs to. That is what makes "which objects outlived their
// row" a listing rather than a join against the database, which matters
// precisely when the database no longer has the row.
//
// The id is a chunk id: the one the submitter was given, which for a split
// submission is the split chunk and names the prefix its chunks live under
// too. The "jobs/" segment is left as it is deliberately — it is a path in
// storage, not a name in the code, and renaming it would orphan every object
// already written.

// InputName is the object a submitted VCF is stored as.
//
// The extension records whether it is compressed, decided once when the upload
// is accepted and never guessed at again. A consumer wraps a gzip reader
// because the name told it to, not because it sniffed the bytes — the process
// that received the file is the one that knows, and it should say so rather
// than leave every later reader to work it out.
func InputName(compressed bool) string {
	if compressed {
		return "input.vcf.gz"
	}
	return "input.vcf"
}

// ObjectURI locates one of a submission's files. base is the configured job
// storage: a bucket prefix or a directory.
func ObjectURI(base, id, name string) string {
	return strings.TrimRight(base, "/") + "/jobs/" + id + "/" + name
}

// JobPrefix is everything belonging to one submission — its input, and for a
// split one its chunks and their answers — for deleting it wholesale.
func JobPrefix(base, id string) string {
	return strings.TrimRight(base, "/") + "/jobs/" + id
}

// Compressed reports whether a stored input's name says it is gzipped.
func Compressed(uri string) bool { return strings.HasSuffix(uri, ".gz") }

// ResultName is the object a job's answer-as-a-VCF is stored as.
//
// Gzipped, and gzipped whether the job was split or not. It was not always: a
// split job's collect wrote result.vcf.gz while an unsplit job wrote a plain
// result.vcf under this name, and the export streamed whichever it found
// straight to the caller as text/plain. A split submission's download was
// therefore gzip bytes in a file called .vcf, and nothing said so — the two
// paths had each chosen sensibly and never compared answers.
//
// So the name carries the compression, as the input's does, and one name means
// there is one thing for a reader to be told rather than two for it to guess
// between. Gzip rather than plain because an annotated chromosome is hundreds
// of megabytes and this is the copy that is kept for the job's whole life; the
// export pays a decompression it was already paying for split jobs.
const ResultName = "result.vcf.gz"
