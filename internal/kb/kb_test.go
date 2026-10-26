package kb

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const sym = "maven auth 2.3.1 com/company/auth/TokenValidator#validateToken()."

// A 2-repo merged DB: auth DEFINES the symbol, payment REFERENCES it.
func testKB(t *testing.T) *KB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kb.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE occurrences (repo TEXT, path TEXT, symbol TEXT, role INTEGER, start_line INTEGER,
			start_char INTEGER, end_line INTEGER, end_char INTEGER, enc_start_line INTEGER, enc_end_line INTEGER)`,
		"CREATE TABLE symbols (repo TEXT, symbol TEXT, display_name TEXT)",
		"CREATE TABLE service_edges (src TEXT, dst TEXT, kind TEXT, channel TEXT, role TEXT, repo TEXT)",
		// auth DEFINES it at line 12 col 16 (0-based 11:15); payment USES it at 5:9
		"INSERT INTO occurrences VALUES ('auth','TokenValidator.java','" + sym + "',1,11,15,11,28,9,20), ('payment','Charge.java','" + sym + "',0,4,8,4,21,NULL,NULL)",
		"INSERT INTO symbols VALUES ('auth','" + sym + "','validateToken')",
		`INSERT INTO service_edges VALUES
			('checkout','','rest','/v1/charge','produce','checkout'),
			('','payment','rest','/v1/charge','consume','payment'),
			('payment','','kafka','payments.done','produce','payment'),
			('','ledger','kafka','payments.done','consume','ledger')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	k, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Close() })
	return k
}

func TestCrossRepoDefRefs(t *testing.T) {
	k := testKB(t)
	defs, err := k.FindDefinition("validateToken", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Repo != "auth" || defs[0].RelativePath != "TokenValidator.java" ||
		defs[0].Line != 12 || defs[0].Column != 16 || defs[0].DisplayName != "validateToken" {
		t.Errorf("defs: %+v", defs)
	}
	refs, err := k.FindReferences("validateToken", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Repo != "payment" || refs[0].Line != 5 || refs[0].Column != 9 {
		t.Errorf("refs: %+v", refs)
	}
}

func TestResolvedEdgesAndContext(t *testing.T) {
	k := testKB(t)
	edges, err := k.ResolvedEdges()
	if err != nil {
		t.Fatal(err)
	}
	want := []Hop{{"checkout", "payment", "rest", "/v1/charge"}, {"payment", "ledger", "kafka", "payments.done"}}
	if !slices.Equal(edges, want) {
		t.Errorf("resolved: %+v", edges)
	}
	got, err := k.Context([]string{"payment"})
	if err != nil {
		t.Fatal(err)
	}
	wantCtx := ServiceContext{Service: "payment",
		Outbound: []Peer{{"ledger", "kafka", "payments.done"}},
		Inbound:  []Peer{{"checkout", "rest", "/v1/charge"}}}
	if len(got) != 1 || got[0].Service != wantCtx.Service || !slices.Equal(got[0].Outbound, wantCtx.Outbound) || !slices.Equal(got[0].Inbound, wantCtx.Inbound) {
		t.Errorf("context: %+v, want %+v", got, wantCtx)
	}
	if _, err := k.Context([]string{"nope"}); err == nil || !strings.Contains(err.Error(), `unknown service "nope"`) {
		t.Errorf("unknown service: %v", err)
	}
}

func TestBlastRadius(t *testing.T) {
	edges := []Hop{{"a", "b", "rest", "/x"}, {"b", "c", "kafka", "t"}, {"z", "a", "rest", "/y"}}
	d := BlastRadius(edges, "a", "downstream", 5)
	if !slices.Equal(d.Downstream.Affected, []string{"b", "c"}) || len(d.Downstream.Layers) != 2 || d.Downstream.Layers[1][0] != "c" {
		t.Errorf("downstream: %+v", d.Downstream)
	}
	u := BlastRadius(edges, "a", "upstream", 5)
	if !slices.Equal(u.Upstream.DependsOn, []string{"z"}) || u.Downstream != nil {
		t.Errorf("upstream: %+v", u)
	}
	if one := BlastRadius(edges, "a", "downstream", 1); !slices.Equal(one.Downstream.Affected, []string{"b"}) {
		t.Errorf("max_hops=1: %+v", one.Downstream)
	}
}

// Domains are separate kb.db files; a hop whose producer and consumer live in
// different domains still resolves, and BlastRadius crosses it.
func TestCrossDomainHop(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, row string) string {
		p := filepath.Join(dir, name, "kb.db")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, q := range []string{
			"CREATE TABLE service_edges (src TEXT, dst TEXT, kind TEXT, channel TEXT, role TEXT, repo TEXT)",
			"INSERT INTO service_edges VALUES " + row,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	mk("payments", "('pay','','kafka','payments.done','produce','pay')")
	mk("orders", "('','ord','kafka','payments.done','consume','ord')")
	k, err := OpenKnowledge(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	edges, err := k.ResolvedEdges()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(edges, []Hop{{"pay", "ord", "kafka", "payments.done"}}) {
		t.Errorf("cross-domain hop: %+v", edges)
	}
	if b := BlastRadius(edges, "pay", "downstream", 5); !slices.Equal(b.Downstream.Affected, []string{"ord"}) {
		t.Errorf("blast: %+v", b.Downstream)
	}
	if _, err := OpenKnowledge(t.TempDir()); err == nil {
		t.Error("empty knowledge dir should error")
	}
}

func TestSymbolNames(t *testing.T) {
	k := testKB(t)
	if got, err := k.SymbolNames("valid", 10); err != nil || !slices.Equal(got, []string{"validateToken"}) {
		t.Errorf("prefix: %v %v", got, err)
	}
	for _, p := range []string{"%", "_alidate", "x"} { // SQL wildcards are literal
		if got, _ := k.SymbolNames(p, 10); len(got) != 0 {
			t.Errorf("%q matched %v", p, got)
		}
	}
}

// blast / context and versions.jsonl / graph.html must agree on the hops: one
// resolver (graph.Resolve), unknown ends dropped.
func TestOneResolver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kb.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE service_edges (src TEXT, dst TEXT, kind TEXT, channel TEXT, role TEXT, repo TEXT)",
		`INSERT INTO service_edges VALUES
			('a','','rest','/x','produce','a'),
			('','b','rest','/x','consume','b'),
			('a','','kafka','orphan','produce','a'),
			('c','d','grpc','Over','produce','c'),
			('','e','grpc','Over','consume','e')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	k, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	got, err := k.ResolvedEdges()
	if err != nil {
		t.Fatal(err)
	}
	// a->b matched; orphan has no consumer (no "?" hop); the override row's own
	// dst (d) is not invented as a consumer — the consumer is e
	want := []Hop{{"a", "b", "rest", "/x"}, {"c", "e", "grpc", "Over"}}
	if !slices.Equal(got, want) {
		t.Errorf("hops: %+v, want %+v", got, want)
	}
}

// `_` and `%` in a query match themselves, not any character.
func TestDefRefsEscapeWildcards(t *testing.T) {
	k := testKB(t)
	for _, q := range []string{"validate_oken", "valid%Token", "_"} {
		if d, err := k.FindDefinition(q, 20); err != nil || len(d) != 0 {
			t.Errorf("def %q: %v %v (wildcard matched)", q, d, err)
		}
		if r, err := k.FindReferences(q, 50); err != nil || len(r) != 0 {
			t.Errorf("refs %q: %v %v (wildcard matched)", q, r, err)
		}
	}
	if d, err := k.FindDefinition("TokenValidator#validateToken", 20); err != nil || len(d) != 1 {
		t.Errorf("plain substring still matches: %v %v", d, err)
	}
}
