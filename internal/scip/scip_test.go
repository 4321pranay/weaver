package scip

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	scipb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"weaver/internal/extract"
)

func TestMethodName(t *testing.T) {
	for sym, want := range map[string]string{
		"scip-java maven maven/com.acme/p 1.0.0 com/acme/AcmeKafka#publish().": "publish",
		"scip x com/acme/AcmeKafka#`<init>`().":                                "<init>",
		"scip x com/acme/Topics#":                                              "",
		"scip x com/acme/package.":                                             "",
	} {
		if got := MethodName(sym); got != want {
			t.Errorf("%s: got %q, want %q", sym, got, want)
		}
	}
}

// Synthetic merged-schema DB: a wrapper method defined beside a sink reference.
// Synthetic occurrences. lib's AcmeKafka#publish calls KafkaTemplate#send inside
// its own body -> a wrapper; its neighbour helper() in the same file doesn't.
// app references publish -> its publish() calls are Kafka. other has its own
// unrelated Plain#publish and never references lib's -> stays plain.
func TestWrappersScopedByReference(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	pub, helper := "scip x com/acme/AcmeKafka#publish().", "scip x com/acme/AcmeKafka#helper()."
	ins := func(repo, path, sym string, role, line int, enc ...int) {
		var es, ee any
		if len(enc) == 2 {
			es, ee = enc[0], enc[1]
		}
		if _, err := db.Exec("INSERT INTO occurrences VALUES (?,?,?,?,?,0,?,5,?,?)", repo, path, sym, role, line, line, es, ee); err != nil {
			t.Fatal(err)
		}
	}
	ins("lib", "AcmeKafka.java", pub, 1, 10, 10, 14)                          // publish() spans lines 10-14
	ins("lib", "AcmeKafka.java", "scip x org/x/KafkaTemplate#send().", 0, 12) // the sink, inside publish
	ins("lib", "AcmeKafka.java", helper, 1, 20, 20, 24)                       // helper() — no sink inside
	ins("app", "App.java", pub, 0, 3)
	ins("other", "Plain.java", "scip x com/other/Plain#publish().", 1, 2, 2, 4)

	syms, err := WrapperSymbols(db)
	if err != nil {
		t.Fatal(err)
	}
	kafka := extract.Wrapper{Kind: "kafka", Role: "produce"}
	if len(syms) != 1 || syms[pub] != kafka {
		t.Fatalf("wrapper symbols: %v (want only publish: helper is in the file but doesn't call the sink)", syms)
	}
	used, err := WrappersUsed(db, syms)
	if err != nil {
		t.Fatal(err)
	}
	if used["app"]["publish"] != kafka {
		t.Errorf("app references the wrapper: %v", used)
	}
	if _, ok := used["other"]; ok {
		t.Errorf("other's unrelated publish() must not be a wrapper: %v", used)
	}

	empty, _ := sql.Open("sqlite", filepath.Join(t.TempDir(), "e.db"))
	defer empty.Close()
	if w, err := WrapperSymbols(empty); err != nil || len(w) != 0 {
		t.Errorf("no SCIP tables -> empty, got %v %v", w, err)
	}
}

func probes(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..", "benchmarks", "scip_probes") // java/ (Maven) and go/
	if _, err := os.Stat(root); err != nil {
		t.Skip("benchmarks/scip_probes not present (gitignored)")
	}
	return root
}

func ingestProbe(t *testing.T, root, probe string) *sql.DB {
	t.Helper()
	kb, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kb.Close() })
	if _, err := Ingest(kb, []Part{{Scip: filepath.Join(root, probe, "index.scip"), Repo: probe}}); err != nil {
		t.Fatal(err)
	}
	return kb
}

// End to end on the probe projects in benchmarks/scip_probes/ — each a few files where an
// internal library method hides a Kafka/REST call, with a committed index.scip
// built by the real scip-java / scip-go (re-index after changing the sources): the source extractor alone misses each wrapped call; with SCIP
// wrapper resolution it recovers it. SCIP resolves the dispatch, source the value.
func TestProbesRecoverWrappedCalls(t *testing.T) {
	root := probes(t)
	cases := []struct {
		name, probe, kind, channel string
	}{
		{"java-kafka", "java", "kafka", "orders.created"},
		{"java-rest", "java", "rest", "/v1/users/lookup"},
		{"go-kafka", "go", "kafka", "orders.created"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kb := ingestProbe(t, root, c.probe)
			syms, err := WrapperSymbols(kb)
			if err != nil {
				t.Fatal(err)
			}
			used, err := WrappersUsed(kb, syms)
			if err != nil {
				t.Fatal(err)
			}
			has := func(x *extract.Extractor) bool {
				for _, e := range x.ExtractRepo(filepath.Join(root, c.probe)) {
					if e.Kind == c.kind && e.Channel == c.channel {
						return true
					}
				}
				return false
			}
			if has(&extract.Extractor{}) {
				t.Errorf("source-only should miss %s %s", c.kind, c.channel)
			}
			if !has(&extract.Extractor{Wrappers: used}) {
				t.Errorf("SCIP should recover %s %s (wrappers %v)", c.kind, c.channel, used)
			}
		})
	}
}

// Two classes define ORDERS with different values; Shipping sends to
// Topics.ORDERS and LegacyTopics.ORDERS. By name both are ambiguous; SCIP
// resolves each reference to its own constant.
func TestExactConstants(t *testing.T) {
	root := probes(t)
	kb := ingestProbe(t, root, "java")
	repoDir := filepath.Join(root, "java")
	cv, err := NewConstValues([]*sql.DB{kb}, func(string) string { return repoDir })
	if err != nil {
		t.Fatal(err)
	}
	channels := func(x *extract.Extractor) map[string]bool {
		out := map[string]bool{}
		for _, e := range x.ExtractRepo(repoDir) {
			if e.Kind == "kafka" && e.Role == "produce" {
				out[e.Channel] = true
			}
		}
		return out
	}
	// by name, ORDERS is ambiguous; only the naming-convention guess remains
	// (Class.ORDERS -> "orders"), which makes BOTH references "orders" — wrong
	// for Topics.ORDERS
	byName := channels(&extract.Extractor{})
	if byName["orders.v1"] {
		t.Errorf("name-based can't know Topics.ORDERS is orders.v1: %v", byName)
	}
	exact := channels(&extract.Extractor{Symbols: cv})
	if !exact["orders.v1"] || !exact["orders"] {
		t.Errorf("SCIP should resolve Topics.ORDERS -> orders.v1 and LegacyTopics.ORDERS -> orders: %v", exact)
	}
}

// Ingest reads index.scip with the official bindings: exact positions,
// enclosing ranges, display names, repo-relative paths; an occurrence with an
// invalid range is skipped alone; locals are dropped.
func TestIngest(t *testing.T) {
	idx := &scipb.Index{Documents: []*scipb.Document{{
		RelativePath: "Svc.java",
		Symbols:      []*scipb.SymbolInformation{{Symbol: "m com/a/Svc#run().", DisplayName: "run"}},
		Occurrences: []*scipb.Occurrence{
			{Symbol: "m com/a/Svc#run().", SymbolRoles: 1, Range: []int32{4, 8, 11}, EnclosingRange: []int32{4, 2, 9, 3}},
			{Symbol: "m com/a/Dep#call().", Range: []int32{6, 4, 6, 8}},
			{Symbol: "m com/a/Bad#x.", Range: []int32{7, 9, -2}}, // the petclinic case
			{Symbol: "local 3", Range: []int32{5, 1, 4}},
		},
	}}}
	b, err := proto.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "index.scip")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	kb, err := sql.Open("sqlite", filepath.Join(dir, "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer kb.Close()
	skipped, err := Ingest(kb, []Part{{Scip: p, Repo: "mono", Prefix: "svc/order"}})
	if err != nil || skipped != 1 {
		t.Fatalf("skipped %d, err %v", skipped, err)
	}
	type row struct {
		path, sym      string
		role, l, c, ec int
		es, ee         sql.NullInt64
	}
	rows, err := kb.Query("SELECT path, symbol, role, start_line, start_char, end_char, enc_start_line, enc_end_line FROM occurrences ORDER BY start_line")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.path, &r.sym, &r.role, &r.l, &r.c, &r.ec, &r.es, &r.ee); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 || got[0].path != "svc/order/Svc.java" || got[0].l != 4 || got[0].c != 8 || got[0].ec != 11 ||
		got[0].es.Int64 != 4 || got[0].ee.Int64 != 9 || got[1].sym != "m com/a/Dep#call()." || got[1].es.Valid {
		t.Errorf("occurrences: %+v", got)
	}
	var name string
	if err := kb.QueryRow("SELECT display_name FROM symbols").Scan(&name); err != nil || name != "run" {
		t.Errorf("symbols: %q %v", name, err)
	}
}

func TestSymbolNameOwner(t *testing.T) {
	for sym, want := range map[string][2]string{
		"scip-java maven m 1.0 com/acme/Topics#ORDERS.":                       {"ORDERS", "Topics"},
		"scip-java maven m 1.0 com/acme/Constants#Topics#CART_EVENTS.":        {"CART_EVENTS", "Topics"},
		"scip-go gomod acme/orders . `acme/orders/internal/messaging`/Topic.": {"Topic", "messaging"},
		"scip-java maven m 1.0 com/acme/AcmeKafka#publish().":                 {"publish", "AcmeKafka"},
	} {
		if n, o := SymbolName(sym), SymbolOwner(sym); n != want[0] || o != want[1] {
			t.Errorf("%s: name %q owner %q, want %v", sym, n, o, want)
		}
	}
}

// A monorepo: Go modules per service, a Gradle project whose subprojects belong
// to it, and build output that must be ignored.
func TestRoots(t *testing.T) {
	d := t.TempDir()
	for _, f := range []string{
		"svc/order/go.mod", "svc/order/sub/go.mod", "svc/catalog/go.mod",
		"java/settings.gradle", "java/app/build.gradle", "node_modules/x/go.mod", "svc/order/target/pom.xml",
	} {
		p := filepath.Join(d, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]string{}
	for _, r := range Roots(d) {
		got[r.Dir] = r.Tool
	}
	want := map[string]string{"java": "scip-java", "svc/catalog": "scip-go", "svc/order": "scip-go", "svc/order/sub": "scip-go"}
	if len(got) != len(want) {
		t.Fatalf("roots: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("roots: %v, want %v", got, want)
		}
	}
	if r := Roots(t.TempDir()); len(r) != 0 {
		t.Errorf("empty repo: %v", r)
	}
}

// A hung indexer (here a fake scip-go whose child sleeps, like a stuck dependency
// download) is killed at the timeout — children included — and reported.
func TestIndexTimeout(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "scip-go"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	err := Index(t.TempDir(), Root{Tool: "scip-go"}, filepath.Join(t.TempDir(), "x.scip"), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("returned after %s: the sleeping child kept the indexer alive", d)
	}
}
