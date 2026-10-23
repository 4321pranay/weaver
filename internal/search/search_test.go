package search

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexScopeAndPrune(t *testing.T) {
	td := t.TempDir()
	idx := filepath.Join(td, "zoekt")
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(td+"/payment/Retry.java", "class Retry { void charge() { retryPayment(3); } }\n")
	write(td+"/payment/.git/config", "retryPayment in git metadata must not be indexed\n")
	write(td+"/checkout/main.go", "package main\nfunc f() { retryPayment(1) }\n")
	for _, r := range []Repo{
		{Name: "payment", Dir: td + "/payment", Domain: "payments", Branch: "release/1.10", Commit: "abc123"},
		{Name: "checkout", Dir: td + "/checkout", Domain: "ordering", Branch: "release/2.0", Commit: "def456"},
	} {
		if err := IndexRepo(idx, r); err != nil {
			t.Fatal(err)
		}
	}

	repos := func(q string) map[string]int {
		hits, err := Search([]string{idx}, q, 50)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int{}
		for _, h := range hits {
			m[h.Repo]++
		}
		return m
	}
	if got := repos("retryPayment"); got["payment"] != 1 || got["checkout"] != 1 || len(got) != 2 {
		t.Errorf("unscoped: %v (the .git file must not match)", got)
	}
	if got := repos("repo:^payment$ retryPayment"); len(got) != 1 || got["payment"] != 1 {
		t.Errorf("repo-scoped: %v", got)
	}
	if got := repos("meta.domain:^ordering$ retryPayment"); len(got) != 1 || got["checkout"] != 1 {
		t.Errorf("domain-scoped: %v", got)
	}
	hits, _ := Search([]string{idx}, "repo:^payment$ retryPayment", 5)
	if len(hits) != 1 || hits[0].File != "Retry.java" || hits[0].Line != 1 {
		t.Errorf("hit: %+v", hits)
	}

	// re-indexing a service replaces its shards instead of duplicating hits
	if err := IndexRepo(idx, Repo{Name: "payment", Dir: td + "/payment", Domain: "payments", Branch: "release/1.10", Commit: "abc123"}); err != nil {
		t.Fatal(err)
	}
	if got := repos("repo:^payment$ retryPayment"); got["payment"] != 1 {
		t.Errorf("after re-index: %v", got)
	}

	// checkout dropped from sources.yaml -> its shards go
	n, err := Prune(idx, map[string]bool{"payment": true})
	if err != nil || n != 1 {
		t.Fatalf("prune removed %d, err %v", n, err)
	}
	if got := repos("retryPayment"); len(got) != 1 || got["payment"] != 1 {
		t.Errorf("after prune: %v", got)
	}
}
