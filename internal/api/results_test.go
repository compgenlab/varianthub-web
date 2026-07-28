package api

import (
	"net/http/httptest"
	"testing"
)

func TestResultQueryPaging(t *testing.T) {
	for _, tc := range []struct {
		name          string
		url           string
		limit, offset int
		sort          string
		desc          bool
		search        string
	}{
		{"defaults", "/r", 100, 0, "", false, ""},
		{"page 3", "/r?page=3&per_page=25", 25, 50, "", false, ""},
		// limit/offset wins when present: scripted consumers page by offset, and
		// mixing the two silently would give whichever the code checked last.
		{"limit wins", "/r?page=5&per_page=25&limit=10&offset=7", 10, 7, "", false, ""},
		{"sort desc", "/r?sort=af&order=DESC", 100, 0, "af", true, ""},
		{"search", "/r?q=BRCA", 100, 0, "", false, "BRCA"},
		{"whitespace sort", "/r?sort=%20af%20", 100, 0, "af", false, ""},
		// Out-of-range values clamp rather than erroring: a per_page of 0 or 10^6
		// is a client bug, not a reason to fail the page. Note the asymmetry —
		// an unparseable value means "unset" and takes the default, while a
		// parseable one is honored as closely as the range allows, so per_page=0
		// gives 1 row rather than the default 100.
		{"per_page clamped high", "/r?per_page=99999", 1000, 0, "", false, ""},
		{"per_page clamped low", "/r?per_page=0", 1, 0, "", false, ""},
		{"per_page unparseable", "/r?per_page=abc", 100, 0, "", false, ""},
		{"page below 1", "/r?page=0&per_page=10", 10, 0, "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := resultQuery(httptest.NewRequest("GET", tc.url, nil))
			if q.Limit != tc.limit {
				t.Errorf("Limit = %d, want %d", q.Limit, tc.limit)
			}
			if q.Offset != tc.offset {
				t.Errorf("Offset = %d, want %d", q.Offset, tc.offset)
			}
			if q.Sort != tc.sort {
				t.Errorf("Sort = %q, want %q", q.Sort, tc.sort)
			}
			if q.Desc != tc.desc {
				t.Errorf("Desc = %v, want %v", q.Desc, tc.desc)
			}
			if q.Search != tc.search {
				t.Errorf("Search = %q, want %q", q.Search, tc.search)
			}
		})
	}
}

// order= is only "desc" or ascending; anything else must not silently reverse.
func TestResultQueryOrderIsExplicit(t *testing.T) {
	for _, raw := range []string{"", "asc", "ASC", "descending", "reverse", "1"} {
		q := resultQuery(httptest.NewRequest("GET", "/r?order="+raw, nil))
		if q.Desc {
			t.Errorf("order=%q set Desc", raw)
		}
	}
	for _, raw := range []string{"desc", "DESC", "Desc"} {
		q := resultQuery(httptest.NewRequest("GET", "/r?order="+raw, nil))
		if !q.Desc {
			t.Errorf("order=%q did not set Desc", raw)
		}
	}
}

func TestMin(t *testing.T) {
	if min(3, 5) != 3 || min(5, 3) != 3 || min(4, 4) != 4 {
		t.Error("min is wrong")
	}
}
