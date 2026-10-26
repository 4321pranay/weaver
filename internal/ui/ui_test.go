package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"weaver/internal/edgemap"
)

func TestWrite(t *testing.T) {
	hostile := "</script><script>alert(1)</script>"
	m := edgemap.Map{Version: edgemap.FormatEdges,
		Nodes: []edgemap.Node{
			{ID: "pay", Type: "service", Props: map[string]any{"domain": "payments"}},
			{ID: "ord", Type: "service", Props: map[string]any{"domain": "orders"}},
			{ID: "database/ledger", Type: "database"},
		},
		Edges: []edgemap.Edge{
			{From: "pay", To: "ord", Kind: "rest", Resource: hostile, Call: "optional"},
			{From: "pay", To: "database/ledger", Kind: "db", Resource: "entries", Op: "write", Call: "required"},
		},
	}
	p := filepath.Join(t.TempDir(), "graph.html")
	if err := Write(p, "payments", m); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if strings.Contains(s, "WEAVER:") || strings.Contains(s, "{{") {
		t.Error("placeholder left in the page")
	}
	if strings.Count(s, "</script>") != 1 { // only the page's own closing tag
		t.Errorf("a repo string closed the script block: %d </script>", strings.Count(s, "</script>"))
	}
	if !strings.Contains(s, "<title>payments · Service Map</title>") {
		t.Error("title")
	}
	get := func(name string, v any) {
		mm := regexp.MustCompile(`(?m)^const ` + name + ` = (.*);$`).FindStringSubmatch(s)
		if mm == nil {
			t.Fatalf("%s not embedded", name)
		}
		if err := json.Unmarshal([]byte(mm[1]), v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var data struct {
		Nodes []struct{ ID, Type, Domain, Label string }
		Edges []struct{ Src, Dst, Kind, Channel, Call string }
	}
	var domains []string
	get("DATA", &data)
	get("DOMAINS", &domains)
	// the page draws exactly edges.yaml: same edges, with call; spec node types mapped to page types
	if len(data.Edges) != 2 || data.Edges[0].Channel != hostile || data.Edges[0].Call != "optional" {
		t.Errorf("edges: %+v", data.Edges)
	}
	if n := data.Nodes[2]; n.Type != "db" || n.Label != "ledger" {
		t.Errorf("database node: %+v", n)
	}
	if strings.Join(domains, ",") != "orders,payments" {
		t.Errorf("domains: %v", domains)
	}
}
