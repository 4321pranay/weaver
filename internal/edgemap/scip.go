package edgemap

import (
	"database/sql"
	"fmt"
	"strings"
)

// scipIndex answers the code-reference questions of spec §4.4–4.6 from kb.db,
// memoised: one build asks about the same methods many times.
type scipIndex struct {
	kbs     []*sql.DB
	byRepo  map[string]*sql.DB
	methods map[[3]any]method
	callers map[string][]site
	eps     map[string]map[string]Entry
	cfg     map[string]map[string]string
	cg      map[string][]CGEdge
	cgIdx   map[string]map[string][]site // repo -> target descriptor -> call sites
}

type method struct {
	symbol string // raw SCIP symbol (for lookups)
	ref    Ref    // version-stripped symbol + definition site
}

type site struct {
	repo, path string
	line       int
}

func newSCIP(kbs []*sql.DB, cg map[string][]CGEdge) *scipIndex {
	s := &scipIndex{kbs: kbs, byRepo: map[string]*sql.DB{}, methods: map[[3]any]method{},
		callers: map[string][]site{}, eps: map[string]map[string]Entry{}, cfg: map[string]map[string]string{},
		cg: cg, cgIdx: map[string]map[string][]site{}}
	for _, db := range kbs {
		rows, err := db.Query("SELECT DISTINCT repo FROM occurrences")
		if err != nil {
			continue // no SCIP tables in this kb
		}
		for rows.Next() {
			var r string
			if rows.Scan(&r) == nil {
				s.byRepo[r] = db
			}
		}
		rows.Close()
	}
	return s
}

// methodAt: the innermost method whose body spans the 0-based line.
func (s *scipIndex) methodAt(repo, path string, line int) (method, bool) {
	k := [3]any{repo, path, line}
	if m, ok := s.methods[k]; ok {
		return m, m.symbol != ""
	}
	var m method
	if db := s.byRepo[repo]; db != nil && line >= 0 {
		var sym string
		var def int
		err := db.QueryRow(`SELECT symbol, start_line FROM occurrences
			WHERE repo = ? AND path = ? AND (role & 1) = 1 AND symbol LIKE '%).'
			  AND enc_start_line <= ? AND enc_end_line >= ?
			ORDER BY enc_end_line - enc_start_line LIMIT 1`, repo, path, line, line).Scan(&sym, &def)
		if err == nil {
			m = method{sym, Ref{Symbol: Symbol(sym), At: fmt.Sprintf("%s:%d", path, def+1)}}
		}
	}
	s.methods[k] = m
	return m, m.symbol != ""
}

// refs: every reference to sym, across all kbs.
// ponytail: exact symbol match, so a library referenced at another version
// than its own checkout is missed (spec §4.6 wants version-stripped matching).
func (s *scipIndex) refs(sym string) []site {
	if r, ok := s.callers[sym]; ok {
		return r
	}
	var out []site
	for _, db := range s.kbs {
		rows, err := db.Query("SELECT repo, path, start_line FROM occurrences WHERE symbol = ? AND (role & 1) = 0", sym)
		if err != nil {
			continue
		}
		for rows.Next() {
			var x site
			if rows.Scan(&x.repo, &x.path, &x.line) == nil {
				out = append(out, x)
			}
		}
		rows.Close()
	}
	s.callers[sym] = out
	return out
}

// calledBy: the methods referencing sym, one level up (spec §4.4).
func (s *scipIndex) calledBy(sym string) []Ref {
	var out []Ref
	seen := map[string]bool{}
	for _, r := range s.refs(sym) {
		ref := Ref{At: fmt.Sprintf("%s:%d", r.path, r.line+1)}
		if m, ok := s.methodAt(r.repo, r.path, r.line); ok {
			ref.Symbol = m.ref.Symbol
		}
		if !seen[ref.At] {
			seen[ref.At] = true
			out = append(out, ref)
		}
	}
	return out
}

// entries: the entry points of repo whose code reaches method sym (spec §4.5).
// "unknown" when the walk was cut off and found none: a dead end that is no
// entry point (injected interface, lambda, reflection). No depth limit: each
// method is visited once, so the walk ends.
func (s *scipIndex) entries(repo, sym string, eps map[string]Entry) any {
	var found []Entry
	seen := map[string]bool{sym: true}
	frontier := []string{sym}
	cut := false
	for len(frontier) > 0 {
		var next []string
		for _, m := range frontier {
			if e, ok := eps[m]; ok {
				found = append(found, e)
				continue // an entry point: the walk ends here
			}
			up := 0
			for _, r := range append(s.refs(m), s.cgSites(repo, m)...) {
				if r.repo != repo {
					continue // stays inside the service (spec §4.5 scope)
				}
				if c, ok := s.methodAt(r.repo, r.path, r.line); ok {
					up++
					if !seen[c.symbol] {
						seen[c.symbol] = true
						next = append(next, c.symbol)
					}
				}
			}
			if up == 0 {
				cut = true
			}
		}
		frontier = next
	}
	if len(found) == 0 {
		if cut {
			return "unknown"
		}
		return []Entry{}
	}
	return found
}

// Symbol: a SCIP symbol without scheme, manager and version (spec §4.6):
// `scip-java maven maven/com.shop/payment 1.4.0 shop/Pay#pay().` ->
// `maven/com.shop/payment . shop/Pay#pay().`
func Symbol(sym string) string {
	f := strings.SplitN(sym, " ", 5)
	if len(f) < 5 {
		return sym
	}
	return f[2] + " . " + f[4]
}

// cgSites: the interface call sites the call graph says reach method sym.
func (s *scipIndex) cgSites(repo, sym string) []site {
	idx, ok := s.cgIdx[repo]
	if !ok {
		idx = s.buildCG(repo)
		s.cgIdx[repo] = idx
	}
	return idx[descriptorKey(sym)]
}

// descriptorKey: `scip-java maven p v com/x/Impl#put(+1).` -> `com/x/Impl#put(`
func descriptorKey(sym string) string {
	f := strings.SplitN(sym, " ", 5)
	if len(f) < 5 {
		return ""
	}
	i := strings.Index(f[4], "(")
	if i < 0 {
		return ""
	}
	return f[4][:i+1]
}

func (s *scipIndex) buildCG(repo string) map[string][]site {
	idx := map[string][]site{}
	if len(s.cg[repo]) == 0 {
		return idx
	}
	// Java callers name a class; its file comes from SCIP's definition of it
	classFile := map[string]string{}
	if db := s.byRepo[repo]; db != nil {
		if rows, err := db.Query(`SELECT symbol, path FROM occurrences WHERE repo = ? AND (role & 1) = 1 AND symbol LIKE '%#'`, repo); err == nil {
			for rows.Next() {
				var sym, path string
				if rows.Scan(&sym, &path) == nil {
					if f := strings.SplitN(sym, " ", 5); len(f) == 5 {
						classFile[f[4]] = path
					}
				}
			}
			rows.Close()
		}
	}
	for _, e := range s.cg[repo] {
		path := e.File
		if path == "" {
			if i := strings.LastIndex(e.Caller, "#"); i > 0 {
				path = classFile[e.Caller[:i+1]]
			}
		}
		if path == "" || e.Line <= 0 {
			continue
		}
		for _, t := range e.Targets {
			idx[t] = append(idx[t], site{repo: repo, path: path, line: e.Line - 1})
		}
	}
	return idx
}
