package build

import (
	"strings"

	"weaver/internal/edgemap"
	"weaver/internal/extract"
	"weaver/internal/facts"
	"weaver/internal/scip"
	"weaver/internal/snapshot"
)

// analyzerName: the analyzers that read a service, by the build roots its
// code is in or under (docs/analyzer-spec.md §1).
func analyzerName(clone, root string) string {
	var names []string
	for _, r := range scip.Roots(clone) {
		in := root == "" || r.Dir == "" || r.Dir == root || strings.HasPrefix(r.Dir, root+"/") || strings.HasPrefix(root, r.Dir+"/")
		n := map[string]string{"scip-go": "weave-go", "scip-java": "weave-java"}[r.Tool]
		if in && n != "" && !contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "names" // no analyzer: method names only (match: name)
	}
	return strings.Join(names, "+")
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// served: rows that are entry points (what the service serves or consumes),
// as opposed to calls it makes.
func served(e extract.Edge) bool {
	return e.Role == "consume" && (e.Kind == "rest" || e.Kind == "grpc" || e.Kind == "kafka")
}

// toFacts: one service's analyzer output as facts.
func toFacts(m snapshot.Manifest, analyzer string, rows []extract.Edge, cg []edgemap.CGEdge, cfg map[string]string, gaps []edgemap.Question) facts.Facts {
	f := facts.Facts{Service: m.ServiceName, Root: m.Root, Run: m.Run, Analyzer: facts.Analyzer{Name: analyzer},
		Entrypoints: []facts.Entry{}, Calls: []facts.Call{}, Callgraph: []facts.Hop{}, Gaps: []facts.Gap{},
		Config: facts.ConfigList(cfg, "files")}
	for _, e := range rows {
		src := "scip"
		if e.Match == "name" {
			src = "name"
		}
		if served(e) {
			if src == "scip" {
				src = "syntax" // a route or listener declaration
			}
			f.Entrypoints = append(f.Entrypoints, facts.Entry{Kind: e.Kind, Resource: e.Channel, At: facts.At(e.Path, e.Line), Source: src})
			continue
		}
		f.Calls = append(f.Calls, facts.Call{Kind: e.Kind, Role: e.Role, Resource: facts.Resource{Expr: e.Channel},
			At: facts.At(e.Path, e.Line), Host: e.Host, Call: e.Call, Source: src})
	}
	for _, h := range cg {
		src := "javacg"
		if h.File != "" {
			src = "gocg"
		}
		f.Callgraph = append(f.Callgraph, facts.Hop{File: h.File, Caller: h.Caller, Line: h.Line, To: h.Targets, Source: src})
	}
	for _, q := range gaps {
		src := "scip"
		if q.Type == "config" {
			src = "spring-probe"
		}
		f.Gaps = append(f.Gaps, facts.Gap{Type: q.Type, Subject: q.Subject, Ask: q.Ask, Expect: q.Expect, Sites: q.Sites, Source: src})
	}
	return f
}

// fromFacts: what the edge mapper takes from one service's facts.
func fromFacts(f facts.Facts) (rows []extract.Edge, cg []edgemap.CGEdge, gaps []edgemap.Question) {
	match := func(src string) string {
		if src == "name" {
			return "name"
		}
		return ""
	}
	for _, e := range f.Entrypoints {
		p, l := facts.Site(e.At)
		rows = append(rows, extract.Edge{Dst: f.Service, Kind: e.Kind, Channel: e.Resource, Role: "consume", Repo: f.Service, Path: p, Line: l, Match: match(e.Source)})
	}
	for _, c := range f.Calls {
		p, l := facts.Site(c.At)
		r := extract.Edge{Kind: c.Kind, Channel: c.Resource.Expr, Role: c.Role, Repo: f.Service, Path: p, Line: l, Call: c.Call, Host: c.Host, Match: match(c.Source)}
		if c.Role == "produce" {
			r.Src = f.Service
		} else {
			r.Dst = f.Service
		}
		rows = append(rows, r)
	}
	for _, h := range f.Callgraph {
		cg = append(cg, edgemap.CGEdge{File: h.File, Caller: h.Caller, Line: h.Line, Targets: h.To})
	}
	for _, g := range f.Gaps {
		gaps = append(gaps, edgemap.GapQuestion(g.Type, g.Subject, g.Ask, g.Expect, g.Sites))
	}
	return rows, cg, gaps
}

// addEntries adds spring-probe's entry points that tree-sitter didn't find: a
// route whose prefix comes from config, a topic from a placeholder. One found
// in the source is kept, with its location.
func addEntries(have, more []facts.Entry) []facts.Entry {
	seen := map[string]bool{}
	for _, e := range have {
		seen[e.Kind+" "+e.Resource] = true
	}
	for _, e := range more {
		if !seen[e.Kind+" "+e.Resource] {
			seen[e.Kind+" "+e.Resource] = true
			have = append(have, e)
		}
	}
	return have
}
