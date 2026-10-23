package build

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"weaver/internal/search"
	"weaver/internal/snapshot"
)

func sh(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// bareRepo: a bare repo where each branch holds VERSION=<branch> plus files.
func bareRepo(t *testing.T, root, name string, files map[string]string, branches ...string) string {
	work, bare := filepath.Join(root, name+"-work"), filepath.Join(root, name+".git")
	sh(t, root, "git", "init", "-q", "-b", "main", work)
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(work, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range append([]string{"main"}, branches...) {
		if b != "main" {
			sh(t, work, "git", "checkout", "-q", "-B", b, "main")
		}
		if err := os.WriteFile(filepath.Join(work, "VERSION"), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
		sh(t, work, "git", "add", ".")
		sh(t, work, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", b)
	}
	sh(t, root, "git", "clone", "-q", "--bare", work, bare)
	return "file://" + bare
}

// Two domains over file:// remotes (no auth): payments' `pay` publishes a topic
// that orders' `ord` consumes. Checks the folder layout, configured branches,
// a missing branch skipped, stale cleanup, and that the CROSS-DOMAIN hop survives in both domains.
func TestRunKnowledgeLayout(t *testing.T) {
	td := t.TempDir()
	pay := bareRepo(t, td, "pay", map[string]string{"P.java": `class P { void f() { kafkaTemplate.send("payments.done", e); } }`},
		"release/1.9", "release/1.10")
	ord := bareRepo(t, td, "ord", map[string]string{"L.java": `class L { @KafkaListener(topics = "payments.done") void on() {} }`},
		"release/3.0", "hotfix/2.9")
	l := Layout{Root: filepath.Join(td, "knowledge"), SourcesRoot: filepath.Join(td, "sources")}
	var services []Service
	for d, yml := range map[string]string{
		"payments": "services:\n  - serviceName: pay\n    gitUrl: " + pay + "\n    branch: release/1.10\n",
		"orders": "services:\n  - serviceName: ord\n    gitUrl: " + ord + "\n    branch: hotfix/2.9\n" +
			"  - serviceName: nob\n    gitUrl: " + ord + "\n    branch: release/9.9\n",
	} {
		if err := os.MkdirAll(l.Sources(d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.SourcesFile(d), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range l.Configured() {
		s, err := LoadSources(l.SourcesFile(d), d)
		if err != nil {
			t.Fatal(err)
		}
		services = append(services, s...)
	}
	if err := os.MkdirAll(filepath.Join(l.Sources("payments"), "gone"), 0o755); err != nil { // stale clone
		t.Fatal(err)
	}

	// per-domain overrides: together a runtime-only REST hop pay -> ord, plus a
	// row naming another domain's service (rejected)
	for d, body := range map[string]string{
		"payments": "- src: pay\n  kind: rest\n  channel: /charge\n  role: produce\n" +
			"- src: ord\n  kind: rest\n  channel: /wrong\n  role: produce\n",
		"orders": "- dst: ord\n  kind: rest\n  channel: /charge\n  role: consume\n",
	} {
		if err := os.MkdirAll(l.Domain(d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.Overrides(d), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var log bytes.Buffer
	if err := Run(services, l, Options{Fetch: true, Log: &log}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(td, "sources", "payments", "pay", "VERSION")); string(b) != "release/1.10" {
		t.Errorf("sources/payments/pay at %q", b)
	}
	if _, err := os.Stat(filepath.Join(l.Domain("payments"), "sources")); !os.IsNotExist(err) {
		t.Error("sources must live outside the knowledge folder")
	}
	if _, err := os.Stat(filepath.Join(l.Sources("payments"), "gone")); !os.IsNotExist(err) {
		t.Error("stale clone kept")
	}
	if b, _ := os.ReadFile(filepath.Join(l.Sources("orders"), "ord", "VERSION")); string(b) != "hotfix/2.9" {
		t.Errorf("configured branch not used: sources/orders/ord at %q", b)
	}
	if !bytes.Contains(log.Bytes(), []byte("nob: branch release/9.9 not found")) {
		t.Errorf("missing configured branch not reported:\n%s", log.String())
	}
	for _, d := range []string{"payments", "orders"} {
		for _, f := range []string{l.KB(d), l.Zoekt(d), l.Versions(d), l.Graph(d), filepath.Join(l.Domain(d), "manifest.json")} {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("missing %s", f)
			}
		}
	}
	if got := l.Domains(); len(got) != 2 || got[0] != "orders" || got[1] != "payments" {
		t.Errorf("domains: %v", got)
	}

	// each domain's kb.db holds its own services
	db, err := sql.Open("sqlite", l.KB("orders"))
	if err != nil {
		t.Fatal(err)
	}
	var svc, branch string
	if err := db.QueryRow("SELECT service, branch FROM domains").Scan(&svc, &branch); err != nil || svc != "ord" || branch != "hotfix/2.9" {
		t.Errorf("orders domains row: %s %s %v", svc, branch, err)
	}
	db.Close()

	// both payments -> orders hops (extracted Kafka + override REST) are in BOTH
	// domains' graph versions
	for _, d := range []string{"payments", "orders"} {
		all, err := snapshot.LoadAll(l.Versions(d))
		if err != nil || len(all) != 1 {
			t.Fatalf("%s versions: %v %v", d, all, err)
		}
		got := map[string]bool{}
		for _, e := range all[0].Edges {
			if e.Src == "pay" && e.Dst == "ord" {
				got[e.Kind+" "+e.Channel] = true
			}
		}
		if len(all[0].Edges) != 2 || !got["kafka payments.done"] || !got["rest /charge"] {
			t.Errorf("%s edges: %+v", d, all[0].Edges)
		}
	}
	if !bytes.Contains(log.Bytes(), []byte(`payments override rest /wrong: service "ord" is not in domain payments`)) {
		t.Errorf("cross-domain override row not rejected:\n%s", log.String())
	}

	// search per domain, carrying the domain
	if hits, err := search.Search([]string{l.Zoekt("orders")}, "meta.domain:^orders$ KafkaListener", 10); err != nil || len(hits) != 1 || hits[0].Repo != "ord" {
		t.Errorf("orders search: %+v %v", hits, err)
	}

	// rebuild from existing sources: no git, a second version appended; a domain
	// without an override file gets the template, an existing one is kept
	if err := os.Remove(l.Overrides("payments")); err != nil {
		t.Fatal(err)
	}
	if err := Run(services, l, Options{Fetch: false, Log: &log}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(l.Overrides("payments")); !bytes.HasPrefix(b, []byte("# Service edges of this domain")) {
		t.Errorf("template not created: %q", b)
	}
	if b, _ := os.ReadFile(l.Overrides("orders")); !bytes.HasPrefix(b, []byte("- dst: ord")) {
		t.Errorf("existing overrides overwritten: %q", b)
	}
	if all, _ := snapshot.LoadAll(l.Versions("payments")); len(all) != 2 {
		t.Errorf("versions after --no-fetch rebuild: %d", len(all))
	}

	// rebuild ONLY payments: its hops into orders come from orders' existing
	// knowledge, which is read but not rewritten
	before, _ := os.ReadFile(l.Versions("orders"))
	paySvcs, err := LoadSources(l.SourcesFile("payments"), "payments")
	if err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := Run(paySvcs, l, Options{Fetch: false, Log: &log}); err != nil {
		t.Fatal(err)
	}
	all, _ := snapshot.LoadAll(l.Versions("payments"))
	// (payments' override file was reset to the template above, so the REST hop
	// is gone; the Kafka hop's consumer lives only in orders' existing knowledge)
	if last := all[len(all)-1]; len(last.Edges) != 1 || last.Edges[0].Dst != "ord" || last.Edges[0].Kind != "kafka" {
		t.Errorf("payments-only rebuild lost cross-domain hops: %+v\n%s", last.Edges, log.String())
	}
	if after, _ := os.ReadFile(l.Versions("orders")); string(after) != string(before) {
		t.Error("payments-only rebuild wrote orders' knowledge")
	}

	// the remote goes away: the fetch fails, the previous clone is kept and used,
	// and the new version still has the domain's edges (not an empty graph)
	if err := os.Rename(filepath.Join(td, "pay.git"), filepath.Join(td, "pay.gone")); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := Run(paySvcs, l, Options{Fetch: true, Log: &log}); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(l.Sources("payments"), "pay", "VERSION")); string(b) != "release/1.10" {
		t.Errorf("previous clone lost after a failed fetch: %q", b)
	}
	if !bytes.Contains(log.Bytes(), []byte("using the previous clone")) {
		t.Errorf("fallback not reported:\n%s", log.String())
	}
	all, _ = snapshot.LoadAll(l.Versions("payments"))
	if last := all[len(all)-1]; len(last.Edges) == 0 {
		t.Error("failed fetch recorded an empty graph version")
	}

	// a domain whose kb.db is missing doesn't block building another domain
	if err := os.Remove(l.KB("orders")); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := Run(paySvcs, l, Options{Fetch: false, Log: &log}); err != nil {
		t.Fatalf("a broken neighbour blocked the build: %v", err)
	}
	if !bytes.Contains(log.Bytes(), []byte("orders: existing knowledge unreadable")) {
		t.Errorf("broken neighbour not reported:\n%s", log.String())
	}
}

func TestLoadSourcesRejects(t *testing.T) {
	td := t.TempDir()
	for name, yml := range map[string]string{
		"missing gitUrl": "services:\n  - serviceName: x\n    branch: b\n",
		"missing branch": "services:\n  - serviceName: x\n    gitUrl: u\n",
		"duplicate":      "services:\n  - serviceName: x\n    gitUrl: u\n    branch: b\n  - serviceName: x\n    gitUrl: v\n    branch: b\n",
		"empty":          "services: []\n",
		"unsafe service": "services:\n  - serviceName: ../b\n    gitUrl: https://h/b\n    branch: b\n",
		"option url":     "services:\n  - serviceName: x\n    gitUrl: --upload-pack=touch /tmp/pwned\n    branch: b\n",
		"no scheme url":  "services:\n  - serviceName: x\n    gitUrl: example.com/x.git\n    branch: b\n",
		"option branch":  "services:\n  - serviceName: x\n    gitUrl: https://h/x\n    branch: --upload-pack=x\n",
	} {
		p := filepath.Join(td, "sources.yaml")
		if err := os.WriteFile(p, []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSources(p, "d"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := LoadSources(filepath.Join(td, "sources.yaml"), "../x"); err == nil {
		t.Error("unsafe domain: expected an error")
	}
}

// A second build on the same knowledge folder is refused while one runs.
func TestBuildLock(t *testing.T) {
	root := t.TempDir()
	unlock, err := lock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock(root); err == nil {
		t.Error("second lock should fail while the first is held")
	}
	unlock()
	again, err := lock(root)
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}
	again()
}

// --no-fetch builds from what is on disk, not from an old manifest.
func TestScanExistingSources(t *testing.T) {
	td := t.TempDir()
	src := filepath.Join(td, "sources", "pay")
	co := filepath.Join(src, "checkout")
	sh(t, td, "git", "init", "-q", "-b", "main", co)
	sh(t, co, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x")
	if err := os.MkdirAll(filepath.Join(src, "copy"), 0o755); err != nil { // plain copy, no .git
		t.Fatal(err)
	}
	var log bytes.Buffer
	m := Scan([]Service{
		{"pay", "checkout", "u1", "release/2.0"}, // on main, config says release/2.0
		{"pay", "copy", "u2", "release/1.0"},
		{"pay", "gone", "u3", "release/1.0"},
	}, src, &log)
	if len(m) != 2 {
		t.Fatalf("manifest: %+v\n%s", m, log.String())
	}
	if m[0].Branch != "main" || len(m[0].SHA) != 40 {
		t.Errorf("git checkout: %+v (branch and commit come from git)", m[0])
	}
	if m[1].Branch != "release/1.0" || m[1].SHA != "nogit" {
		t.Errorf("plain copy: %+v", m[1])
	}
	for _, want := range []string{"checkout: checked out on main, sources.yaml says release/2.0", "gone: not in"} {
		if !bytes.Contains(log.Bytes(), []byte(want)) {
			t.Errorf("log missing %q:\n%s", want, log.String())
		}
	}
}

// Nothing available for a domain (every fetch failed, no previous clone): the
// build stops and records nothing — no empty version, no manifest.
func TestRunNothingAvailable(t *testing.T) {
	td := t.TempDir()
	l := Layout{Root: filepath.Join(td, "knowledge"), SourcesRoot: filepath.Join(td, "sources")}
	svcs := []Service{{"pay", "a", "file://" + filepath.Join(td, "missing.git"), "main"}}
	var log bytes.Buffer
	err := Run(svcs, l, Options{Fetch: true, Log: &log})
	if err == nil || !strings.Contains(err.Error(), "none of its 1 services is available") {
		t.Fatalf("want a stop, got %v\n%s", err, log.String())
	}
	for _, f := range []string{l.Versions("pay"), filepath.Join(l.Domain("pay"), "manifest.json"), l.KB("pay")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s written for a domain with nothing to build", f)
		}
	}
}

// SCIP indexing is skipped for a service whose commit, working tree and build
// roots are unchanged; anything else re-indexes. A fake scip-go counts its runs
// (an empty file is a valid, empty SCIP index).
func TestScipCache(t *testing.T) {
	td := t.TempDir()
	bin, count := filepath.Join(td, "bin"), filepath.Join(td, "runs")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho x >> " + count + "\n[ -f FAIL ] && exit 1\n: > \"$2\"\n" // scip-go --output <file>
	if err := os.WriteFile(filepath.Join(bin, "scip-go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runs := func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "x")
	}

	l := Layout{Root: filepath.Join(td, "knowledge"), SourcesRoot: filepath.Join(td, "sources")}
	repo := filepath.Join(l.Sources("pay"), "svc")
	sh(t, td, "git", "init", "-q", "-b", "main", repo)
	commit := func(f, body string) {
		if err := os.WriteFile(filepath.Join(repo, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sh(t, repo, "git", "add", ".")
		sh(t, repo, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", f)
	}
	commit("go.mod", "module svc\n")
	svcs := []Service{{"pay", "svc", "file://unused", "main"}}
	var log bytes.Buffer
	build := func(o Options) {
		t.Helper()
		o.Log = &log
		log.Reset()
		if err := Run(svcs, l, o); err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
	}

	build(Options{})
	if runs() != 1 {
		t.Fatalf("first build: %d scip-go runs", runs())
	}
	build(Options{}) // nothing changed
	if runs() != 1 || !bytes.Contains(log.Bytes(), []byte("reusing its SCIP index")) {
		t.Errorf("unchanged commit re-indexed (%d runs)\n%s", runs(), log.String())
	}
	commit("main.go", "package main\n") // new commit
	build(Options{})
	if runs() != 2 {
		t.Errorf("new commit not re-indexed (%d runs)", runs())
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build(Options{}) // uncommitted edit
	if runs() != 3 {
		t.Errorf("dirty checkout not re-indexed (%d runs)", runs())
	}
	sh(t, repo, "git", "checkout", "-q", "--", "main.go")
	build(Options{Reindex: true})
	if runs() != 4 {
		t.Errorf("--reindex did not re-index (%d runs)", runs())
	}

	// a failure at a commit isn't retried every build — only with --reindex
	if err := os.WriteFile(filepath.Join(repo, "FAIL"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, repo, "git", "add", "FAIL")
	sh(t, repo, "git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "fail")
	build(Options{})
	build(Options{})
	if runs() != 5 || !bytes.Contains(log.Bytes(), []byte("failed at this commit last build")) {
		t.Errorf("failed root retried at the same commit (%d runs)\n%s", runs(), log.String())
	}
	build(Options{Reindex: true})
	if runs() != 6 {
		t.Errorf("--reindex did not retry the failed root (%d runs)", runs())
	}
}
