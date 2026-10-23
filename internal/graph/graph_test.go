package graph

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"weaver/internal/extract"
)

// A service that produces AND consumes one channel is internal: detected by
// ResolveInternal, kept out of the cross-service graph.
func TestInternalKeptSeparate(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(root+"/billing/Billing.java", `package com.acme.billing;
@RestController public class Billing {
  RestTemplate rest = new RestTemplate();
  KafkaTemplate<String,String> kafka;
  @PostMapping("/v1/settle") public void settle() {}
  void retry() { rest.postForObject("http://billing/v1/settle", null, String.class); }
  @KafkaListener(topics = "audit.local") void onAudit() {}
  void emit() { kafka.send("audit.local", "x"); }
  void charge() { rest.postForObject("http://ledger/v1/post", null, String.class); }
}
`)
	write(root+"/ledger/Ledger.java", `@RestController public class Ledger {
  @PostMapping("/v1/post") public void post() {}
}
`)
	raw := (&extract.Extractor{}).ExtractAll(root)
	cross, internal := Resolve(raw), ResolveInternal(raw)

	for _, want := range []Internal{{"billing", "rest", "/v1/settle"}, {"billing", "kafka", "audit.local"}} {
		if !slices.Contains(internal, want) {
			t.Errorf("internal edge %v not detected: %v", want, internal)
		}
	}
	for _, h := range cross {
		if h.Src == h.Dst {
			t.Errorf("internal edge leaked into cross-service graph: %v", h)
		}
	}
	if !slices.Contains(cross, Hop{Src: "billing", Dst: "ledger", Kind: "rest", Channel: "/v1/post"}) {
		t.Errorf("cross edge missing: %v", cross)
	}
	if slices.Contains(internal, Internal{"billing", "rest", "/v1/post"}) {
		t.Errorf("cross edge counted as internal: %v", internal)
	}
}

func TestToGraphInfraDedupe(t *testing.T) {
	hops := []Hop{
		{Src: "a", Dst: "b", Kind: "kafka", Channel: "t"},
		{Src: "a", Dst: "c", Kind: "kafka", Channel: "t"},
		{Src: "a", Dst: "b", Kind: "rest", Channel: "/x"},
	}
	g := ToGraph(hops, true, map[string]string{"a": "pay"}, nil)
	// a->kafka:t once, kafka:t->b, kafka:t->c, a->b rest
	if len(g.Edges) != 4 {
		t.Errorf("edges: %+v", g.Edges)
	}
	if g.Nodes[0].ID != "a" || g.Nodes[0].Domain != "pay" || len(g.Nodes) != 4 {
		t.Errorf("nodes: %+v", g.Nodes)
	}
	if g.Edges[len(g.Edges)-1].Signature != "GET /x" {
		t.Errorf("rest signature: %+v", g.Edges[len(g.Edges)-1])
	}
}
