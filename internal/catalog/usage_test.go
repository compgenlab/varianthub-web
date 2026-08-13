package catalog

import (
	"context"
	"fmt"
	"testing"
)

// now is a fixed clock, so a window boundary is exact rather than "about a week
// ago" and a test cannot fail at midnight.
const usageNow int64 = 1_800_000_000

func daysAgo(n int64) int64 { return usageNow - n*86400 }

// insertJob writes a job row directly. The queue is the thing that normally
// does this and lives in another package; what is under test here is the
// counting, so the rows are made rather than earned.
var jobSeq int

func insertJob(t *testing.T, s *Store, kind, userID, session, origin string, variants, created int64) {
	t.Helper()
	jobSeq++
	id := fmt.Sprintf("job-%d", jobSeq)
	ctx := context.Background()
	// A job and its chunk. Usage counts submissions, and how a submission went
	// is read from its chunks — see the job_state view — so a seeded job
	// without one has no status to count.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO job (id,kind,snapshot,selection,client_ip,session_id,user_id,
		                 label,origin,created_at,input_chunk_id)
		VALUES ($1,$2,'snap','','10.0.0.1',$3,NULLIF($4,''),'',$5,$6,$7)`,
		id, kind, session, userID, origin, created, id+"-c0"); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO chunk (id,kind,snapshot,selection,status,client_ip,session_id,user_id,
		                   label,weight,origin,n_variants,created_at,started_at,
		                   finished_at,job_id,completes_job)
		VALUES ($1,$2,'snap','','done','10.0.0.1',$3,NULLIF($4,''),'',1,$5,$6,$7,$7,$7,$8,TRUE)`,
		id+"-c0", kind, session, userID, origin, variants, created, id); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
}

func usageFixture(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()

	for _, u := range []struct{ id, email string }{
		{"u1", "ann@example.org"},
		{"u2", "bob@example.org"},
		{"u3", "never@example.org"}, // has an account, has never submitted
	} {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO app_user (id,email,role,tier,created_at,updated_at)
			VALUES ($1,$2,'member','standard',$3,$3)`, u.id, u.email, usageNow); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	// A disabled account still exists and still owns its old work.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO app_user (id,email,role,tier,disabled,created_at,updated_at)
		VALUES ('u4','gone@example.org','member','standard',true,$1,$1)`, usageNow); err != nil {
		t.Fatalf("insert disabled user: %v", err)
	}

	insertJob(t, s, "locus", "u1", "", "web", 10, daysAgo(1))
	insertJob(t, s, "vcf", "u1", "", "api", 100, daysAgo(3))
	insertJob(t, s, "locus", "u2", "", "web", 5, daysAgo(20))
	insertJob(t, s, "locus", "u1", "", "web", 7, daysAgo(60))
	// Recorded before origin existed.
	insertJob(t, s, "locus", "u2", "", "", 3, daysAgo(80))
	// Anonymous: no account, a session instead.
	insertJob(t, s, "locus", "", "sess-a", "web", 2, daysAgo(2))
	insertJob(t, s, "locus", "", "sess-a", "web", 2, daysAgo(4))
	insertJob(t, s, "locus", "", "sess-b", "web", 1, daysAgo(5))
	// Provisioning is the deployment's own doing, not somebody's usage.
	insertJob(t, s, "download", "", "", "web", 0, daysAgo(1))
	// Older than every window.
	insertJob(t, s, "locus", "u1", "", "web", 999, daysAgo(200))
	return s
}

func window(t *testing.T, u Usage, days int) WindowUsage {
	t.Helper()
	for _, w := range u.Windows {
		if w.Days == days {
			return w
		}
	}
	t.Fatalf("no %d-day window in %+v", days, u.Windows)
	return WindowUsage{}
}

func TestUsageCountsWindowsAndSplitsByOrigin(t *testing.T) {
	s := usageFixture(t)
	u, err := s.Usage(context.Background(), usageNow)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if u.Accounts != 4 || u.Disabled != 1 {
		t.Errorf("accounts = %d (%d disabled), want 4 and 1", u.Accounts, u.Disabled)
	}

	// 7 days: u1 twice (web+api), and three anonymous jobs across two sessions.
	w := window(t, u, 7)
	if w.Jobs.Total != 5 {
		t.Errorf("7d jobs = %d, want 5 (%+v)", w.Jobs.Total, w.Jobs)
	}
	if w.Jobs.Web != 4 || w.Jobs.API != 1 {
		t.Errorf("7d split = web %d / api %d, want 4 and 1", w.Jobs.Web, w.Jobs.API)
	}
	if w.Variants.Total != 115 {
		t.Errorf("7d variants = %d, want 115", w.Variants.Total)
	}
	if w.Accounts != 1 {
		t.Errorf("7d accounts = %d, want 1", w.Accounts)
	}
	// Two browsers, not two people — counted apart from accounts for that reason.
	if w.Anonymous != 2 {
		t.Errorf("7d anonymous sessions = %d, want 2", w.Anonymous)
	}

	// 90 days picks up the job recorded before origin existed. It must be its
	// own number, not folded into web or api.
	w90 := window(t, u, 90)
	if w90.Jobs.Unknown != 1 {
		t.Errorf("90d unknown-origin jobs = %d, want 1 (%+v)", w90.Jobs.Unknown, w90.Jobs)
	}
	if w90.Jobs.Web+w90.Jobs.API+w90.Jobs.Unknown != w90.Jobs.Total {
		t.Errorf("90d split does not add up: %+v", w90.Jobs)
	}
	// Every annotation job in the fixture except the 200-day-old one, and not
	// the download: five from accounts, three anonymous.
	if w90.Jobs.Total != 8 {
		t.Errorf("90d jobs = %d, want 8 — a download or an expired job was counted", w90.Jobs.Total)
	}
}

func TestUsagePerUserIncludesAccountsThatNeverSubmitted(t *testing.T) {
	s := usageFixture(t)
	u, err := s.Usage(context.Background(), usageNow)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	byEmail := map[string]UserUsage{}
	for _, r := range u.Users {
		byEmail[r.Email] = r
	}
	if len(u.Users) != 4 {
		t.Fatalf("got %d user rows, want 4 — every account should appear", len(u.Users))
	}

	ann := byEmail["ann@example.org"]
	if ann.Jobs.Total != 3 || ann.Jobs.Web != 2 || ann.Jobs.API != 1 {
		t.Errorf("ann jobs = %+v, want 3 total (2 web, 1 api)", ann.Jobs)
	}
	if ann.Variants.Total != 117 {
		t.Errorf("ann variants = %d, want 117", ann.Variants.Total)
	}
	if ann.LastSubmitted != daysAgo(1) {
		t.Errorf("ann last submitted = %d, want %d", ann.LastSubmitted, daysAgo(1))
	}
	if ann.Tier != "standard" {
		t.Errorf("ann tier = %q, want standard", ann.Tier)
	}

	// The point of the LEFT JOIN: who is not using this is an answer too, and an
	// absent row reads as an account that does not exist.
	never, ok := byEmail["never@example.org"]
	if !ok {
		t.Fatal("an account that never submitted is missing from the breakdown")
	}
	if never.Jobs.Total != 0 || never.LastSubmitted != 0 {
		t.Errorf("never = %+v, want zeroes", never)
	}
	// And no phantom origin for it.
	if never.Jobs.Unknown != 0 {
		t.Errorf("an account with no jobs was given %d unknown-origin jobs", never.Jobs.Unknown)
	}
}
