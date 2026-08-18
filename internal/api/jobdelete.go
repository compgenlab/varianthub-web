package api

import (
	"errors"
	"log"
	"net/http"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Finishing with a job, and asking for another go at one that failed.
//
// The two are opposite ends of the same fact — a job's payload is separable
// from its record — so they sit together. Deleting destroys the payload early;
// retrying needs it to still be there.

// handleDeleteJob removes a job from the caller's list and destroys its payload.
//
// Signed-in callers only, and that is not a permission so much as a
// consequence. An anonymous visitor is identified by a session cookie that
// outlives nothing: the browser is closed, the session is gone, and with it any
// claim on the jobs submitted under it. Offering a delete button there promises
// control over data that will already have become unreachable by other means,
// and hands an unauthenticated caller a way to make rows disappear from a list
// they cannot otherwise be held to. Anonymous work ages out on the sweeper's
// schedule, which is the same guarantee without the false one attached.
//
// What is destroyed is the payload — the submitted VCF, the built result, the
// rows behind the results table. What is kept is the record. See migration 0038.
func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.ownedJob(w, r)
	if !ok {
		return
	}
	c := callerOf(r)
	if c.Anonymous() {
		writeError(w, http.StatusForbidden,
			"deleting a job needs an account; anonymous work is removed on the "+
				"installation's own schedule")
		return
	}

	switch err := s.queue.DeleteJob(r.Context(), job.ID); {
	case errors.Is(err, queue.ErrNotDeletable):
		// A worker is holding it. Emptying it now leaves that worker writing a
		// result whose input has gone, and it finds out by failing in a way that
		// reads as a storage fault rather than as somebody pressing delete.
		writeError(w, http.StatusConflict,
			"cancel the job before deleting it; it is still running")
		return
	case errors.Is(err, queue.ErrNoSuchJob):
		writeError(w, http.StatusNotFound, "no such job")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("api: job %s deleted by %s; the record is kept", job.ID, c.Label())
	w.WriteHeader(http.StatusNoContent)
}

// handleRetryJob queues a failed job's failed chunks again.
//
// Web only, by Marcus's decision. A program that wants another attempt has the
// request that produced this one and can submit it again; what it cannot do is
// notice that the cause was transient, and a retry endpoint on the published
// API mostly invites a client to loop on a job that will fail the same way.
// Someone looking at a screen has already made that judgement.
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.ownedJob(w, r)
	if !ok {
		return
	}
	out, err := s.queue.RetryJob(r.Context(), job.ID)
	switch {
	case errors.Is(err, queue.ErrNotRetryable):
		// Says which of the two it is, because the difference decides what the
		// caller can do next: a job that did not fail needs nothing, and one
		// whose input has expired can only be submitted again.
		detail := "only a failed job can be retried"
		if job.Purged() {
			detail = "this job's input has expired, so it cannot be run again; submit it afresh"
		}
		writeError(w, http.StatusConflict, detail)
		return
	case errors.Is(err, queue.ErrNoSuchJob):
		writeError(w, http.StatusNotFound, "no such job")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("api: job %s retried by %s", job.ID, callerOf(r).Label())
	writeJSON(w, http.StatusOK, jobStatus(out))
}
