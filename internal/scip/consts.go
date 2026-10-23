package scip

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"

	"weaver/internal/extract"
)

// ConstValues resolves constant references exactly: the SCIP symbol occurring at
// the reference's file/line/column, and that symbol's string value as written at
// its definition. Implements extract.SymbolValues.
type ConstValues struct {
	db   map[string]*sql.DB // repo -> the kb holding its occurrences
	vals map[string]string  // symbol -> value (only unambiguous, string constants)

	mu   sync.Mutex
	refs map[[2]string][]refOcc // (repo, path) -> reference occurrences, loaded lazily
}

type refOcc struct {
	sym             string
	line, from, end int
}

// NewConstValues reads every term definition in kbs and keeps those whose source
// line declares a string constant of the same name. source(repo) is the repo's
// checkout on disk (its occurrences' paths are relative to it).
func NewConstValues(kbs []*sql.DB, source func(repo string) string) (*ConstValues, error) {
	c := &ConstValues{db: map[string]*sql.DB{}, vals: map[string]string{}, refs: map[[2]string][]refOcc{}}
	bad := map[string]bool{}
	for _, kb := range kbs {
		if ok, err := hasSCIP(kb); err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		// terms (fields, consts, vars) end in "." — methods in ")."
		rows, err := kb.Query(`SELECT repo, path, symbol, start_line FROM occurrences
			WHERE (role & 1) = 1 AND symbol LIKE '%.' AND symbol NOT LIKE '%).'
			ORDER BY repo, path`)
		if err != nil {
			return nil, err
		}
		type def struct {
			sym  string
			line int
		}
		byFile := map[[2]string][]def{}
		for rows.Next() {
			var repo, path, sym string
			var line int
			if err := rows.Scan(&repo, &path, &sym, &line); err != nil {
				rows.Close()
				return nil, err
			}
			c.db[repo] = kb
			byFile[[2]string{repo, path}] = append(byFile[[2]string{repo, path}], def{sym, line})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for f, defs := range byFile {
			consts, err := extract.FileConstants(filepath.Join(source(f[0]), filepath.FromSlash(f[1])))
			if err != nil || len(consts) == 0 {
				continue // generated / missing source: nothing to read
			}
			at := map[[2]any]string{} // (name, line) -> value
			for _, k := range consts {
				at[[2]any{k.Name, k.Line}] = k.Value
			}
			for _, d := range defs {
				v, ok := at[[2]any{SymbolName(d.sym), d.line}]
				if !ok {
					continue
				}
				if old, seen := c.vals[d.sym]; seen && old != v {
					bad[d.sym] = true // one symbol, two values: indexer and source disagree
				}
				c.vals[d.sym] = v
			}
		}
	}
	for s := range bad {
		delete(c.vals, s)
	}
	return c, nil
}

// Lookup: the value of the constant referenced as ref (`ORDERS`, `Topics.ORDERS`)
// whose name starts at 0-based line/col of service's file path.
func (c *ConstValues) Lookup(service, path string, line, col int, ref string) (string, bool) {
	name := ref[strings.LastIndex(ref, ".")+1:]
	owner := ""
	if parts := strings.Split(ref, "."); len(parts) > 1 {
		owner = parts[len(parts)-2]
	}
	var exact, onLine []string
	for _, o := range c.refsOf(service, path) {
		if o.line != line || SymbolName(o.sym) != name {
			continue
		}
		if col >= o.from && col < o.end {
			exact = append(exact, o.sym)
		}
		if owner == "" || SymbolOwner(o.sym) == owner {
			onLine = append(onLine, o.sym)
		}
	}
	for _, cands := range [][]string{exact, onLine} { // the occurrence at the column; else the qualifier on that line
		if s, ok := one(cands); ok {
			v, ok := c.vals[s]
			return v, ok
		}
	}
	return "", false
}

func one(syms []string) (string, bool) {
	if len(syms) == 0 {
		return "", false
	}
	for _, s := range syms[1:] {
		if s != syms[0] {
			return "", false // two different symbols: not exact
		}
	}
	return syms[0], true
}

func (c *ConstValues) refsOf(repo, path string) []refOcc {
	key := [2]string{repo, path}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.refs[key]; ok {
		return r
	}
	var out []refOcc
	if kb := c.db[repo]; kb != nil {
		rows, err := kb.Query(`SELECT symbol, start_line, start_char, end_char FROM occurrences
			WHERE repo = ? AND path = ? AND (role & 1) = 0 AND symbol LIKE '%.' AND symbol NOT LIKE '%).'`, repo, path)
		if err == nil {
			for rows.Next() {
				var o refOcc
				if rows.Scan(&o.sym, &o.line, &o.from, &o.end) == nil {
					out = append(out, o)
				}
			}
			rows.Close()
		}
	}
	c.refs[key] = out
	return out
}
