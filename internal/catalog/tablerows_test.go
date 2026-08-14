package catalog

import "testing"

// The table-row cap is the smaller of the configured cap and one chunk.
//
// Taking the minimum is what keeps the rule local: a job's first chunk applies
// it knowing only its own output, so twenty-six workers finishing at once need
// no shared counter and cannot race past the limit. A cap larger than a chunk
// would be a number the first chunk could not reach on its own, which is a
// promise the code does not keep.
func TestTableRowsIsTheSmallerOfTheCapAndAChunk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		site  Site
		want  int
	}{
		{"defaults", Site{}, DefaultMaxTableRows},
		{"a chunk smaller than the cap bounds it",
			Site{VCFChunkSize: 500, MaxTableRows: 10_000}, 500},
		{"a cap smaller than the chunk bounds it",
			Site{VCFChunkSize: 100_000, MaxTableRows: 2_000}, 2_000},
		{"an unset cap falls back to the default",
			Site{VCFChunkSize: 100_000}, DefaultMaxTableRows},
		{"an unset chunk size falls back to its own default",
			Site{MaxTableRows: 1_000_000}, DefaultVCFChunkSize},
	} {
		if got := tc.site.TableRows(); got != tc.want {
			t.Errorf("%s: TableRows() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The default keeps a whole genome out of Postgres.
func TestTheDefaultCapIsSmallEnoughToBeWorthHaving(t *testing.T) {
	if got := (Site{}).TableRows(); got > 10_000 {
		t.Errorf("the default cap is %d rows; the point is to keep a chromosome's "+
			"2.6M variants out of the results table", got)
	}
}

// A new setting reaches every list it has to.
//
// The keys, the override map and the parser are three parallel lists, and this
// file's own comment says why that matters: a key added to one and not the
// others is silently ignored — the form saves it, the reader never looks, and
// the setting appears not to work.
func TestTheRowCapIsWiredThroughEverySettingList(t *testing.T) {
	var listed bool
	for _, k := range SettingKeys {
		if k == KeyMaxTableRows {
			listed = true
		}
	}
	if !listed {
		t.Error("max_table_rows is not in SettingKeys, so the settings form will not offer it")
	}
	if _, ok := (Site{MaxTableRows: 4321}).Values()[KeyMaxTableRows]; !ok {
		t.Error("Values() does not render max_table_rows, so a stored override cannot round-trip")
	}
	var s Site
	if err := (&s).ApplySetting(KeyMaxTableRows, "4321"); err != nil {
		t.Fatalf("ApplySetting: %v", err)
	}
	if s.MaxTableRows != 4321 {
		t.Errorf("MaxTableRows = %d after applying the override, want 4321", s.MaxTableRows)
	}
}
