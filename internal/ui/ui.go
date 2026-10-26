// Package ui renders the service map page (graph.html): a self-contained HTML
// file with the domain's map (edges.yaml) embedded, so it opens from disk with
// no server. Written only with `weaver build --graph`; nothing reads it.
package ui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"sort"
	"strings"

	"weaver/internal/edgemap"
	"weaver/internal/graph"
)

//go:embed graph.html
var page string

type version struct {
	Version int64             `json:"version"`
	TS      string            `json:"ts"`
	Commits map[string]string `json:"commits"`
	Edges   [][4]string       `json:"edges"` // [src, dst, kind, channel]
}

type edge struct {
	graph.Edge
	Call string `json:"call,omitempty"`
}

// spec node type -> the page's type (its colour and icon keys)
var pageType = map[string]string{"database": "db"}

// Write renders the domain's map m, as edges.yaml holds it.
// ponytail: the compare view gets only the current map; feed it
// edges.history snapshots when per-release compare is needed.
func Write(path, domain string, m edgemap.Map) error {
	nodes := []graph.Node{}
	seen := map[string]bool{}
	var domains []string
	services := 0
	for _, n := range m.Nodes {
		t := n.Type
		if pt, ok := pageType[t]; ok {
			t = pt
		}
		d, _ := n.Props["domain"].(string)
		label := ""
		if t != "service" {
			label = n.ID[strings.Index(n.ID, "/")+1:]
		} else {
			services++
			if d != "" && !seen[d] {
				seen[d] = true
				domains = append(domains, d)
			}
		}
		nodes = append(nodes, graph.Node{ID: n.ID, Type: t, Domain: d, Label: label})
	}
	sort.Strings(domains)
	edges := []edge{}
	cur := version{Edges: [][4]string{}, Commits: map[string]string{}}
	for _, e := range m.Edges {
		edges = append(edges, edge{graph.Edge{Src: e.From, Dst: e.To, Kind: e.Kind, Channel: e.Resource, Signature: e.Resource}, e.Call})
		cur.Edges = append(cur.Edges, [4]string{e.From, e.To, e.Kind, e.Resource})
	}

	// json.Marshal escapes <, > and & — so repo-derived strings can't close the <script>
	js := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err) // only plain structs here
		}
		return string(b)
	}
	out := strings.NewReplacer(
		"{{TITLE}}", html.EscapeString(domain+" · Service Map"),
		"{{SUB}}", html.EscapeString(fmt.Sprintf("%s · %d services · %d edges · %s", domain, services, len(edges), m.Run)),
		"/*WEAVER:DATA*/null", js(map[string]any{"nodes": nodes, "edges": edges}),
		"/*WEAVER:VDATA*/null", js(map[string]any{"versions": []version{cur}}),
		"/*WEAVER:DOMAINS*/[]", js(domains),
	).Replace(page)
	return os.WriteFile(path, []byte(out), 0o644)
}
