package catalog

import (
	"context"
	"fmt"
)

// UsageWindows are the periods the summary reports over, in days.
//
// Three rather than one because the question is usually about a trend: "is this
// being used" and "is it being used more than last month" are different, and a
// single number answers neither.
var UsageWindows = []int{7, 30, 90}

// Split counts the same thing three ways: from the web app, from the REST API,
// and from before either was recorded.
//
// Unknown is not a rounding error to be folded into one of the others. Jobs
// predating the origin column genuinely do not say how they arrived, and
// attributing them would put a number on a distinction nobody captured. It goes
// to zero on its own as those jobs age out of the windows.
type Split struct {
	Web     int64 `json:"web"`
	API     int64 `json:"api"`
	Unknown int64 `json:"unknown"`
	Total   int64 `json:"total"`
}

func (s *Split) add(origin string, n int64) {
	switch origin {
	case "web":
		s.Web += n
	case "api":
		s.API += n
	default:
		s.Unknown += n
	}
	s.Total += n
}

// WindowUsage is what happened over one period.
type WindowUsage struct {
	Days int `json:"days"`
	// Jobs submitted in the window, however they ended.
	Jobs Split `json:"jobs"`
	// Variants annotated: the sum of each job's variant count. What the service
	// actually did, as opposed to how often it was asked.
	Variants Split `json:"variants"`
	// Accounts that submitted at least one job. Anonymous work has no account
	// and is counted in Anonymous instead, so the two do not overlap.
	Accounts int64 `json:"accounts"`
	// Anonymous sessions that submitted at least one job.
	Anonymous int64 `json:"anonymous"`
}

// UserUsage is one account's activity over the longest window.
type UserUsage struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Tier     string `json:"tier"`
	Jobs     Split  `json:"jobs"`
	Variants Split  `json:"variants"`
	// LastSubmitted is Unix seconds, 0 for an account that never has.
	LastSubmitted int64 `json:"last_submitted"`
}

// Usage is the whole summary.
type Usage struct {
	// Accounts is every account, active or not — the denominator the windows
	// are read against.
	Accounts int64 `json:"accounts"`
	// Disabled accounts, which still exist and still own their old jobs.
	Disabled int64         `json:"disabled"`
	Windows  []WindowUsage `json:"windows"`
	Users    []UserUsage   `json:"users"`
}

// Usage summarizes what the installation has been asked to do.
//
// Counted from the job table rather than from a rollup, because the numbers are
// small enough to count and a rollup is a second source of truth that can be
// wrong. Revisit when a window scan stops being cheap; the indexes on
// (user_id, created_at) and (created_at) are what keep it so.
func (s *Store) Usage(ctx context.Context, now int64) (Usage, error) {
	var out Usage
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE disabled)
		  FROM app_user`).Scan(&out.Accounts, &out.Disabled); err != nil {
		return Usage{}, fmt.Errorf("usage: accounts: %w", err)
	}

	for _, days := range UsageWindows {
		w, err := s.windowUsage(ctx, now-int64(days)*86400)
		if err != nil {
			return Usage{}, err
		}
		w.Days = days
		out.Windows = append(out.Windows, w)
	}

	longest := UsageWindows[len(UsageWindows)-1]
	users, err := s.userUsage(ctx, now-int64(longest)*86400)
	if err != nil {
		return Usage{}, err
	}
	out.Users = users
	return out, nil
}

// windowUsage counts one period.
//
// Jobs, not chunks: what someone asked for is one submission, however many
// pieces it was cut into. Counting chunks would report a chromosome as
// twenty-six requests and make one user look like a department.
//
// Only annotation work: a provisioning download is the deployment's own doing
// and would swamp a per-user reading of what people asked for. A submitted
// VCF is kind "vcf" on the job whatever its first chunk does.
func (s *Store) windowUsage(ctx context.Context, since int64) (WindowUsage, error) {
	var w WindowUsage
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(origin,''), count(*), COALESCE(sum(n_variants),0)
		  FROM job_state
		 WHERE created_at >= $1 AND kind IN ('locus','vcf')
		 GROUP BY COALESCE(origin,'')`, since)
	if err != nil {
		return WindowUsage{}, fmt.Errorf("usage: jobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var origin string
		var jobs, variants int64
		if err := rows.Scan(&origin, &jobs, &variants); err != nil {
			return WindowUsage{}, fmt.Errorf("usage: jobs: %w", err)
		}
		w.Jobs.add(origin, jobs)
		w.Variants.add(origin, variants)
	}
	if err := rows.Err(); err != nil {
		return WindowUsage{}, fmt.Errorf("usage: jobs: %w", err)
	}

	// Distinct submitters, by account where there is one and by session where
	// there is not. Counted separately rather than summed into one "users"
	// figure, which would imply an anonymous session is a person — it is one
	// browser, and the same person on two machines is two.
	if err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT user_id) FILTER (WHERE COALESCE(user_id,'') <> ''),
		       count(DISTINCT session_id) FILTER (WHERE COALESCE(user_id,'') = ''
		                                            AND COALESCE(session_id,'') <> '')
		  FROM job_state
		 WHERE created_at >= $1 AND kind IN ('locus','vcf')`,
		since).Scan(&w.Accounts, &w.Anonymous); err != nil {
		return WindowUsage{}, fmt.Errorf("usage: submitters: %w", err)
	}
	return w, nil
}

// userUsage is the per-account breakdown.
//
// A LEFT JOIN from app_user, so an account that has never submitted appears
// with zeroes. "Who is not using this" is as much of an answer as who is, and
// an absent row reads as an account that does not exist.
func (s *Store) userUsage(ctx context.Context, since int64) ([]UserUsage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.tier, COALESCE(j.origin,''),
		       count(j.id), COALESCE(sum(j.n_variants),0), COALESCE(max(j.created_at),0)
		  FROM app_user u
		  LEFT JOIN job_state j
		    ON j.user_id = u.id AND j.created_at >= $1 AND j.kind IN ('locus','vcf')
		 GROUP BY u.id, u.email, u.tier, COALESCE(j.origin,'')
		 ORDER BY lower(u.email)`, since)
	if err != nil {
		return nil, fmt.Errorf("usage: per user: %w", err)
	}
	defer rows.Close()

	// One row per (user, origin), folded back into one entry per user.
	byID := map[string]*UserUsage{}
	var order []string
	for rows.Next() {
		var id, email, tier, origin string
		var jobs, variants, last int64
		if err := rows.Scan(&id, &email, &tier, &origin, &jobs, &variants, &last); err != nil {
			return nil, fmt.Errorf("usage: per user: %w", err)
		}
		u, ok := byID[id]
		if !ok {
			u = &UserUsage{UserID: id, Email: email, Tier: tier}
			byID[id] = u
			order = append(order, id)
		}
		// The LEFT JOIN's miss is a real row with a null job, which count()
		// already reports as 0 — adding it would invent an "unknown" origin for
		// an account that has submitted nothing.
		if jobs > 0 {
			u.Jobs.add(origin, jobs)
			u.Variants.add(origin, variants)
		}
		if last > u.LastSubmitted {
			u.LastSubmitted = last
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: per user: %w", err)
	}

	out := make([]UserUsage, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}
