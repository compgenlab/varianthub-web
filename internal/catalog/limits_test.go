package catalog

import (
	"testing"

	"github.com/compgenlab/varianthub-web/internal/config"
)

// A tier with no cap accepts anything; one with a cap accepts up to it.
//
// Zero as "unlimited" matters most for the elevated tier, which exists for
// cohort work: a whole chromosome in one submission is the point of it, and a
// cap that defaulted to some number would silently make that tier useless for
// what it was created to do.
func TestMaxVariantsTreatsZeroAsUnlimited(t *testing.T) {
	unlimited := Limits{MaxVariants: 0}
	for _, n := range []int{1, 10_000, 2_600_000} {
		if !unlimited.AllowsVariants(n) {
			t.Errorf("an uncapped tier refused %d variants", n)
		}
	}

	capped := Limits{MaxVariants: 10_000}
	if !capped.AllowsVariants(10_000) {
		t.Error("the cap itself should be allowed, not just under it")
	}
	if capped.AllowsVariants(10_001) {
		t.Error("one past the cap was allowed")
	}
}

// The chunk size and the submission cap are different questions, and the whole
// reason this setting exists separately is that they were the same number.
//
// Chromosome 22 of a WGS cohort is ~2.6M variants. At the interactive cap that
// is 260 chunks, each paying a claim, a stage from storage, a varhub start and a
// snapshot load. This test fails if someone "tidies up" by pointing one at the
// other.
func TestTheChunkSizeIsNotTheSubmissionCap(t *testing.T) {
	s := SiteFromConfig(config.Defaults())
	if s.ChunkSize() == s.StandardMaxVariants {
		t.Errorf("chunk size and the standard submission cap are both %d; "+
			"they answer different questions and should not share a number",
			s.ChunkSize())
	}
	if s.ChunkSize() != DefaultVCFChunkSize {
		t.Errorf("chunk size = %d, want the configured default %d",
			s.ChunkSize(), DefaultVCFChunkSize)
	}

	// chr22 as the yardstick the size was chosen against.
	const chr22Variants = 2_596_403
	chunks := (chr22Variants + s.ChunkSize() - 1) / s.ChunkSize()
	if chunks > 40 {
		t.Errorf("a chromosome splits into %d chunks; the fixed per-chunk cost "+
			"dominates at that count", chunks)
	}
}

// An unset chunk size resolves to the default rather than to zero, which would
// be a splitter that never advances.
func TestAnUnsetChunkSizeResolvesToTheDefault(t *testing.T) {
	var s Site
	if got := s.ChunkSize(); got != DefaultVCFChunkSize {
		t.Errorf("ChunkSize() = %d on a zero Site, want %d", got, DefaultVCFChunkSize)
	}
	s.VCFChunkSize = 250_000
	if got := s.ChunkSize(); got != 250_000 {
		t.Errorf("ChunkSize() = %d, want the configured %d", got, 250_000)
	}
}

// Each tier resolves to its own cap. A tier reading another's would be invisible
// until someone noticed their limit was not the one they were given.
func TestEachTierResolvesToItsOwnVariantCap(t *testing.T) {
	s := Site{
		AnonMaxVariants:     100,
		StandardMaxVariants: 200,
		ElevatedMaxVariants: 300,
	}
	if got := s.AnonLimits().MaxVariants; got != 100 {
		t.Errorf("anon = %d, want 100", got)
	}
	if got := s.LimitsFor(TierStandard).MaxVariants; got != 200 {
		t.Errorf("standard = %d, want 200", got)
	}
	if got := s.LimitsFor(TierElevated).MaxVariants; got != 300 {
		t.Errorf("elevated = %d, want 300", got)
	}
	// An unrecognized tier gets standard, never the most generous — a typo in a
	// hand-edited row must not promote an account.
	if got := s.LimitsFor("platinum").MaxVariants; got != 200 {
		t.Errorf("an unknown tier resolved to %d, want the standard 200", got)
	}
	// The unlimited tier is uncapped in every dimension, this one included.
	if !s.LimitsFor(TierUnlimited).Unlimited() {
		t.Error("the unlimited tier reported a limit")
	}
}
