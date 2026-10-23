package snapshot

import (
	"path/filepath"
	"testing"

	"weaver/internal/graph"
)

func TestCompare(t *testing.T) {
	e := func(src, dst, kind, ch string) graph.Hop {
		return graph.Hop{Src: src, Dst: dst, Kind: kind, Channel: ch, SrcRepo: src, DstRepo: dst}
	}
	v1 := Snapshot{Version: 1, TS: "t1", Commits: map[string]string{"a": "c1", "b": "x1", "e": "e1"},
		Hashes: map[string]string{"a": "ha1", "b": "hb1", "e": "he1"},
		Edges:  []graph.Hop{e("a", "b", "rest", "/v1/pay"), e("a", "c", "kafka", "old.topic")}}
	v2 := Snapshot{Version: 2, TS: "t2", Commits: map[string]string{"a": "c2", "b": "x2", "e": "e1"},
		Hashes: map[string]string{"a": "ha2", "b": "hb2", "e": "he1"}, // b changed internally
		Edges:  []graph.Hop{e("a", "b", "rest", "/v1/charge"), e("a", "d", "grpc", "New")}}

	d := Compare(v1, v2)
	if len(d.Added) != 1 || d.Added[0].Dst != "d" {
		t.Errorf("added: %+v", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0].Channel != "old.topic" {
		t.Errorf("removed: %+v", d.Removed)
	}
	if len(d.Changed) != 1 || d.Changed[0].FromChannel != "/v1/pay" || d.Changed[0].ToChannel != "/v1/charge" {
		t.Fatalf("changed: %+v", d.Changed)
	}
	if c := d.Changed[0].Cause; c.Repo == nil || *c.Repo != "a" || c.From != "c1" {
		t.Errorf("cause: %+v", c)
	}
	// b's hash moved but b sits on a changed edge -> not internal
	if len(d.Internal) != 0 {
		t.Errorf("internal: %+v", d.Internal)
	}
	// e changes with no edge touching it -> internal
	v3 := v2
	v3.Hashes = map[string]string{"a": "ha2", "b": "hb2", "e": "he2"}
	d2 := Compare(v1, v3)
	if len(d2.Internal) != 1 || d2.Internal[0].Service != "e" || d2.Internal[0].FromHash != "he1" || d2.Internal[0].ToHash != "he2" {
		t.Errorf("internal: %+v", d2.Internal)
	}
}

func TestAppendLoad(t *testing.T) {
	log := filepath.Join(t.TempDir(), "v.jsonl")
	for i := int64(1); i <= 2; i++ {
		if err := Append(log, Snapshot{Version: i, Edges: []graph.Hop{{Src: "a", Dst: "b", Kind: "rest", Channel: "/x"}}}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := LoadAll(log)
	if err != nil || len(all) != 2 || all[1].Version != 2 || all[1].Edges[0].Dst != "b" {
		t.Errorf("load: %+v %v", all, err)
	}
}
