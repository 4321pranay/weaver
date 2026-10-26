// Package mcp serves weaver's knowledge as deterministic tools over MCP
// (docs/mcp-spec.md §2): JSON-RPC 2.0, one message per line on stdio. Every
// answer carries the build id of the domain it was read from.
package mcp

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"weaver/internal/build"
	"weaver/internal/edgemap"
	"weaver/internal/facts"
	"weaver/internal/kb"
	"weaver/internal/search"
	"weaver/internal/snapshot"
)

// Server answers tool calls from one knowledge folder.
type Server struct {
	L build.Layout
	// Pinned: domains read from a build in progress instead of their current
	// generation, for weave-ai reviewing the map that build is making.
	Pinned map[string]Pin
}

// Pin: where a build in progress keeps its code index and code text, and the
// releases it is building.
type Pin struct {
	KB, Zoekt string
	Releases  map[string]string // service -> release name
}

func (s *Server) kbPath(d string) string {
	if p, ok := s.Pinned[d]; ok {
		return p.KB
	}
	return s.L.KB(d)
}

func (s *Server) zoektDir(d string) string {
	if p, ok := s.Pinned[d]; ok {
		return p.Zoekt
	}
	return s.L.Zoekt(d)
}

func (s *Server) releaseNames(d string) map[string]string {
	if p, ok := s.Pinned[d]; ok {
		return p.Releases
	}
	return build.ReleaseNames(s.L.Releases(d))
}

type tool struct {
	desc   string
	params map[string]string // name -> description; "?" prefix = optional
	run    func(s *Server, a args) (any, error)
}

type args map[string]any

func (a args) str(k string) string {
	v, _ := a[k].(string)
	return v
}

func (a args) num(k string, def int) int {
	if f, ok := a[k].(float64); ok {
		return int(f)
	}
	return def
}

func (a args) flag(k string) bool { b, _ := a[k].(bool); return b }

var tools = map[string]tool{
	"domains": {"every domain with its services and current build id", nil, (*Server).domains},
	"symbol_search": {"SCIP symbols whose name contains query", map[string]string{"domain": "domain", "query": "name or fragment", "?limit": "max results (default 20)"},
		func(s *Server, a args) (any, error) { return s.locations(a, true) }},
	"definition": {"where a symbol is defined, with the lines around it", map[string]string{"domain": "domain", "symbol": "SCIP symbol or fragment"}, (*Server).definition},
	"references": {"every place that uses a symbol", map[string]string{"domain": "domain", "symbol": "SCIP symbol or fragment", "?limit": "max results (default 50)"},
		func(s *Server, a args) (any, error) { return s.locations(a, false) }},
	"search":   {"Zoekt query over the code text; current releases unless the query names a branch", map[string]string{"domain": "domain", "query": "Zoekt syntax", "?service": "limit to one service", "?limit": "max hits (default 50)"}, (*Server).search},
	"read":     {"lines of a file at a release (default: the current one)", map[string]string{"domain": "domain", "service": "service", "path": "repo-relative path", "?from": "first line (1)", "?to": "last line", "?release": "release version"}, (*Server).read},
	"config":   {"config keys a service reads, with values and sources", map[string]string{"domain": "domain", "service": "service", "?key": "key, or a fragment of it"}, (*Server).config},
	"nodes":    {"nodes of the map", map[string]string{"domain": "domain", "?type": "node type"}, (*Server).nodes},
	"edges":    {"edges of a node, without callers", map[string]string{"domain": "domain", "node": "node id", "?direction": "in, out or both (default both)", "?kind": "edge kind"}, (*Server).edges},
	"edge":     {"one edge with callers, handler and entries", map[string]string{"domain": "domain", "ref": "from → to kind resource"}, (*Server).edge},
	"blast":    {"what depends on a node, transitively", map[string]string{"domain": "domain", "node": "node id", "?all": "follow non-required edges too"}, (*Server).blast},
	"entries":  {"a service's entry points and the edges each one reaches", map[string]string{"domain": "domain", "service": "service"}, (*Server).entries},
	"releases": {"a service's releases", map[string]string{"domain": "domain", "service": "service"}, (*Server).releases},
	"diff":     {"edges added, removed and changed between two releases", map[string]string{"domain": "domain", "service": "service", "a": "release", "b": "release"}, (*Server).diff},
}

// Serve speaks MCP on r/w until r ends.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if req.ID == nil {
			continue // a notification: nothing to answer
		}
		res, rerr := s.handle(req.Method, req.Params)
		msg := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			msg["error"] = map[string]any{"code": -32601, "message": rerr.Error()}
		} else {
			msg["result"] = res
		}
		if err := enc.Encode(msg); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "weaver", "version": "1"}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		names := make([]string, 0, len(tools))
		for n := range tools {
			names = append(names, n)
		}
		sort.Strings(names)
		list := []any{}
		for _, n := range names {
			props, req := map[string]any{}, []string{}
			for p, d := range tools[n].params {
				name := strings.TrimPrefix(p, "?")
				typ := "string"
				switch name {
				case "limit", "from", "to":
					typ = "integer"
				case "all":
					typ = "boolean"
				}
				props[name] = map[string]any{"type": typ, "description": d}
				if !strings.HasPrefix(p, "?") {
					req = append(req, name)
				}
			}
			sort.Strings(req)
			list = append(list, map[string]any{"name": n, "description": tools[n].desc,
				"inputSchema": map[string]any{"type": "object", "properties": props, "required": req}})
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string `json:"name"`
			Arguments args   `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		out, err := s.Call(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}, nil
		}
		b, err := json.Marshal(plain(out))
		if err != nil {
			return nil, err
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}, nil
	}
	return nil, fmt.Errorf("method %s not found", method)
}

// Call runs one tool: {build, result}, read from the domain's current build.
func (s *Server) Call(name string, a args) (any, error) {
	t, ok := tools[name]
	if !ok {
		return nil, fmt.Errorf("no tool %q", name)
	}
	for p := range t.params {
		if !strings.HasPrefix(p, "?") && a.str(p) == "" {
			return nil, fmt.Errorf("%s needs %s", name, p)
		}
	}
	res, err := t.run(s, a)
	if err != nil {
		return nil, err
	}
	if d := a.str("domain"); d != "" {
		return map[string]any{"build": s.L.BuildID(d), "result": res}, nil
	}
	return res, nil
}

// ---- domains ----

func (s *Server) domains(args) (any, error) {
	out := []any{}
	for _, d := range s.L.Domains() {
		ms, _ := snapshot.ReadManifest(s.L.Domain(d))
		svcs := []string{}
		for _, m := range ms {
			svcs = append(svcs, m.ServiceName)
		}
		var meta struct {
			Run     string `yaml:"run"`
			BuiltAt string `yaml:"builtAt"`
		}
		if b, err := os.ReadFile(filepath.Join(s.L.Current(d), "build.yaml")); err == nil {
			_ = yaml.Unmarshal(b, &meta)
		}
		out = append(out, map[string]any{"domain": d, "build": s.L.BuildID(d), "run": meta.Run, "builtAt": meta.BuiltAt,
			"services": svcs, "nameMatched": s.nameShare(d)})
	}
	return out, nil
}

// nameShare: per service, the share of its edges found by method names only.
func (s *Server) nameShare(d string) map[string]float64 {
	m, err := s.edgeMap(d)
	if err != nil {
		return nil
	}
	all, named := map[string]int{}, map[string]int{}
	for _, e := range m.Edges {
		all[e.From]++
		if e.Match == "name" {
			named[e.From]++
		}
	}
	out := map[string]float64{}
	for svc, n := range all {
		out[svc] = float64(named[svc]) / float64(n)
	}
	return out
}

// ---- code (SCIP) ----

func (s *Server) kb(d string) (*kb.KB, error) { return kb.Open(s.kbPath(d)) }

func (s *Server) locations(a args, def bool) (any, error) {
	k, err := s.kb(a.str("domain"))
	if err != nil {
		return nil, err
	}
	defer k.Close()
	q := a.str("query")
	if q == "" {
		q = a.str("symbol")
	}
	locs, err := k.FindReferences(q, a.num("limit", 50))
	if def {
		locs, err = k.FindDefinition(q, a.num("limit", 20))
	}
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, l := range locs {
		out = append(out, map[string]any{"service": l.Repo, "symbol": l.Symbol, "at": fmt.Sprintf("%s:%d", l.RelativePath, l.Line)})
	}
	return out, nil
}

func (s *Server) definition(a args) (any, error) {
	d := a.str("domain")
	k, err := s.kb(d)
	if err != nil {
		return nil, err
	}
	defer k.Close()
	locs, err := k.FindDefinition(a.str("symbol"), 1)
	if err != nil || len(locs) == 0 {
		return nil, fmt.Errorf("no definition of %q", a.str("symbol"))
	}
	l := locs[0]
	lines, _ := s.lines(d, l.Repo, l.RelativePath, "", int(l.Line), int(l.Line)+15)
	return map[string]any{"symbol": l.Symbol, "service": l.Repo, "at": fmt.Sprintf("%s:%d", l.RelativePath, l.Line), "snippet": lines}, nil
}

// ---- text (Zoekt) ----

func (s *Server) search(a args) (any, error) {
	d := a.str("domain")
	q := a.str("query")
	if svc := a.str("service"); svc != "" {
		q = fmt.Sprintf("repo:^%s$ (%s)", svc, q)
	}
	return search.SearchReleases([]string{s.zoektDir(d)}, q, a.num("limit", 50), s.releaseNames(d))
}

func (s *Server) read(a args) (any, error) {
	return s.lines(a.str("domain"), a.str("service"), a.str("path"), a.str("release"), a.num("from", 1), a.num("to", 0))
}

// lines: [{n, text}] of a file at a release (default current), from Zoekt.
func (s *Server) lines(d, svc, path, release string, from, to int) ([]map[string]any, error) {
	if release == "" {
		release = s.releaseNames(d)[svc]
	}
	text, ok, err := search.ReadFile([]string{s.zoektDir(d)}, svc, path, release)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s %s not found at release %s", svc, path, release)
	}
	all := strings.Split(text, "\n")
	if to <= 0 || to > len(all) {
		to = len(all)
	}
	out := []map[string]any{}
	for n := max(from, 1); n <= to; n++ {
		out = append(out, map[string]any{"n": n, "text": all[n-1]})
	}
	return out, nil
}

// ---- values ----

func (s *Server) config(a args) (any, error) {
	f, err := facts.Read(filepath.Join(s.L.Current(a.str("domain")), "facts"), a.str("service"))
	if err != nil {
		return nil, err
	}
	out := []facts.Value{}
	for _, v := range f.Config {
		if k := a.str("key"); k == "" || strings.Contains(v.Key, k) {
			out = append(out, v)
		}
	}
	return out, nil
}

// ---- map ----

func (s *Server) edgeMap(d string) (edgemap.Map, error) {
	var m edgemap.Map
	b, err := os.ReadFile(s.L.EdgeMap(d))
	if err != nil {
		return m, err
	}
	return m, yaml.Unmarshal(b, &m)
}

func (s *Server) nodes(a args) (any, error) {
	m, err := s.edgeMap(a.str("domain"))
	if err != nil {
		return nil, err
	}
	out := []edgemap.Node{}
	for _, n := range m.Nodes {
		if t := a.str("type"); t == "" || n.Type == t {
			out = append(out, n)
		}
	}
	return out, nil
}

func (s *Server) edges(a args) (any, error) {
	m, err := s.edgeMap(a.str("domain"))
	if err != nil {
		return nil, err
	}
	node, dir := a.str("node"), a.str("direction")
	out := []edgemap.Edge{}
	for _, e := range m.Edges {
		in, outb := e.To == node, e.From == node
		if (dir == "in" && !in) || (dir == "out" && !outb) || (!in && !outb) {
			continue
		}
		if k := a.str("kind"); k != "" && e.Kind != k {
			continue
		}
		e.Callers, e.Handler = nil, nil
		out = append(out, e)
	}
	return out, nil
}

func ref(e edgemap.Edge) string {
	return fmt.Sprintf("%s → %s %s %s", e.From, e.To, e.Kind, e.Resource)
}

func (s *Server) edge(a args) (any, error) {
	m, err := s.edgeMap(a.str("domain"))
	if err != nil {
		return nil, err
	}
	want := strings.Join(strings.Fields(strings.ReplaceAll(a.str("ref"), "->", "→")), " ")
	for _, e := range m.Edges {
		if ref(e) == want {
			return e, nil
		}
	}
	return nil, fmt.Errorf("no edge %q", want)
}

// dependsOn: the node each edge makes its dependent rely on. A call edge's
// caller depends on its target; a consume or subscribe edge's service depends
// on the broker it reads from.
func dependsOn(e edgemap.Edge) (dependent, on string) {
	if e.Op == "consume" || e.Op == "subscribe" {
		return e.To, e.From
	}
	return e.From, e.To
}

func (s *Server) blast(a args) (any, error) {
	m, err := s.edgeMap(a.str("domain"))
	if err != nil {
		return nil, err
	}
	all := a.flag("all")
	type item struct {
		Node string   `json:"node"`
		Path []string `json:"path"`
	}
	seen := map[string]bool{a.str("node"): true}
	queue := []item{{a.str("node"), []string{a.str("node")}}}
	out := []item{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range m.Edges {
			dep, on := dependsOn(e)
			if on != cur.Node || seen[dep] {
				continue
			}
			if !all && e.Call != "" && e.Call != "required" && e.Call != "unknown" {
				continue // only failures that reach the caller spread
			}
			seen[dep] = true
			it := item{dep, append(append([]string{}, cur.Path...), dep)}
			out = append(out, it)
			queue = append(queue, it)
		}
	}
	return out, nil
}

func (s *Server) entries(a args) (any, error) {
	m, err := s.edgeMap(a.str("domain"))
	if err != nil {
		return nil, err
	}
	svc := a.str("service")
	reach := map[string][]string{}
	var order []string
	for _, e := range m.Edges {
		if e.From != svc {
			continue
		}
		for _, c := range e.Callers {
			list, ok := c.Entries.([]any)
			if !ok {
				continue
			}
			for _, x := range list {
				en, _ := x.(map[string]any)
				key := fmt.Sprintf("%v %v", en["kind"], en["resource"])
				if _, ok := reach[key]; !ok {
					order = append(order, key)
				}
				if r := ref(e); !contains(reach[key], r) {
					reach[key] = append(reach[key], r)
				}
			}
		}
	}
	sort.Strings(order)
	out := []any{}
	for _, k := range order {
		out = append(out, map[string]any{"entry": k, "edges": reach[k]})
	}
	return out, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ---- releases ----

func (s *Server) releaseFile(d string) (edgemap.ReleaseFile, error) {
	var f edgemap.ReleaseFile
	b, err := os.ReadFile(s.L.Releases(d))
	if err != nil {
		return f, err
	}
	return f, yaml.Unmarshal(b, &f)
}

func (s *Server) releases(a args) (any, error) {
	f, err := s.releaseFile(a.str("domain"))
	if err != nil {
		return nil, err
	}
	for _, sv := range f.Services {
		if sv.ServiceName == a.str("service") {
			return sv.Releases, nil
		}
	}
	return nil, fmt.Errorf("no service %q", a.str("service"))
}

func (s *Server) diff(a args) (any, error) {
	d, svc := a.str("domain"), a.str("service")
	f, err := s.releaseFile(d)
	if err != nil {
		return nil, err
	}
	snap := map[string]string{} // release version -> its snapshot
	for _, sv := range f.Services {
		if sv.ServiceName == svc {
			for _, r := range sv.Releases {
				snap[r.Version] = r.Edges
			}
		}
	}
	load := func(rel string) (map[string]edgemap.Edge, error) {
		name, ok := snap[rel]
		if !ok || name == "" {
			return nil, fmt.Errorf("%s has no snapshot for release %q", svc, rel)
		}
		var m edgemap.Map
		b, err := os.ReadFile(filepath.Join(s.L.History(d), svc, name+".yaml"))
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		out := map[string]edgemap.Edge{}
		for _, e := range m.Edges {
			e.Callers, e.Handler = nil, nil
			out[ref(e)] = e
		}
		return out, nil
	}
	ea, err := load(a.str("a"))
	if err != nil {
		return nil, err
	}
	eb, err := load(a.str("b"))
	if err != nil {
		return nil, err
	}
	res := map[string][]any{"added": {}, "removed": {}, "changed": {}}
	for k, e := range eb {
		if old, ok := ea[k]; !ok {
			res["added"] = append(res["added"], e)
		} else if old.Call != e.Call || old.Op != e.Op {
			res["changed"] = append(res["changed"], map[string]any{"edge": k, "from": map[string]string{"call": old.Call, "op": old.Op}, "to": map[string]string{"call": e.Call, "op": e.Op}})
		}
	}
	for k, e := range ea {
		if _, ok := eb[k]; !ok {
			res["removed"] = append(res["removed"], e)
		}
	}
	return res, nil
}

// plain: a result with the field names of weaver's files (their yaml tags,
// empty fields left out), so an answer reads like edges.yaml.
func plain(v any) any {
	b, err := yaml.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if yaml.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// Pinned is a Server reading one domain from a build in progress, as weave-ai's
// tools (it satisfies weaveai.Tools).
type Pinned struct{ S *Server }

func (p Pinned) Call(name string, a map[string]any) (any, error) { return p.S.Call(name, args(a)) }

// ---- call structure (SCIP) ----

func init() {
	tools["enclosing_method"] = tool{"the method whose body contains a location", map[string]string{"domain": "domain", "service": "service", "at": "path:line"}, (*Server).enclosingMethod}
	tools["call_chain"] = tool{"who calls a method (up) or what it calls (down), as a tree", map[string]string{"domain": "domain", "symbol": "SCIP symbol", "direction": "up or down", "?depth": "levels (default 5)"}, (*Server).callChain}
	tools["implementations"] = tool{"types implementing an interface, or methods implementing an interface method", map[string]string{"domain": "domain", "symbol": "SCIP symbol of the interface or its method"}, (*Server).implementations}
}

// method symbols: Java `#m().`, Go `#M.` and functions `F().`
const methodLike = `(o.symbol LIKE '%).' OR (o.symbol LIKE 'scip-go %#%.' AND o.symbol NOT LIKE '%#'))`

func (s *Server) db(d string) (*sql.DB, error) { return sql.Open("sqlite", s.kbPath(d)) }

// enclosing: the innermost method definition around path:line (0-based).
func enclosing(db *sql.DB, repo, path string, line int) (string, string, error) {
	var sym string
	var start, end int
	err := db.QueryRow(`SELECT o.symbol, o.enc_start_line, o.enc_end_line FROM occurrences o
		WHERE o.repo = ? AND o.path = ? AND (o.role & 1) = 1 AND `+methodLike+`
		AND o.enc_start_line <= ? AND o.enc_end_line >= ?
		ORDER BY o.enc_end_line - o.enc_start_line LIMIT 1`, repo, path, line, line).Scan(&sym, &start, &end)
	if err != nil {
		return "", "", err
	}
	return sym, fmt.Sprintf("%s:%d-%d", path, start+1, end+1), nil
}

func (s *Server) enclosingMethod(a args) (any, error) {
	db, err := s.db(a.str("domain"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	path, line := facts.Site(a.str("at"))
	sym, rng, err := enclosing(db, a.str("service"), path, line)
	if err != nil {
		return nil, fmt.Errorf("no method around %s", a.str("at"))
	}
	return map[string]any{"symbol": sym, "range": rng}, nil
}

type chainNode struct {
	Symbol   string      `json:"symbol" yaml:"symbol"`
	At       string      `json:"at,omitempty" yaml:"at,omitempty"`
	Cut      string      `json:"cut,omitempty" yaml:"cut,omitempty"`
	Children []chainNode `json:"children,omitempty" yaml:"children,omitempty"`
}

func (s *Server) callChain(a args) (any, error) {
	db, err := s.db(a.str("domain"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	up := a.str("direction") != "down"
	seen := map[string]bool{}
	var walk func(sym string, depth int) chainNode
	walk = func(sym string, depth int) chainNode {
		n := chainNode{Symbol: sym}
		if seen[sym] {
			n.Cut = "cycle"
			return n
		}
		seen[sym] = true
		if depth == 0 {
			n.Cut = "depth"
			return n
		}
		var rows *sql.Rows
		if up { // each reference, and the method it is in
			rows, err = db.Query(`SELECT r.repo, r.path, r.start_line FROM occurrences r WHERE r.symbol = ? AND (r.role & 1) = 0`, sym)
		} else { // each method called inside sym's body
			rows, err = db.Query(`SELECT DISTINCT c.symbol, c.path, c.start_line FROM occurrences d JOIN occurrences c
				ON c.repo = d.repo AND c.path = d.path AND (c.role & 1) = 0 AND c.start_line BETWEEN d.enc_start_line AND d.enc_end_line
				WHERE d.symbol = ? AND (d.role & 1) = 1 AND (c.symbol LIKE '%).' OR (c.symbol LIKE 'scip-go %#%.' AND c.symbol NOT LIKE '%#'))`, sym)
		}
		if err != nil {
			n.Cut = err.Error()
			return n
		}
		type hit struct {
			x, path string
			line    int
		}
		var hits []hit
		for rows.Next() {
			var h hit
			if rows.Scan(&h.x, &h.path, &h.line) == nil {
				hits = append(hits, h)
			}
		}
		rows.Close()
		for _, h := range hits {
			if up {
				m, _, err := enclosing(db, h.x, h.path, h.line)
				if err != nil {
					n.Children = append(n.Children, chainNode{Symbol: "(top level)", At: facts.At(h.path, h.line)})
					continue
				}
				c := walk(m, depth-1)
				c.At = facts.At(h.path, h.line)
				n.Children = append(n.Children, c)
			} else if h.x != sym {
				c := walk(h.x, depth-1)
				c.At = facts.At(h.path, h.line)
				n.Children = append(n.Children, c)
			}
		}
		return n
	}
	return walk(a.str("symbol"), a.num("depth", 5)), nil
}

// implementations: what an interface method's calls reach, from the call
// graphs in the facts (gocg, javacg): exact for Go's implicit interfaces too.
// With no call-graph rows, a type whose file references the interface and that
// defines the same method. ponytail: that fallback is a name rule; store SCIP's
// is_implementation relationships at ingest if it misfires.
func (s *Server) implementations(a args) (any, error) {
	d, sym := a.str("domain"), a.str("symbol")
	db, err := s.db(d)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	type site struct {
		path string
		line int
	}
	refs := map[string][]site{} // service -> where sym is called
	rows, err := db.Query(`SELECT repo, path, start_line FROM occurrences WHERE symbol = ? AND (role & 1) = 0`, sym)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var repo string
		var st site
		if rows.Scan(&repo, &st.path, &st.line) == nil {
			refs[repo] = append(refs[repo], st)
		}
	}
	rows.Close()
	seen := map[string]bool{}
	out := []any{}
	for svc, sites := range refs {
		f, err := facts.Read(filepath.Join(s.L.Current(d), "facts"), svc)
		if err != nil {
			continue
		}
		for _, h := range f.Callgraph {
			for _, st := range sites {
				hit := h.Line == st.line+1 && (h.File == st.path ||
					(h.Caller != "" && strings.HasSuffix(st.path, "/"+strings.Split(h.Caller[strings.LastIndex(h.Caller, "/")+1:], "#")[0]+".java")))
				if !hit {
					continue
				}
				for _, t := range h.To {
					if !seen[t] {
						seen[t] = true
						out = append(out, map[string]any{"symbol": t, "via": h.Source})
					}
				}
			}
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	iface, method := sym, ""
	if i := strings.LastIndex(sym, "#"); i >= 0 && i < len(sym)-1 {
		iface, method = sym[:i+1], sym[i+1:]
	}
	rows, err = db.Query(`SELECT DISTINCT d.symbol, d.path, d.start_line FROM occurrences t
		JOIN occurrences d ON d.repo = t.repo AND d.path = t.path AND (d.role & 1) = 1
		WHERE t.symbol = ? AND (t.role & 1) = 0 AND d.symbol != ? AND d.symbol LIKE ?
		AND d.path NOT IN (SELECT path FROM occurrences WHERE symbol = ? AND (role & 1) = 1)`, iface, sym, "%#"+method, iface)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x, path string
		var line int
		if rows.Scan(&x, &path, &line) == nil {
			out = append(out, map[string]any{"symbol": x, "at": facts.At(path, line), "via": "name"})
		}
	}
	return out, nil
}
