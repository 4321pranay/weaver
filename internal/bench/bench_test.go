package bench

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"weaver/internal/extract"
)

// benchmarks: the gitignored corpora dir; tests skip when it isn't checked out.
func benchmarks(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	d := filepath.Join(filepath.Dir(here), "..", "..", "benchmarks")
	if _, err := os.Stat(d); err != nil {
		t.Skip("benchmarks/ not present (gitignored)")
	}
	return d
}

// The scoreboard. complex's one miss is known (a runtime-only edge); a
// change here is a regression or an improvement — either way, update on purpose.
func TestBenchmarks(t *testing.T) {
	root := benchmarks(t)
	for dir, want := range map[string][2]int{ // {caught, catchable}
		"core":         {7, 7},
		"generated":    {100, 100},
		"generated_1k": {1000, 1000},
		"adversarial":  {17, 17},
		"complex":      {7, 8},
	} {
		r, err := Score(&extract.Extractor{}, filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		if r.Caught != want[0] || r.Catchable != want[1] || r.False != 0 || r.Junk != 0 {
			var b strings.Builder
			r.Print(&b)
			t.Errorf("%s: caught %d/%d false %d junk %d\n%s", dir, r.Caught, r.Catchable, r.False, r.Junk, b.String())
		}
	}
}
