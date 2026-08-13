package queue

import "strings"

// Where a job's files live under the configured job storage.
//
// One implementation, used by the API that writes an input and by the worker
// that reads it. Two would be two spellings of one location, and a job whose
// input is written under one and looked for under the other is a job whose
// input has vanished — with nothing to see, because both processes would be
// behaving exactly as written.
//
// The layout is jobs/<job-id>/<name>, so a bucket listing says which job every
// object belongs to. That is what makes "which objects outlived their job" a
// listing rather than a join against the database, which matters precisely when
// the database no longer has the row.

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

// ObjectURI locates one of a job's files. base is the configured job storage: a
// bucket prefix or a directory.
func ObjectURI(base, jobID, name string) string {
	return strings.TrimRight(base, "/") + "/jobs/" + jobID + "/" + name
}

// JobPrefix is everything belonging to one job, for deleting it wholesale.
func JobPrefix(base, jobID string) string {
	return strings.TrimRight(base, "/") + "/jobs/" + jobID
}

// Compressed reports whether a stored input's name says it is gzipped.
func Compressed(uri string) bool { return strings.HasSuffix(uri, ".gz") }
