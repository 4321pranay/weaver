package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every release has its own shard and stays searchable; a query that names no
// branch searches the current release only.
func TestReleasesKept(t *testing.T) {
	td := t.TempDir()
	idx := filepath.Join(td, "zoekt")
	write := func(dir, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Pay.java"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(td, "v1"), "class Pay { void chargeOld() {} }\n")
	write(filepath.Join(td, "v2"), "class Pay { void chargeNew() {} }\n")
	for _, r := range []Repo{
		{Name: "pay", Dir: filepath.Join(td, "v1"), Commit: "1111111111111111", Release: "1.0.0"},
		{Name: "pay", Dir: filepath.Join(td, "v2"), Commit: "2222222222222222", Release: "1.1.0"},
	} {
		if err := IndexRepo(idx, r); err != nil {
			t.Fatal(err)
		}
	}
	current := map[string]string{"pay": "1.1.0"}
	if h, err := SearchReleases([]string{idx}, "charge", 10, current); err != nil || len(h) != 1 || !strings.Contains(h[0].Text, "chargeNew") {
		t.Errorf("current release only: %+v %v", h, err)
	}
	if h, err := SearchReleases([]string{idx}, "charge branch:1.0.0", 10, current); err != nil || len(h) != 1 || !strings.Contains(h[0].Text, "chargeOld") {
		t.Errorf("an old release stays searchable by branch: %+v %v", h, err)
	}
}
