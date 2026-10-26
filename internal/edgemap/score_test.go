package edgemap

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "expected.yaml")
	os.WriteFile(p, []byte("edges:\n  - {from: a, to: b, kind: rest, resource: /x}\n  - {from: a, to: c, kind: rest, resource: /y}\n"), 0o644)
	m := Map{Edges: []Edge{
		{From: "a", To: "b", Kind: "rest", Resource: "/x"},
		{From: "a", To: "d", Kind: "rest", Resource: "/z"},
		{From: "a", To: "unknown/q-1", Kind: "rest", Resource: "{}"},
	}}
	s, err := Score(m, p)
	if err != nil || len(s) != 1 {
		t.Fatal(s, err)
	}
	if r := s[0]; r.Right != 1 || r.Found != 2 || r.Expected != 2 || r.Waiting != 1 || len(r.Missed) != 1 || len(r.Extra) != 1 {
		t.Errorf("%+v", r)
	}
}
