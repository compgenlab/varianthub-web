package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-web/internal/queue"
)

// Deleting a job destroys its payload and leaves the record — the account of
// what this installation has run is not a user-editable list. See 0038.

// seedOwnedJob is seedJob with an owner, which is what these need: every
// assertion here is about who may act on a job, and seedJob's rows belong to
// nobody in particular.
func seedOwnedJob(t *testing.T, h *harness, id, userID, session string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	chunk := chunkOf(id)
	if _, err := pool.Exec(ctx, `
		INSERT INTO job (id,kind,snapshot,selection,client_ip,session_id,user_id,
		                 created_at,input_chunk_id)
		VALUES ($1,'locus','s','','1.1.1.1',$2,$3,1,$4)`,
		id, session, userID, chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO chunk (id,kind,snapshot,selection,status,client_ip,session_id,
		                   user_id,created_at,started_at,finished_at,columns,job_id,
		                   chunk_index,completes_job,n_variants,runner)
		VALUES ($1,'locus','s','','done','1.1.1.1',$2,$3,1,1,2,$4,$5,0,TRUE,7,'local')`,
		chunk, session, userID, vcfCols, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO chunk_variant (chunk_id,idx,chrom,pos,ref,alt,annotations)
		VALUES ($1,0,'chr1',100,'A','G','{"GENE":"TP53"}')`, chunk); err != nil {
		t.Fatal(err)
	}
}

func TestDeletingAJobKeepsTheRecordAndDropsThePayload(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	u, _ := h.admin(t)
	sess := h.sessionFor(t, u.ID)
	seedOwnedJob(t, h, "mine", u.ID, "")

	if w := h.doSession("DELETE", "/api/v1/jobs/mine", sess, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body.String())
	}

	j, ok, err := h.server.queue.GetJob(context.Background(), "mine")
	if err != nil || !ok {
		t.Fatalf("the record went with the payload: ok=%v err=%v", ok, err)
	}
	if j.DeletedAt == 0 {
		t.Error("the job is not marked as left the caller's list")
	}
	if j.PurgedAt == 0 {
		t.Error("the payload was not purged")
	}
	// The summary is what the record is for.
	if j.Status != "done" || j.NVariants != 7 || j.Runner != "local" {
		t.Errorf("the summary did not survive: status=%q n=%d runner=%q",
			j.Status, j.NVariants, j.Runner)
	}
	if j.UserID != u.ID || j.Snapshot == "" || j.CreatedAt == 0 {
		t.Errorf("the record lost its attribution: user=%q snapshot=%q created=%d",
			j.UserID, j.Snapshot, j.CreatedAt)
	}

	// And it is gone from the list the caller sees.
	list, err := h.server.queue.ListJobs(context.Background(),
		queue.JobFilter{UserID: u.ID}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range list {
		if l.ID == "mine" {
			t.Error("a deleted job is still in the caller's listing")
		}
	}
}

// An anonymous caller is identified by a session that outlives nothing, so a
// delete button there would promise control over data that becomes unreachable
// on its own — and hand an unauthenticated caller a way to make rows leave a
// list. Anonymous work ages out on the sweeper's schedule instead.
//
// Anonymous annotation is switched on here deliberately. With it off, requireAuth
// turns the request away at the door with a 401 and the handler's own rule is
// never consulted — so the test would pass against a guard that did nothing. The
// installation that needs the guard is exactly the one that lets anonymous
// callers submit in the first place.
func TestAnAnonymousCallerCannotDeleteAJob(t *testing.T) {
	h := newHarness(t)
	h.server.cfg.AllowAnonymous = true
	h.withQueue(t)
	sess := h.anon(t)
	seedOwnedJob(t, h, "anon-job", "", sess)

	w := h.doAnon("DELETE", "/api/v1/jobs/anon-job", sess, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("anonymous delete = %d, want 403: %s", w.Code, w.Body.String())
	}
	j, ok, _ := h.server.queue.GetJob(context.Background(), "anon-job")
	if !ok || j.DeletedAt != 0 || j.PurgedAt != 0 {
		t.Error("the job was deleted despite the refusal")
	}
}

// Someone else's job is not deletable and is not confirmed to exist — 404, the
// same answer every other job route gives, so an id cannot be probed.
//
// A member rather than an administrator. trustedCaller is IsAdmin, so an admin
// may act on any job — the existing rule that cancel and every other job route
// already follow. Written with an admin, this passed by exercising that rule
// instead of the ownership one.
func TestDeletingSomebodyElsesJobIsNotOffered(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	mine, _ := h.member(t, "member@example.com")
	seedOwnedJob(t, h, "theirs", "someone-else", "")
	sess := h.sessionFor(t, mine.ID)

	w := h.doSession("DELETE", "/api/v1/jobs/theirs", sess, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete of another caller's job = %d, want 404: %s", w.Code, w.Body.String())
	}
	if j, ok, _ := h.server.queue.GetJob(context.Background(), "theirs"); !ok || j.DeletedAt != 0 {
		t.Error("another caller's job was deleted")
	}
}

// --- retry ---

// failJob marks a seeded job's chunk failed, which is the only state retry acts on.
func failJob(t *testing.T, h *harness, id string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		`UPDATE chunk SET status='error', error='the reference was missing', attempts=3
		  WHERE job_id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

func TestRetryingAFailedJobQueuesItAgain(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	u, _ := h.admin(t)
	sess := h.sessionFor(t, u.ID)
	seedOwnedJob(t, h, "flaky", u.ID, "")
	failJob(t, h, "flaky")

	w := h.doSession("POST", "/api/v1/jobs/flaky/retry", sess, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("retry = %d, want 200: %s", w.Code, w.Body.String())
	}
	j, ok, err := h.server.queue.GetJob(context.Background(), "flaky")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if j.Status != "queued" {
		t.Errorf("status after retry = %q, want queued", j.Status)
	}
	// Attempts are reset, or a chunk that had spent them fails again instantly
	// and the button reads as having done nothing. Read from the row: the
	// counter is not on the API's chunk projection.
	pool, err := pgxpool.New(context.Background(), h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var attempts int
	var chunkErr *string
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts, error FROM chunk WHERE job_id=$1`, "flaky").
		Scan(&attempts, &chunkErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("the chunk kept %d attempts; it would fail without trying", attempts)
	}
	if chunkErr != nil && *chunkErr != "" {
		t.Errorf("the chunk kept its old error %q", *chunkErr)
	}
}

// A job whose payload has gone has no input to run against, so retrying it
// would fail a second time for a reason unrelated to the first.
func TestAPurgedJobCannotBeRetried(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	u, _ := h.admin(t)
	sess := h.sessionFor(t, u.ID)
	seedOwnedJob(t, h, "expired", u.ID, "")
	failJob(t, h, "expired")

	// Sweep it, which is what a week would have done.
	if _, err := h.server.queue.PurgeOlderThan(context.Background(), 1<<62); err != nil {
		t.Fatal(err)
	}

	w := h.doSession("POST", "/api/v1/jobs/expired/retry", sess, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("retry of a purged job = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "expired") {
		t.Errorf("the refusal does not say the input is gone, so the caller "+
			"cannot tell what to do instead: %s", w.Body.String())
	}
}

// A job that succeeded has nothing to retry, and saying so is not the same
// answer as "its input expired" — the two lead the caller somewhere different.
func TestASuccessfulJobIsNotRetryable(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	u, _ := h.admin(t)
	sess := h.sessionFor(t, u.ID)
	seedOwnedJob(t, h, "fine", u.ID, "")

	w := h.doSession("POST", "/api/v1/jobs/fine/retry", sess, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("retry of a done job = %d, want 409: %s", w.Code, w.Body.String())
	}
}

// Retry is web-only: a program holds the request that produced the job and can
// submit it again, and an endpoint invites a client to loop on a job that fails
// the same way every time.
func TestRetryIsNotOnThePublishedAPI(t *testing.T) {
	h := newHarness(t)
	h.withQueue(t)
	u, tok := h.admin(t)
	seedOwnedJob(t, h, "tokenly", u.ID, "")
	failJob(t, h, "tokenly")

	w := h.do("POST", "/api/v1/jobs/tokenly/retry", tok, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("retry with a token = %d, want 404 — web-only routes are absent, "+
			"not forbidden: %s", w.Code, w.Body.String())
	}
}
