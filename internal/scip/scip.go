// Package scip indexes repos with scip-java / scip-go, reads each index.scip
// with the official SCIP bindings into kb.db (exact occurrences + symbols,
// tagged by repo), and resolves wrapper methods and constants from it.
package scip

import (
	"context"
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
	Scip   string // the indexer's index.scip
	Repo   string // service name the rows are tagged with
	Prefix string // the root's folder inside the repo ("" = repo root): paths become repo-relative
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

// Ingest reads each part's index.scip into kb. An occurrence with an invalid
// range (seen from scip-java: negative offsets) is skipped on its own — it no
// longer costs the whole repo its index. Returns the count skipped.
func Ingest(kb *sql.DB, parts []Part) (skipped int, err error) {
	for _, q := range schema {
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
	for _, q := range indexes {
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
	occ, err := tx.Prepare("INSERT INTO occurrences VALUES (?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return 0, err
	}
	defer occ.Close()
	sym, err := tx.Prepare("INSERT INTO symbols VALUES (?,?,?)")
	if err != nil {
		return 0, err
	}
	defer sym.Close()
	skipped := 0
	for _, doc := range idx.GetDocuments() {
		path := filepath.ToSlash(filepath.Join(p.Prefix, doc.GetRelativePath()))
		for _, si := range doc.GetSymbols() {
			if s := si.GetSymbol(); s != "" && !strings.HasPrefix(s, "local ") {
				name := si.GetDisplayName()
				if name == "" {
					name = SymbolName(s)
				}
				if _, err := sym.Exec(p.Repo, s, name); err != nil {
					return 0, err
				}
			}
		}
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
			var encStart, encEnd any // NULL when the indexer gives no enclosing range
			if e, ok := occRange(o.GetEnclosingRange(), o.GetSingleLineEnclosingRange(), o.GetMultiLineEnclosingRange()); ok {
				encStart, encEnd = e.Start.Line, e.End.Line
			}
			if _, err := occ.Exec(p.Repo, path, s, o.GetSymbolRoles(),
				r.Start.Line, r.Start.Character, r.End.Line, r.End.Character, encStart, encEnd); err != nil {
				return 0, err
			}
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
			  AND d.symbol LIKE '%#%().'
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
	return out, nil
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
	err := kb.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='occurrences'").Scan(&n)
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
	return strings.Trim(name, "`")
}
