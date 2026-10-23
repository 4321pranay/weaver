// Package kb queries the knowledge base: every domain's kb.db, read together.
// Each holds SCIP tables (occurrences with exact positions, symbols — tagged by
// repo) and the service_edges table the extractor writes.
// Cross-repo go-to-definition is a join on the global symbol string; results from
// all domains are concatenated, so cross-domain lookups and hops just work.
// SCIP role is a bitmask; bit 1 = definition.
package kb

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"weaver/internal/extract"
	"weaver/internal/graph"
)

const definitionRole = 1

type KB struct{ dbs []*sql.DB }

// Open opens kb.db files read-only, as one knowledge base.
func Open(paths ...string) (*KB, error) {
	k := &KB{}
	for _, p := range paths {
		db, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
		if err == nil {
			err = db.Ping()
		}
		if err != nil {
			k.Close()
			return nil, fmt.Errorf("open %s: %w", p, err)
		}
		k.dbs = append(k.dbs, db)
	}
	return k, nil
}

// OpenKnowledge opens every <root>/<domain>/kb.db.
func OpenKnowledge(root string) (*KB, error) {
	paths, _ := filepath.Glob(filepath.Join(root, "*", "kb.db"))
	if len(paths) == 0 {
		return nil, fmt.Errorf("no <domain>/kb.db under %s (run `weaver build` first)", root)
	}
	return Open(paths...)
}

func (k *KB) Close() error {
	for _, db := range k.dbs {
		db.Close()
	}
	return nil
}

// each runs fn over every domain DB, stopping at the first error.
func (k *KB) each(fn func(*sql.DB) error) error {
	for _, db := range k.dbs {
		if err := fn(db); err != nil {
			return err
		}
	}
	return nil
}

type Location struct {
	Repo         string `json:"repo"`
	Symbol       string `json:"symbol"`
	DisplayName  string `json:"display_name,omitempty"`
	RelativePath string `json:"relative_path"`
	Line         int64  `json:"line"`   // 1-based, as editors show it
	Column       int64  `json:"column"` // 1-based
}

// locations: occurrences of symbols matching q (a substring of the SCIP symbol,
// `_` and `%` literal) with the given role bit, exact positions.
func (k *KB) locations(q string, def bool, limit int) ([]Location, error) {
	want := 0
	if def {
		want = definitionRole
	}
	out := []Location{}
	err := k.each(func(db *sql.DB) error {
		if !hasTable(db, "occurrences") || len(out) >= limit {
			return nil // a domain without a SCIP index has no symbols
		}
		rows, err := db.Query(`SELECT o.repo, o.symbol, coalesce(min(s.display_name), ''), o.path, o.start_line + 1, o.start_char + 1
			FROM occurrences o LEFT JOIN symbols s ON s.repo = o.repo AND s.symbol = o.symbol
			WHERE o.symbol LIKE ? ESCAPE '\' AND (o.role & 1) = ?
			GROUP BY o.repo, o.symbol, o.path, o.start_line, o.start_char
			ORDER BY o.repo, o.path, o.start_line LIMIT ?`,
			"%"+likeEscape(q)+"%", want, limit-len(out))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l Location
			if err := rows.Scan(&l.Repo, &l.Symbol, &l.DisplayName, &l.RelativePath, &l.Line, &l.Column); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// FindDefinition: where a symbol (substring of its SCIP symbol, e.g. a method
// name or FQN fragment) is DEFINED, in any repo — exact file, line and column.
func (k *KB) FindDefinition(q string, limit int) ([]Location, error) {
	return k.locations(q, true, limit)
}

// FindReferences: every place, in any repo, that USES a matching symbol.
func (k *KB) FindReferences(q string, limit int) ([]Location, error) {
	return k.locations(q, false, limit)
}

func hasTable(db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n)
	return n > 0
}

type EdgeRow struct {
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
	Role    string `json:"role"`
	Repo    string `json:"repo"`
}

// ServiceFlow: raw service_edges for a service (either side, or its name in the
// channel), or every edge when service is "".
func (k *KB) ServiceFlow(service string) ([]EdgeRow, error) {
	q, args := "SELECT src, dst, kind, channel, role, coalesce(repo, '') FROM service_edges", []any{}
	if service != "" {
		q += " WHERE src=? OR dst=? OR channel LIKE ?"
		args = []any{service, service, "%" + service + "%"}
	}
	out := []EdgeRow{}
	err := k.each(func(db *sql.DB) error {
		rows, err := db.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e EdgeRow
			if err := rows.Scan(&e.Src, &e.Dst, &e.Kind, &e.Channel, &e.Role, &e.Repo); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Hop is a resolved, directed service -> service edge.
type Hop struct {
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
}

// ResolvedEdges: the service -> service hops, matched by graph.Resolve — the
// same rule versions.jsonl and graph.html use — minus hops with an unknown end.
// This is the directed graph BlastRadius walks.
func (k *KB) ResolvedEdges() ([]Hop, error) {
	rows, err := k.ServiceFlow("")
	if err != nil {
		return nil, err
	}
	raw := make([]extract.Edge, 0, len(rows))
	for _, r := range rows {
		raw = append(raw, extract.Edge{Src: r.Src, Dst: r.Dst, Kind: r.Kind, Channel: r.Channel, Role: r.Role, Repo: r.Repo})
	}
	out := []Hop{}
	for _, h := range graph.Resolve(raw) {
		if h.Src != "?" && h.Dst != "?" {
			out = append(out, Hop{h.Src, h.Dst, h.Kind, h.Channel})
		}
	}
	return out, nil
}

type Downstream struct {
	Layers   [][]string `json:"layers"`
	Affected []string   `json:"affected"`
	Edges    []Hop      `json:"edges"`
}

type Upstream struct {
	Layers    [][]string `json:"layers"`
	DependsOn []string   `json:"depends_on"`
	Edges     []Hop      `json:"edges"`
}

type Blast struct {
	Service    string      `json:"service"`
	Direction  string      `json:"direction"`
	MaxHops    int         `json:"max_hops"`
	Downstream *Downstream `json:"downstream,omitempty"`
	Upstream   *Upstream   `json:"upstream,omitempty"`
}

// BlastRadius: what is reachable from service, per hop. downstream = who
// depends on it (breaks if it's down), upstream = what it depends on, both.
// Static edges = POTENTIAL impact, not a guarantee the path is hit at runtime.
func BlastRadius(edges []Hop, service, direction string, maxHops int) Blast {
	fwd, rev := map[string][]Hop{}, map[string][]Hop{}
	for _, e := range edges {
		fwd[e.Src] = append(fwd[e.Src], e)
		rev[e.Dst] = append(rev[e.Dst], e)
	}
	bfs := func(adj map[string][]Hop, peerOf func(Hop) string) ([][]string, []string, []Hop) {
		seen := map[string]bool{service: true}
		layers, traversed, all := [][]string{}, []Hop{}, []string{}
		frontier := []string{service}
		for hops := 0; len(frontier) > 0 && hops < maxHops; hops++ {
			var next []string
			for _, n := range frontier {
				for _, e := range adj[n] {
					if p := peerOf(e); !seen[p] {
						seen[p] = true
						next = append(next, p)
						traversed = append(traversed, e)
					}
				}
			}
			if len(next) > 0 {
				layer := append([]string(nil), next...)
				sort.Strings(layer)
				layers = append(layers, layer)
				all = append(all, layer...)
			}
			frontier = next
		}
		sort.Strings(all)
		return layers, all, traversed
	}
	b := Blast{Service: service, Direction: direction, MaxHops: maxHops}
	if direction == "downstream" || direction == "both" {
		l, all, tr := bfs(fwd, func(e Hop) string { return e.Dst })
		b.Downstream = &Downstream{Layers: l, Affected: all, Edges: tr}
	}
	if direction == "upstream" || direction == "both" {
		l, all, tr := bfs(rev, func(e Hop) string { return e.Src })
		b.Upstream = &Upstream{Layers: l, DependsOn: all, Edges: tr}
	}
	return b
}

// Context builds the system prompt for a plain-English question: services named
// in the query, each with its resolved inbound/outbound edges, under rules that
// forbid the model from inventing connections. only (nil = all) limits which
// services can be named; their edges still reach services outside it.
func (k *KB) Context(query string, only map[string]bool) (string, error) {
	rows, err := k.ServiceFlow("")
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	var services []string
	for _, r := range rows {
		for _, s := range []string{r.Src, r.Dst} {
			if s != "" && !seen[s] {
				seen[s] = true
				services = append(services, s)
			}
		}
	}
	sort.Strings(services)

	lq := strings.ToLower(query)
	var found []string
	for _, s := range services {
		if only != nil && !only[s] {
			continue
		}
		if containsWord(lq, strings.ToLower(s)) {
			found = append(found, s)
		}
	}
	if len(found) == 0 {
		return "<architecture_graph>\nNo relevant architectural services found in your query.\n</architecture_graph>", nil
	}

	hops, err := k.ResolvedEdges()
	if err != nil {
		return "", err
	}
	var ctx strings.Builder
	for _, s := range found {
		fmt.Fprintf(&ctx, "Service: %s\n", s)
		var out, in []string
		for _, h := range hops {
			if h.Src == s {
				out = append(out, fmt.Sprintf("- OUTBOUND: Calls `%s` via `%s` on channel `%s`\n", h.Dst, h.Kind, h.Channel))
			}
			if h.Dst == s {
				in = append(in, fmt.Sprintf("- INBOUND: Called by `%s` via `%s` on channel `%s`\n", h.Src, h.Kind, h.Channel))
			}
		}
		for _, l := range append(out, in...) {
			ctx.WriteString(l)
		}
		if len(out)+len(in) == 0 {
			ctx.WriteString("- No known dependencies.\n")
		}
		ctx.WriteString("\n")
	}
	return `<system_instructions>
You are a Staff-Level Software Architect Assistant. You MUST obey these rules with 100% accuracy:
1. If a network connection is not listed in the <architecture_graph> block below, you MUST state that no connection exists. Do not guess or assume.
2. Base all architectural advice solely on the deterministic edges provided.
3. If the user asks about data schemas or code that is not present in this graph, explicitly state "Schema/Code not found in current context."
</system_instructions>

<architecture_graph>
` + strings.TrimSpace(ctx.String()) + `
</architecture_graph>
`, nil
}

// SymbolNames: distinct symbol display names starting with prefix (for shell
// completion), across the open domain DBs.
func (k *KB) SymbolNames(prefix string, limit int) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	err := k.each(func(db *sql.DB) error {
		if !hasTable(db, "symbols") {
			return nil // a domain with no SCIP index has no symbols
		}
		rows, err := db.Query(`SELECT DISTINCT display_name FROM symbols
			WHERE display_name LIKE ? ESCAPE '\' AND display_name != '' LIMIT ?`,
			likeEscape(prefix)+"%", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			if !seen[s] && len(out) < limit {
				seen[s] = true
				out = append(out, s)
			}
		}
		return rows.Err()
	})
	return out, err
}

// likeEscape makes text match itself in a LIKE ... ESCAPE '\' pattern: `_` and
// `%` (common in symbol names) are otherwise wildcards.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// containsWord: w occurs in s as a whole word (a regex \bw\b, without
// compiling one per service per question).
func containsWord(s, w string) bool {
	if w == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before := j == 0 || !isWordByte(s[j-1]) || !isWordByte(w[0])
		end := j + len(w)
		after := end == len(s) || !isWordByte(s[end]) || !isWordByte(w[len(w)-1])
		if before && after {
			return true
		}
		i = j + 1
	}
}
