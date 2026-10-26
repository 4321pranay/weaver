// Package scip indexes repos with scip-java / scip-go, reads each index.scip
// with the official SCIP bindings into kb.db (exact occurrences + symbols,
// tagged by repo), and resolves wrapper methods and constants from it.
package scip

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	scipb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"

	"weaver/internal/extract"
)

// Root is one buildable project inside a repo. A repo can hold several: a
// monorepo with a go.mod per service, or Gradle/Maven projects in subfolders.
type Root struct {
	Dir  string `json:"dir"`  // relative to the repo, "" for the repo root
	Tool string `json:"tool"` // "scip-go" | "scip-java"
}

var skipDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true, "target": true, "build": true, "testdata": true}

// Roots finds every build root under repoDir: each go.mod (nested modules are
// separate modules), and each TOP-MOST Maven/Gradle project — its subprojects
// belong to it, so the walk stops there.
func Roots(repoDir string) []Root {
	var out []Root
	_ = filepath.WalkDir(repoDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != repoDir && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(repoDir, p)
		if rel == "." {
			rel = ""
		}
		for _, f := range []string{"pom.xml", "settings.gradle", "settings.gradle.kts", "build.gradle", "build.gradle.kts"} {
			if exists(p, f) {
				out = append(out, Root{rel, "scip-java"})
				return filepath.SkipDir
			}
		}
		if exists(p, "go.mod") {
			out = append(out, Root{rel, "scip-go"})
		}
		return nil
	})
	return out
}

// Index SCIP-indexes one build root of repoDir into outScip, giving up after
// timeout (0 = none). The indexers build the project, so they fetch every
// dependency first — the slow part, and where an air-gapped host without a Go
// proxy / Maven mirror hangs. Returns an error the caller logs and skips (the
// repo's service edges are still extracted).
func Index(repoDir string, r Root, outScip string, timeout time.Duration) error {
	// the indexers run inside the root, so a relative output path would land there
	outScip, err := filepath.Abs(outScip)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "scip-go", "--output", outScip)
	if r.Tool == "scip-java" {
		cmd = exec.CommandContext(ctx, "scip-java", "index", "--output", outScip)
		// Maven: never build or push images (a docker plugin bound to the build reaches for a
		// Docker host). Via MAVEN_OPTS: arguments after "--" would replace scip-java's own goals.
		cmd.Env = append(os.Environ(), "MAVEN_OPTS="+strings.TrimSpace(os.Getenv("MAVEN_OPTS")+" -Ddocker.skip=true"))
	}
	cmd.Dir = filepath.Join(repoDir, r.Dir)
	// kill the whole process group: the indexers spawn go list / gradle / mvn,
	// which would otherwise outlive the timeout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s: timed out after %s (raise SCIP_TIMEOUT, or check dependency downloads)", r.Tool, timeout)
	}
	if err != nil {
		return fmt.Errorf("%s: %v: %s", r.Tool, err, reason(out))
	}
	return nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// reason: the one line of a failed build worth showing (Gradle/Maven/Go print
// pages of trace); falls back to the last line.
func reason(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, l := range lines {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "> ") || strings.HasPrefix(t, "[ERROR]") ||
			strings.Contains(t, "Unsupported class file major version") || strings.HasPrefix(t, "error:") {
			return t
		}
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 200 {
		last = last[:200] + "…"
	}
	return last
}

// Part is one build root's index to ingest.
type Part struct {
	Scip    string // the indexer's index.scip
	Repo    string // service name the rows are tagged with
	Prefix  string // the root's folder inside the repo ("" = repo root): paths become repo-relative
	Release string // the commit built: the release these documents belong to
}

// Schema: SCIP read straight from index.scip with the official bindings, kept
// exact — every occurrence with its line/column range, and for definitions the
// enclosing range (the whole method/field). Lines and columns are 0-based, as
// in SCIP. Local symbols ("local N") are file-private and skipped.
var schema = []string{
	`CREATE TABLE occurrences (repo TEXT, path TEXT, symbol TEXT, role INTEGER,
		start_line INTEGER, start_char INTEGER, end_line INTEGER, end_char INTEGER,
		enc_start_line INTEGER, enc_end_line INTEGER)`,
	`CREATE TABLE symbols (repo TEXT, symbol TEXT, display_name TEXT)`,
}

var indexes = []string{
	"CREATE INDEX occ_symbol ON occurrences(symbol, role)",
	"CREATE INDEX occ_file ON occurrences(repo, path, start_line)",
	"CREATE INDEX sym_name ON symbols(display_name)",
}

// docSchema: SCIP stored by document (build-spec §4). A document is one file's
// SCIP data, keyed by a hash of its path and its occurrences and symbols, so a
// file that didn't change shares its document with the releases before it, and
// a monorepo's shared file is stored once. release_docs lists each service
// release's documents; occurrences and symbols are views of the current
// releases, so every query reads the code as it is now.
var docSchema = []string{
	`CREATE TABLE IF NOT EXISTS docs (doc TEXT PRIMARY KEY)`,
	`CREATE TABLE IF NOT EXISTS doc_occ (doc TEXT, path TEXT, symbol TEXT, role INTEGER,
		start_line INTEGER, start_char INTEGER, end_line INTEGER, end_char INTEGER,
		enc_start_line INTEGER, enc_end_line INTEGER)`,
	`CREATE TABLE IF NOT EXISTS doc_sym (doc TEXT, symbol TEXT, display_name TEXT)`,
	`CREATE TABLE IF NOT EXISTS release_docs (repo TEXT, release TEXT, doc TEXT, current INTEGER, UNIQUE(repo, release, doc))`,
	`CREATE VIEW IF NOT EXISTS occurrences AS SELECT r.repo AS repo, o.path, o.symbol, o.role, o.start_line, o.start_char,
		o.end_line, o.end_char, o.enc_start_line, o.enc_end_line
		FROM release_docs r JOIN doc_occ o ON o.doc = r.doc WHERE r.current = 1`,
	`CREATE VIEW IF NOT EXISTS symbols AS SELECT r.repo AS repo, s.symbol, s.display_name
		FROM release_docs r JOIN doc_sym s ON s.doc = r.doc WHERE r.current = 1`,
}

var docIndexes = []string{
	"CREATE INDEX IF NOT EXISTS doc_occ_doc ON doc_occ(doc)",
	"CREATE INDEX IF NOT EXISTS doc_occ_symbol ON doc_occ(symbol, role)",
	"CREATE INDEX IF NOT EXISTS doc_occ_file ON doc_occ(path, start_line)",
	"CREATE INDEX IF NOT EXISTS doc_sym_doc ON doc_sym(doc)",
	"CREATE INDEX IF NOT EXISTS doc_sym_name ON doc_sym(display_name)",
	"CREATE INDEX IF NOT EXISTS release_docs_doc ON release_docs(doc, current)",
	"CREATE INDEX IF NOT EXISTS release_docs_repo ON release_docs(repo, current)",
}

// CarryForward copies every release of the previous kb.db that isn't being
// rebuilt now (another commit of a service, or a service no longer built) into
// kb, as not current, with the documents they list: old releases are never
// indexed again, and never lost. A previous kb.db without documents has none.
func CarryForward(kb *sql.DB, prevPath string, built map[string]string) error {
	if _, err := os.Stat(prevPath); err != nil {
		return nil
	}
	for _, q := range docSchema {
		if _, err := kb.Exec(q); err != nil {
			return err
		}
	}
	if _, err := kb.Exec("ATTACH DATABASE ? AS prev", prevPath); err != nil {
		return err
	}
	defer kb.Exec("DETACH DATABASE prev")
	var n int
	if kb.QueryRow("SELECT count(*) FROM prev.sqlite_master WHERE name = 'release_docs'").Scan(&n); n == 0 {
		return nil
	}
	if _, err := kb.Exec("CREATE TEMP TABLE built (repo TEXT PRIMARY KEY, release TEXT)"); err != nil {
		return err
	}
	defer kb.Exec("DROP TABLE temp.built")
	for repo, rel := range built {
		if _, err := kb.Exec("INSERT INTO temp.built VALUES (?, ?)", repo, rel); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`INSERT OR IGNORE INTO release_docs SELECT r.repo, r.release, r.doc, 0 FROM prev.release_docs r
			WHERE NOT EXISTS (SELECT 1 FROM temp.built b WHERE b.repo = r.repo AND b.release = r.release)`,
		`INSERT OR IGNORE INTO docs SELECT d.doc FROM prev.docs d WHERE d.doc IN (SELECT doc FROM release_docs)`,
		`INSERT INTO doc_occ SELECT o.* FROM prev.doc_occ o WHERE o.doc IN (SELECT doc FROM release_docs)
			AND o.doc NOT IN (SELECT doc FROM doc_occ)`,
		`INSERT INTO doc_sym SELECT s.* FROM prev.doc_sym s WHERE s.doc IN (SELECT doc FROM release_docs)
			AND s.doc NOT IN (SELECT doc FROM doc_sym)`,
	} {
		if _, err := kb.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// Ingest reads each part's index.scip into kb. An occurrence with an invalid
// range (seen from scip-java: negative offsets) is skipped on its own — it no
// longer costs the whole repo its index. Returns the count skipped.
func Ingest(kb *sql.DB, parts []Part) (skipped int, err error) {
	for _, q := range docSchema {
		if _, err := kb.Exec(q); err != nil {
			return 0, err
		}
	}
	for _, p := range parts {
		n, err := ingestOne(kb, p)
		if err != nil {
			return skipped, fmt.Errorf("%s: %w", p.Repo, err)
		}
		skipped += n
	}
	for _, q := range docIndexes {
		if _, err := kb.Exec(q); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

func ingestOne(kb *sql.DB, p Part) (int, error) {
	b, err := os.ReadFile(p.Scip)
	if err != nil {
		return 0, err
	}
	var idx scipb.Index
	if err := proto.Unmarshal(b, &idx); err != nil {
		return 0, fmt.Errorf("read %s: %w", p.Scip, err)
	}
	tx, err := kb.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	occ, err := tx.Prepare("INSERT INTO doc_occ VALUES (?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return 0, err
	}
	defer occ.Close()
	sym, err := tx.Prepare("INSERT INTO doc_sym VALUES (?,?,?)")
	if err != nil {
		return 0, err
	}
	defer sym.Close()
	skipped := 0
	type orow struct {
		s                string
		role             int32
		sl, sc, el, ec   int32
		encStart, encEnd any
	}
	for _, doc := range idx.GetDocuments() {
		path := filepath.ToSlash(filepath.Join(p.Prefix, doc.GetRelativePath()))
		if path == ".." || strings.HasPrefix(path, "../") {
			continue // outside the repo, e.g. scip-go's generated test files in the build cache
		}
		h := sha1.New()
		fmt.Fprintf(h, "%s\x00", path)
		var syms [][2]string
		for _, si := range doc.GetSymbols() {
			if s := si.GetSymbol(); s != "" && !strings.HasPrefix(s, "local ") {
				name := si.GetDisplayName()
				if name == "" {
					name = SymbolName(s)
				}
				syms = append(syms, [2]string{s, name})
				fmt.Fprintf(h, "s%s\x00%s\x00", s, name)
			}
		}
		var occs []orow
		for _, o := range doc.GetOccurrences() {
			s := o.GetSymbol()
			if s == "" || strings.HasPrefix(s, "local ") {
				continue
			}
			r, ok := occRange(o.GetRange(), o.GetSingleLineRange(), o.GetMultiLineRange())
			if !ok {
				skipped++
				continue
			}
			row := orow{s: s, role: o.GetSymbolRoles(), sl: r.Start.Line, sc: r.Start.Character, el: r.End.Line, ec: r.End.Character}
			if e, ok := occRange(o.GetEnclosingRange(), o.GetSingleLineEnclosingRange(), o.GetMultiLineEnclosingRange()); ok {
				row.encStart, row.encEnd = e.Start.Line, e.End.Line
			}
			occs = append(occs, row)
			fmt.Fprintf(h, "o%s\x00%d %d %d %d %d %v %v\x00", s, row.role, row.sl, row.sc, row.el, row.ec, row.encStart, row.encEnd)
		}
		id := fmt.Sprintf("%x", h.Sum(nil))
		if res, err := tx.Exec("INSERT OR IGNORE INTO docs VALUES (?)", id); err != nil {
			return 0, err
		} else if n, _ := res.RowsAffected(); n == 1 { // a document not stored yet
			for _, x := range syms {
				if _, err := sym.Exec(id, x[0], x[1]); err != nil {
					return 0, err
				}
			}
			for _, o := range occs {
				if _, err := occ.Exec(id, path, o.s, o.role, o.sl, o.sc, o.el, o.ec, o.encStart, o.encEnd); err != nil {
					return 0, err
				}
			}
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO release_docs VALUES (?, ?, ?, 1)", p.Repo, p.Release, id); err != nil {
			return 0, err
		}
	}
	return skipped, tx.Commit()
}

// occRange: an occurrence's range from whichever encoding the indexer used.
func occRange(packed []int32, single *scipb.SingleLineRange, multi *scipb.MultiLineRange) (scipb.Range, bool) {
	switch {
	case len(packed) > 0:
		r, err := scipb.NewRange(packed)
		return r, err == nil
	case single != nil:
		r := scipb.Range{Start: scipb.Position{Line: single.Line, Character: single.StartCharacter},
			End: scipb.Position{Line: single.Line, Character: single.EndCharacter}}
		return r, r.Validate() == nil
	case multi != nil:
		r := scipb.Range{Start: scipb.Position{Line: multi.StartLine, Character: multi.StartCharacter},
			End: scipb.Position{Line: multi.EndLine, Character: multi.EndCharacter}}
		return r, r.Validate() == nil
	}
	return scipb.Range{}, false
}

// SymbolName: the last descriptor of a SCIP symbol, without its suffix —
// `…com/acme/Topics#ORDERS.` -> "ORDERS", `…AcmeKafka#publish().` -> "publish",
// “ …`acme/orders/internal/messaging`/Topic. “ -> "Topic".
func SymbolName(sym string) string {
	s := strings.TrimRight(strings.TrimSpace(sym), ".#/")
	s = strings.TrimSuffix(s, "()")
	if i := strings.IndexByte(s, '('); i >= 0 && strings.HasSuffix(s, ")") {
		s = s[:i] // disambiguated overloads: name(+1)
	}
	if i := strings.LastIndexAny(s, "#/ "); i >= 0 {
		s = s[i+1:]
	}
	return strings.Trim(s, "`")
}

// SymbolOwner: the descriptor before the name — the class for Java
// (`Topics#ORDERS.` -> "Topics"), the package's last path element for Go.
func SymbolOwner(sym string) string {
	s := strings.TrimRight(strings.TrimSpace(sym), ".#/")
	i := strings.LastIndexAny(s, "#/")
	if i < 0 {
		return ""
	}
	s = strings.TrimRight(s[:i], "`")
	if j := strings.LastIndexAny(s, "#/ `"); j >= 0 {
		s = s[j+1:]
	}
	return s
}

// Sinks: SCIP symbol substrings of each transport call, and the edge a wrapper
// around it produces.
var Sinks = []struct {
	Symbol string
	extract.Wrapper
}{
	// Java / Spring
	{"KafkaTemplate#send", extract.Wrapper{Kind: "kafka", Role: "produce"}},
	{"RestTemplate#exchange", extract.Wrapper{Kind: "rest", Role: "produce"}},
	{"RestTemplate#postForObject", extract.Wrapper{Kind: "rest", Role: "produce"}},
	{"RestTemplate#getForObject", extract.Wrapper{Kind: "rest", Role: "produce"}},
	{"WebClient#", extract.Wrapper{Kind: "rest", Role: "produce"}},
	{"RedisTemplate#", extract.Wrapper{Kind: "redis", Role: "produce"}},
	// Go (kafka-go / sarama / net-http)
	{"Writer#WriteMessages", extract.Wrapper{Kind: "kafka", Role: "produce"}},
	{"SyncProducer#SendMessage", extract.Wrapper{Kind: "kafka", Role: "produce"}},
	{"Client#Do", extract.Wrapper{Kind: "rest", Role: "produce"}},
}

// WrapperSymbols finds wrapper methods by their full SCIP symbol: a method
// whose own body (its definition's enclosing range) references a sink —
// `AcmeKafka#publish` calling `KafkaTemplate#send`. Where an indexer gives no
// enclosing ranges, falls back to "a method defined in a file that references a
// sink". Empty when the DB has no SCIP tables.
func WrapperSymbols(kb *sql.DB) (map[string]extract.Wrapper, error) {
	out := map[string]extract.Wrapper{}
	if ok, err := hasSCIP(kb); err != nil || !ok {
		return out, err
	}
	for _, s := range Sinks {
		rows, err := kb.Query(`SELECT DISTINCT d.symbol FROM occurrences r
			JOIN occurrences d ON d.repo = r.repo AND d.path = r.path AND (d.role & 1) = 1
			  AND (d.symbol LIKE '%#%().' OR (d.symbol LIKE 'scip-go %#%.' AND d.symbol NOT LIKE '%#'))
			  AND (d.enc_start_line IS NULL OR r.start_line BETWEEN d.enc_start_line AND d.enc_end_line)
			WHERE r.symbol LIKE ? AND (r.role & 1) = 0`, "%"+s.Symbol+"%")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sym string
			if err := rows.Scan(&sym); err != nil {
				rows.Close()
				return nil, err
			}
			if name := MethodName(sym); name != "" && name != "<init>" {
				if _, ok := out[sym]; !ok {
					out[sym] = s.Wrapper
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, interfaceWrappers(kb, out)
}

// interfaceWrappers adds the interface side of each wrapper: callers of an
// injected RedisService call RedisService#get(), not RedisServiceImpl#get().
// kb.db keeps no SCIP relationships, so a method counts when a type its
// wrapper's file references (`implements RedisService`) defines a method of
// the same descriptor. ponytail: name + referenced type, not the real
// is_implementation relationship; store relationships if this ever misfires.
func interfaceWrappers(kb *sql.DB, out map[string]extract.Wrapper) error {
	impls := map[string]extract.Wrapper{}
	for k, v := range out {
		impls[k] = v
	}
	for sym, w := range impls {
		i := strings.LastIndex(sym, "#")
		if i < 0 {
			continue
		}
		desc := sym[i:] // #get().
		rows, err := kb.Query(`SELECT DISTINCT m.symbol FROM occurrences d
			JOIN occurrences t ON t.repo = d.repo AND t.path = d.path AND (t.role & 1) = 0 AND t.symbol LIKE '%#'
			JOIN occurrences m ON (m.role & 1) = 1 AND m.symbol = t.symbol || substr(?, 2)
			WHERE d.symbol = ? AND (d.role & 1) = 1`, desc, sym)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil && m != sym {
				if _, ok := out[m]; !ok {
					out[m] = w
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// WrappersUsed scopes wrappers to the services that call them: for each repo in
// kb, the method names of the wrapper symbols (from any domain) its own code
// REFERENCES. Another service's unrelated publish() is never read as a Kafka
// call — a name counts only where SCIP saw the reference.
func WrappersUsed(kb *sql.DB, wrappers map[string]extract.Wrapper) (map[string]map[string]extract.Wrapper, error) {
	out := map[string]map[string]extract.Wrapper{}
	if ok, err := hasSCIP(kb); err != nil || !ok {
		return out, err
	}
	for sym, w := range wrappers {
		rows, err := kb.Query("SELECT DISTINCT repo FROM occurrences WHERE symbol = ? AND (role & 1) = 0", sym)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var repo string
			if err := rows.Scan(&repo); err != nil {
				rows.Close()
				return nil, err
			}
			if out[repo] == nil {
				out[repo] = map[string]extract.Wrapper{}
			}
			if name := MethodName(sym); out[repo][name] == (extract.Wrapper{}) {
				out[repo][name] = w
			}
		}
		rows.Close()
	}
	return out, nil
}

func hasSCIP(kb *sql.DB) (bool, error) {
	var n int
	err := kb.QueryRow("SELECT count(*) FROM sqlite_master WHERE type IN ('table', 'view') AND name='occurrences'").Scan(&n)
	return n > 0, err
}

// MethodName: `... com/acme/AcmeKafka#publish().` -> "publish" ("" if not a method).
func MethodName(sym string) string {
	parts := strings.Split(strings.TrimSpace(sym), " ")
	t := parts[len(parts)-1]
	_, after, ok := strings.Cut(t, "#")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(after, "(")
	return strings.Trim(strings.TrimSuffix(name, "."), "`") // scip-go: Type#Method. (no parens)
}
