package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// loci builds n submittable variants.
func loci(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("chr1-%d-A-T", i+1)
	}
	return out
}

// capHarness is a server with a real queue whose variant cap is n for every
// tier, so the test does not depend on which tier the credential resolves to —
// that is a different question, covered in the catalog package.
func capHarness(t *testing.T, n int) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.withQueue(t)
	h.server.cfg.AnonMaxVariants = n
	h.server.cfg.StandardMaxVariants = n
	h.server.cfg.ElevatedMaxVariants = n
	h.http = h.server.Routes()
	_, token := h.admin(t)
	return h, token
}

// The cap comes from the caller's tier, not from a number compiled in.
//
// It used to be a const, so an installation could not raise it for anyone and
// the elevated tier — the one that exists for cohort work — was held to the
// same 10,000 as an anonymous visitor.
func TestTheVariantCapFollowsTheTier(t *testing.T) {
	h, token := capHarness(t, 5)

	// Exactly at the cap is allowed: the rejection is for exceeding it, not for
	// reaching it. An off-by-one here means a caller told they may submit N
	// finds that they may not.
	w := h.do("POST", "/api/v1/annotate", token, map[string]any{
		"snapshot": "s", "variants": loci(5),
	})
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("exactly at the cap was refused: %s", w.Body)
	}

	// One past: refused, and the message names the limit that applied rather
	// than a number the caller has no way to check.
	w = h.do("POST", "/api/v1/annotate", token, map[string]any{
		"snapshot": "s", "variants": loci(6),
	})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", w.Code, w.Body)
	}
	if body := w.Body.String(); !strings.Contains(body, "5-variant") {
		t.Errorf("the error should name the limit that applied: %s", body)
	}
}

// A cap of zero is unlimited, which is what the elevated tier ships with. That
// tier exists so a whole chromosome can go in one submission, and a cap that
// quietly defaulted to some number would make it useless for its purpose.
func TestAZeroVariantCapAcceptsALargeSubmission(t *testing.T) {
	h, token := capHarness(t, 0)

	w := h.do("POST", "/api/v1/annotate", token, map[string]any{
		"snapshot": "s", "variants": loci(50_000),
	})
	if w.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("an uncapped tier refused 50,000 variants: %s", w.Body)
	}
}

// The cap a job was admitted under is recorded on it, so the worker enforcing
// it later is judging the job by the terms it was accepted on — not by whatever
// the setting says by the time it runs.
func TestTheAdmittedCapIsStampedOnTheJob(t *testing.T) {
	h, token := capHarness(t, 5000)

	w := h.do("POST", "/api/v1/annotate", token, map[string]any{
		"snapshot": "s", "variants": loci(3),
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(ctx, `SELECT max_variants FROM chunk LIMIT 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5000 {
		t.Errorf("the job records a cap of %d, want the 5000 it was admitted under", n)
	}
}
