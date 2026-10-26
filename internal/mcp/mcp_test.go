package mcp

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"weaver/internal/build"
)

func fixture(t *testing.T) *Server {
	root := t.TempDir()
	l := build.Layout{Root: root}
	gen := l.Gen("shop", 3)
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	m := `version: weaver/edges/v1.0
nodes:
  - {id: web, type: service}
  - {id: checkout, type: service}
  - {id: payment, type: service}
  - {id: ledger, type: service}
  - {id: kafka/orders, type: kafka}
edges:
  - {from: web, to: checkout, kind: rest, resource: POST /order, call: required}
  - {from: checkout, to: payment, kind: rest, resource: POST /pay, call: required}
  - {from: ledger, to: payment, kind: rest, resource: GET /balance, call: optional}
  - {from: payment, to: kafka/orders, kind: kafka, resource: paid, op: produce, call: async}
  - {from: kafka/orders, to: ledger, kind: kafka, resource: paid, op: consume}
`
	for f, body := range map[string]string{"edges.yaml": m, "build.yaml": "build: 3\nrun: run-x\nbuiltAt: \"t\"\n"} {
		if err := os.WriteFile(filepath.Join(gen, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("generations", "3"), l.Current("shop")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Domain("shop"), "manifest.json"), []byte(`[{"serviceName":"web"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Server{L: l}
}

func TestTools(t *testing.T) {
	s := fixture(t)
	res, err := s.Call("blast", args{"domain": "shop", "node": "payment"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(plain(res))
	// checkout needs payment (required), web needs checkout; ledger's call is optional
	if got := string(b); !strings.Contains(got, `"build":3`) || !strings.Contains(got, `"node":"web"`) || strings.Contains(got, `"node":"ledger"`) {
		t.Errorf("blast follows required hops only: %s", got)
	}
	res, _ = s.Call("blast", args{"domain": "shop", "node": "kafka/orders", "all": true})
	if b, _ := json.Marshal(plain(res)); !strings.Contains(string(b), `"node":"ledger"`) {
		t.Errorf("a consumer depends on the broker it reads from: %s", b)
	}
	if _, err := s.Call("edge", args{"domain": "shop", "ref": "checkout -> payment rest POST /pay"}); err != nil {
		t.Errorf("edge by ref: %v", err)
	}
	if _, err := s.Call("edge", args{"domain": "shop"}); err == nil {
		t.Error("a missing parameter is refused")
	}
}

func TestServe(t *testing.T) {
	s := fixture(t)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"edges","arguments":{"domain":"shop","node":"checkout","direction":"out"}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := s.Serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("one answer per request, none for the notification: %s", out.String())
	}
	if !strings.Contains(lines[1], `"name":"blast"`) || !strings.Contains(lines[2], `\"from\":\"checkout\"`) {
		t.Errorf("tools/list and a call with the files' field names:\n%s", out.String())
	}
}

func TestCallStructure(t *testing.T) {
	s := fixture(t)
	db, err := sql.Open("sqlite", s.L.KB("shop"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE occurrences (repo TEXT, path TEXT, symbol TEXT, role INTEGER,
		start_line INTEGER, start_char INTEGER, end_line INTEGER, end_char INTEGER, enc_start_line INTEGER, enc_end_line INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]any{
		{"pay", "A.java", "s a/A#run().", 1, 10, 10, 20},    // run() spans 10-20
		{"pay", "A.java", "s b/B#call().", 0, 12, nil, nil}, // run() calls call()
		{"pay", "B.java", "s b/B#call().", 1, 1, 1, 5},
		{"pay", "C.java", "s c/C#main().", 1, 25, 25, 40},
		{"pay", "C.java", "s a/A#run().", 0, 30, nil, nil}, // main() calls run()
	} {
		if _, err := db.Exec("INSERT INTO occurrences VALUES (?,?,?,?,?,0,?,5,?,?)", r[0], r[1], r[2], r[3], r[4], r[4], r[5], r[6]); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	out := func(name string, a args) string {
		res, err := s.Call(name, a)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := json.Marshal(plain(res))
		return string(b)
	}
	if got := out("enclosing_method", args{"domain": "shop", "service": "pay", "at": "A.java:13"}); !strings.Contains(got, "a/A#run()") {
		t.Errorf("enclosing: %s", got)
	}
	if got := out("call_chain", args{"domain": "shop", "symbol": "s a/A#run().", "direction": "up"}); !strings.Contains(got, "c/C#main()") || !strings.Contains(got, "C.java:31") {
		t.Errorf("up: %s", got)
	}
	if got := out("call_chain", args{"domain": "shop", "symbol": "s a/A#run().", "direction": "down"}); !strings.Contains(got, "b/B#call()") {
		t.Errorf("down: %s", got)
	}
}
