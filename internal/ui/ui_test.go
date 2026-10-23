package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"weaver/internal/graph"
	"weaver/internal/snapshot"
)

func TestWrite(t *testing.T) {
	hostile := "</script><script>alert(1)</script>"
	vs := []snapshot.Snapshot{
		{Version: 1, TS: "2026-10-01T00:00:00", Commits: map[string]string{"pay": "a1"},
			Edges: []graph.Hop{{Src: "pay", Dst: "ord", Kind: "kafka", Channel: "payments.done"}}},
		{Version: 2, TS: "2026-10-02T00:00:00", Commits: map[string]string{"pay": "a2"},
			Edges: []graph.Hop{{Src: "pay", Dst: "ord", Kind: "rest", Channel: hostile}}},
	}
	p := filepath.Join(t.TempDir(), "graph.html")
	if err := Write(p, "payments", vs, map[string]string{"pay": "payments", "ord": "orders"}); err != nil {
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
		m := regexp.MustCompile(`(?m)^const ` + name + ` = (.*);$`).FindStringSubmatch(s)
		if m == nil {
			t.Fatalf("%s not embedded", name)
		}
		if err := json.Unmarshal([]byte(m[1]), v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var data graph.Graph
	var domains []string
	var vdata struct {
		Versions []struct {
			Version int64
			Edges   [][4]string
		}
	}
	get("DATA", &data)
	get("DOMAINS", &domains)
	get("VDATA", &vdata)
	// the newest version is the graph; both domains show (ord is a cross-domain neighbour)
	if len(data.Edges) != 1 || data.Edges[0].Channel != hostile {
		t.Errorf("graph: %+v", data.Edges)
	}
	if strings.Join(domains, ",") != "orders,payments" {
		t.Errorf("domains: %v", domains)
	}
	// versions use the drawn form: kafka goes through its topic node
	if len(vdata.Versions) != 2 || vdata.Versions[0].Edges[0] != [4]string{"pay", "kafka:payments.done", "kafka", "payments.done"} {
		t.Errorf("versions: %+v", vdata.Versions)
	}
}
