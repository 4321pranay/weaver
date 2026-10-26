package scip

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	scipb "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// SCIP is stored by document: one file indexed for two services is stored
// once, and a rebuild keeps the previous kb.db's other releases, not current.
func TestDocumentStore(t *testing.T) {
	td := t.TempDir()
	index := func(name, sym string) string {
		idx := &scipb.Index{Documents: []*scipb.Document{{RelativePath: "A.java",
			Occurrences: []*scipb.Occurrence{{Range: []int32{3, 4, 9}, Symbol: sym, SymbolRoles: 1}}}}}
		b, err := proto.Marshal(idx)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(td, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	v1, v2 := index("v1.scip", "scip x a/A#run()."), index("v2.scip", "scip x a/A#start().")
	open := func(name string) *sql.DB {
		db, err := sql.Open("sqlite", filepath.Join(td, name))
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	count := func(db *sql.DB, q string) int {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// build 1: two services share the file (a monorepo)
	kb1 := open("kb1.db")
	if _, err := Ingest(kb1, []Part{{Scip: v1, Repo: "cart", Release: "c1"}, {Scip: v1, Repo: "catalog", Release: "c1"}}); err != nil {
		t.Fatal(err)
	}
	if n := count(kb1, "SELECT count(*) FROM docs"); n != 1 {
		t.Errorf("one document for the shared file, got %d", n)
	}
	if n := count(kb1, "SELECT count(*) FROM occurrences"); n != 2 {
		t.Errorf("both services see it, got %d occurrences", n)
	}
	kb1.Close()
	// build 2: cart at a new commit; catalog unchanged and not rebuilt here
	kb2 := open("kb2.db")
	if err := CarryForward(kb2, filepath.Join(td, "kb1.db"), map[string]string{"cart": "c2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(kb2, []Part{{Scip: v2, Repo: "cart", Release: "c2"}}); err != nil {
		t.Fatal(err)
	}
	if n := count(kb2, "SELECT count(*) FROM release_docs WHERE repo = 'cart' AND release = 'c1' AND current = 0"); n != 1 {
		t.Errorf("cart's old release kept, not current: %d", n)
	}
	if n := count(kb2, "SELECT count(*) FROM occurrences WHERE repo = 'cart' AND symbol = 'scip x a/A#start().'"); n != 1 {
		t.Errorf("queries read cart's current release: %d", n)
	}
	if n := count(kb2, "SELECT count(*) FROM occurrences WHERE repo = 'cart' AND symbol = 'scip x a/A#run().'"); n != 0 {
		t.Errorf("an old release isn't current: %d", n)
	}
	if n := count(kb2, "SELECT count(*) FROM docs"); n != 2 {
		t.Errorf("old document carried, new one added: %d", n)
	}
}
