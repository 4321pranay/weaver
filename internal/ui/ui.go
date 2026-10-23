// Package ui renders the service map page (graph.html): a self-contained HTML
// file with the graph, its domains and its version history embedded, so it
// opens from disk with no server.
package ui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"sort"
	"strings"

	"weaver/internal/graph"
	"weaver/internal/snapshot"
)

//go:embed graph.html
var page string

type version struct {
	Version int64             `json:"version"`
	TS      string            `json:"ts"`
	Commits map[string]string `json:"commits"`
	Edges   [][4]string       `json:"edges"` // [src, dst, kind, channel], drawn form
}

// Write renders the map of one domain: the newest version as the graph, every
// version for the compare view. domainOf maps services of ALL domains, so a
// cross-domain neighbour is coloured by its own domain.
func Write(path, domain string, versions []snapshot.Snapshot, domainOf map[string]string) error {
	if len(versions) == 0 {
		return fmt.Errorf("%s: no versions to draw", domain)
	}
	// topics / keys / tables drawn as nodes, in the graph and in every version,
	// so compare mode matches edges the way they are drawn
	draw := func(s snapshot.Snapshot) graph.Graph { return graph.ToGraph(s.Edges, true, domainOf, nil) }

	g := draw(versions[len(versions)-1])
	var vs []version
	for _, s := range versions {
		v := version{Version: s.Version, TS: s.TS, Commits: s.Commits, Edges: [][4]string{}}
		for _, e := range draw(s).Edges {
			v.Edges = append(v.Edges, [4]string{e.Src, e.Dst, e.Kind, e.Channel})
		}
		vs = append(vs, v)
	}
	seen := map[string]bool{}
	domains := []string{}
	for _, n := range g.Nodes {
		if n.Domain != "" && !seen[n.Domain] {
			seen[n.Domain] = true
			domains = append(domains, n.Domain)
		}
	}
	sort.Strings(domains)

	// json.Marshal escapes <, > and & — so repo-derived strings can't close the <script>
	js := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err) // only plain structs here
		}
		return string(b)
	}
	services := 0
	for _, n := range g.Nodes {
		if n.Type == "service" {
			services++
		}
	}
	out := strings.NewReplacer(
		"{{TITLE}}", html.EscapeString(domain+" · Service Map"),
		"{{SUB}}", html.EscapeString(fmt.Sprintf("%s · %d services · %d versions", domain, services, len(vs))),
		"/*WEAVER:DATA*/null", js(g),
		"/*WEAVER:VDATA*/null", js(map[string]any{"versions": vs}),
		"/*WEAVER:DOMAINS*/[]", js(domains),
	).Replace(page)
	return os.WriteFile(path, []byte(out), 0o644)
}
