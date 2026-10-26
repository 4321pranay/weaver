package edgemap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"weaver/internal/extract"
	"weaver/internal/snapshot"
)

func TestResource(t *testing.T) {
	for _, c := range []struct {
		kind, in, want string
		ok             bool
	}{
		{"rest", "http://ledger:8080/v1/users/123", "/v1/users/{}", true},
		{"rest", "/v1/users/:id/orders", "/v1/users/{}/orders", true},
		{"redis", "cart:{}", "cart:{}", true},
		{"kafka", "orders.<RUNTIME_PARAMETER: env>", "orders.{}", true},
		{"kafka", "orders.created [Condition: profile=dev]", "orders.created", true},
		{"kafka", "<DYNAMIC_DISPATCH: Topics.ORDERS>", "", false},
		{"kafka", "<RUNTIME_PARAMETER: topic>", "", false},
	} {
		got, ok := resource(c.kind, c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("resource(%s, %q) = %q %v, want %q %v", c.kind, c.in, got, ok, c.want, c.ok)
		}
	}
}

func override(t *testing.T, body string) (*Override, error) {
	p := filepath.Join(t.TempDir(), "edges.override.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadOverride(p)
}

func TestLoadOverrideErrors(t *testing.T) {
	// a file that can't be read as the schema fails
	for body, msg := range map[string]string{
		"- src: a\n":      "old format",
		"nodez: []\n":     `unknown key "nodez"`,
		"edges: {a: 1}\n": "expected a list",
	} {
		if _, err := override(t, body); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: want error containing %q, got %v", body, msg, err)
		}
	}
	// an invalid row is skipped and reported; the rest still applies (edges-spec §6)
	good := "  - {from: a, to: b, kind: rest, resource: /ok}\n"
	for body, msg := range map[string]string{
		"edges:\n  - {from: a, to: b, kind: amqp, resource: x}\n" + good:                `kind "amqp"`,
		"edges:\n  - {from: a, to: b, kind: kafka, resource: x, op: read}\n" + good:     `op "read" is not valid for kind kafka`,
		"edges:\n  - {from: a, to: b, kind: rest, resource: x, call: unknown}\n" + good: "generated only",
		"edges:\n  - {from: a, to: b, kind: rest, resource: x, callers: []}\n" + good:   "callers is generated only",
		"edges:\n  - {from: a, kind: rest, resource: x}\n" + good:                       "needs from, to, kind and resource",
		"edges:\n  - {from: a, to: unknown/q-1, kind: rest, resource: x}\n" + good:      "placeholder",
		"nodes:\n  - {id: a, aliases: [x]}\n  - {id: b, aliases: [x]}\n":                `"x" names both a`,
	} {
		o, err := override(t, body)
		if err != nil || len(o.Skipped) != 1 || !strings.Contains(o.Skipped[0], msg) {
			t.Errorf("%q: want one skipped row containing %q, got %v %v", body, msg, o, err)
			continue
		}
		if strings.Contains(body, "/ok") && len(o.Edges) != 1 {
			t.Errorf("%q: the valid row still applies: %+v", body, o.Edges)
		}
	}
	if o, err := override(t, "# only comments\n"); err != nil || len(o.Edges)+len(o.Nodes) != 0 {
		t.Errorf("comment-only: %+v %v", o, err)
	}
}

func TestBuildMergesOverride(t *testing.T) {

	raw := []extract.Edge{
		{Src: "checkout", Kind: "rest", Channel: "/pay", Role: "produce", Repo: "checkout", Path: "C.java", Line: 4, Call: "required"},
		{Dst: "payment", Kind: "rest", Channel: "/pay", Role: "consume", Repo: "payment", Path: "P.java", Line: 2},
		{Src: "checkout", Kind: "rest", Channel: "/ping", Role: "produce", Repo: "checkout", Path: "C.java", Line: 9, Call: "optional"},
		{Dst: "legacy", Kind: "rest", Channel: "/ping", Role: "consume", Repo: "legacy", Path: "L.java", Line: 1},
		{Src: "checkout", Kind: "kafka", Channel: "orders.created", Role: "produce", Repo: "checkout", Path: "C.java", Line: 12, Call: "async"},
		{Src: "checkout", Kind: "rest", Channel: "https://api.stripe.com/v1/charges", Host: "api.stripe.com", Role: "produce", Repo: "checkout", Path: "S.java", Line: 3},
		{Src: "checkout", Kind: "kafka", Channel: "<DYNAMIC_DISPATCH: topicFor(x)>", Role: "produce", Repo: "checkout", Path: "C.java", Line: 20},
	}
	in := Input{Domain: "shop", DomainOf: map[string]string{"checkout": "shop", "payment": "shop", "legacy": "shop"}, Raw: raw,
		Config: func(string) map[string]string { return map[string]string{} }}

	// without an override: kafka has no address -> asked, and the edges are still
	// written, to the placeholder unknown/<id>, call: unknown, waiting on it
	m, qs, _, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if hasEdge(m, "checkout", "kafka/") || len(qs) != 2 {
		t.Errorf("no kafka node without an address, and 2 questions (address, topic): %+v", qs)
	}
	waiting := 0
	for _, e := range m.Edges {
		if e.From == "checkout" && e.Kind == "kafka" {
			waiting++
			if !strings.HasPrefix(e.To, "unknown/") || e.Call != "unknown" || e.Question == "" || e.To != "unknown/"+e.Question {
				t.Errorf("a kafka edge waiting on its address question: %+v", e)
			}
		}
	}
	if waiting != 2 {
		t.Errorf("both kafka call sites stay in the map (topic orders.created and the unresolved one): %d", waiting)
	}
	if !hasEdge(m, "checkout", "external/api.stripe.com") {
		t.Errorf("call to an unknown host must go to an external node: %+v", m.Edges)
	}

	// with one: the address answered by alias, a call corrected, an edge ignored, one added
	o, err := override(t, `version: weaver/edges.override/v1.0
nodes:
  - id: kafka/k1
    type: kafka
    props: {brokers: [k1:9092]}
    aliases: [kafka@checkout]
edges:
  - {from: checkout, to: payment, kind: rest, resource: /pay, call: fallback}
  - {from: checkout, to: legacy, kind: rest, resource: /ping, call: ignore}
  - {from: payment, to: kafka/k1, kind: kafka, resource: payments.done, op: produce}
`)
	if err != nil {
		t.Fatal(err)
	}
	in.Override = o
	m, _, warns, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Edges {
		switch {
		case e.To == "payment" && (e.Call != "fallback" || e.Callers[0].Call != "fallback"):
			t.Errorf("call not overridden: %+v", e)
		case e.To == "legacy":
			t.Errorf("ignored edge kept: %+v", e)
		case e.To == "kafka/k1" && e.From == "checkout" && e.Op != "produce":
			t.Errorf("kafka produce edge: %+v", e)
		}
	}
	if !hasEdge(m, "checkout", "kafka/k1") || !hasEdge(m, "payment", "kafka/k1") {
		t.Errorf("answered kafka edges missing: %+v", m.Edges)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "payment → kafka/k1") {
		t.Errorf("an added edge with an op is warned about once: %v", warns)
	}

	// an op that contradicts the direction is refused
	in.Override, _ = override(t, "nodes:\n  - {id: kafka/k1, type: kafka, aliases: [kafka@checkout]}\nedges:\n  - {from: checkout, to: kafka/k1, kind: kafka, resource: orders.created, op: consume}\n")
	if _, _, _, err := Build(in); err == nil || !strings.Contains(err.Error(), "contradicts its direction") {
		t.Errorf("op/direction: %v", err)
	}
}

func hasEdge(m Map, from, toPrefix string) bool {
	for _, e := range m.Edges {
		if e.From == from && strings.HasPrefix(e.To, toPrefix) {
			return true
		}
	}
	return false
}

func TestRecordReleases(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "src", "checkout")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	pom := func(v string) {
		body := "<project><parent><version>9.9</version></parent><artifactId>checkout</artifactId><version>" + v +
			"</version><dependencies><dependency><version>7.7</version></dependency></dependencies></project>"
		if err := os.WriteFile(filepath.Join(svc, "pom.xml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wout, hist := filepath.Join(dir, "releases.yaml"), filepath.Join(dir, "edges.history")
	src := func(string) string { return svc }
	edge := func(res string) Map {
		return Map{Edges: []Edge{{From: "checkout", To: "payment", Kind: "rest", Resource: res, Call: "required"}}}
	}
	build := func(sha, version string, m Map) ReleaseFile {
		pom(version)
		if _, err := Record(wout, hist, []snapshot.Manifest{{ServiceName: "checkout", GitURL: "u", Branch: "release", SHA: sha}}, src, m, time.Now()); err != nil {
			t.Fatal(err)
		}
		var w ReleaseFile
		b, _ := os.ReadFile(wout)
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		return w
	}

	w := build("aaaa1111aaaa1111", "1.3.0", edge("/pay"))
	if r := w.Services[0].Releases; len(r) != 1 || r[0].Version != "1.3.0" || r[0].Edges != "1.3.0" {
		t.Fatalf("first release (project version, not parent's or a dependency's): %+v", r)
	}
	if w = build("aaaa1111aaaa1111", "1.3.0", edge("/pay")); len(w.Services[0].Releases) != 1 {
		t.Errorf("same commit must not add a release: %+v", w.Services[0].Releases)
	}
	w = build("bbbb2222bbbb2222", "1.3.2", edge("/pay")) // new release, same edges
	if r := w.Services[0].Releases; len(r) != 2 || r[0].Version != "1.3.2" || r[0].Edges != "1.3.0" {
		t.Errorf("unchanged edges must point at the older snapshot: %+v", r)
	}
	w = build("cccc3333cccc3333", "1.4.0", edge("/v2/pay")) // edges changed
	if r := w.Services[0].Releases; r[0].Edges != "1.4.0" {
		t.Errorf("changed edges need a new snapshot: %+v", r)
	}
	if r := w.Services[0].Releases; r[len(r)-1].GitURL != "u" || r[0].GitURL != "" {
		t.Errorf("gitUrl only on the oldest release while the repo doesn't move: %+v", r)
	}
	if files, _ := filepath.Glob(filepath.Join(hist, "checkout", "*.yaml")); len(files) != 2 {
		t.Errorf("snapshots: %v", files)
	}
	// a new commit with the same version updates that release, it doesn't add one
	w = build("dddd4444dddd4444", "1.4.0", edge("/v2/pay"))
	if r := w.Services[0].Releases; len(r) != 3 || r[0].Version != "1.4.0" || r[0].CommitHash != "dddd4444dddd4444" {
		t.Errorf("same version, new commit: one release, latest commit: %+v", r)
	}
	// no build file: version.json (Go and anything else); never a git tag
	os.Remove(filepath.Join(svc, "pom.xml"))
	if err := os.WriteFile(filepath.Join(svc, "version.json"), []byte(`{"version": "2.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := ReleaseVersion(svc); v != "2.0.0" {
		t.Errorf("version.json: %q", v)
	}
}

func TestQuestionsLifecycle(t *testing.T) {
	dir := t.TempDir()
	qp, op := filepath.Join(dir, "questions.yaml"), filepath.Join(dir, "edges.override.yaml")
	addr := Question{Type: "address", Subject: map[string]string{"service": "order", "nodeType": "kafka"}, Ask: "order: which kafka?", Pending: 2}
	addr.ID = qid(addr)
	now := time.Now()

	q, _ := LoadQuestions(qp)
	if r := q.Record("run-a", 0, now, 3, []Question{addr}); r.New != 1 || r.Pending != 2 {
		t.Fatalf("first run: %+v", r)
	}
	if err := q.Write(qp); err != nil {
		t.Fatal(err)
	}
	// a wrong-shaped answer is refused and changes nothing
	if _, err := Answer(qp, op, addr.ID, "orders-cluster", "pranay", now); err == nil {
		t.Error("an address answer must be a kafka/<name> node id")
	}
	it, err := Answer(qp, op, addr.ID, "kafka/food", "pranay", now)
	if err != nil || it.Status != "answered" || it.Answer.By != "pranay" {
		t.Fatalf("answer: %+v %v", it, err)
	}
	o, err := LoadOverride(op)
	if err != nil || len(o.Nodes) != 1 || o.Nodes[0].ID != "kafka/food" || o.Nodes[0].Aliases[0] != "kafka@order" {
		t.Fatalf("override after answer: %+v %v", o, err)
	}
	// the next run no longer raises it: resolved, answer and who kept
	q, _ = LoadQuestions(qp)
	if r := q.Record("run-b", 0, now, 3, nil); r.Resolved != 1 {
		t.Errorf("second run: %+v", r)
	}
	if it := q.Questions[0]; it.Status != "resolved" || it.Answer.By != "pranay" || it.FirstRun != "run-a" || len(q.Runs) != 2 {
		t.Errorf("after resolve: %+v runs=%d", it, len(q.Runs))
	}

	// the run id depends only on what was built
	a := []snapshot.Manifest{{GitURL: "g/a", SHA: "1"}, {GitURL: "g/b", SHA: "2"}}
	b := []snapshot.Manifest{{GitURL: "g/b", SHA: "2"}, {GitURL: "g/a", SHA: "1"}}
	c := []snapshot.Manifest{{GitURL: "g/a", SHA: "1"}, {GitURL: "g/b", SHA: "3"}}
	if RunID(a) != RunID(b) || RunID(a) == RunID(c) {
		t.Errorf("run ids: %s %s %s", RunID(a), RunID(b), RunID(c))
	}
}

func TestCheckFormat(t *testing.T) {
	for got, ok := range map[string]bool{
		"weaver/edges.override/v1.0": true,
		"weaver/edges.override/v0.9": false, // older major
		"weaver/edges.override/v1.3": false, // newer minor than this weaver reads
		"weaver/questions/v1.0":      false, // another file's format
		"weaver/v1.0":                false,
		"1":                          false,
	} {
		if err := CheckFormat(got, "weaver/edges.override/v1.0"); (err == nil) != ok {
			t.Errorf("%s: %v", got, err)
		}
	}
}

func TestDescriptorKey(t *testing.T) {
	for sym, want := range map[string]string{
		"scip-java maven maven/com.shop/erp 1.0 com/shop/OrderServiceImpl#create(+1).":       "com/shop/OrderServiceImpl#create(",
		"scip-go gomod github.com/x/y v1 `github.com/x/y/internal/repo`/orderRepo#Create().": "`github.com/x/y/internal/repo`/orderRepo#Create(",
		"scip-java maven maven/com.shop/erp 1.0 com/shop/OrderServiceImpl#":                  "",
	} {
		if got := descriptorKey(sym); got != want {
			t.Errorf("%s: %q, want %q", sym, got, want)
		}
	}
	// a Java call site (caller class + line) becomes a site in the class's file
	s := newSCIP(nil, map[string][]CGEdge{"erp": {{Caller: "com/shop/OrderController#create(", Line: 66,
		Targets: []string{"com/shop/OrderServiceImpl#create("}}}})
	s.cgIdx["erp"] = map[string][]site{"com/shop/OrderServiceImpl#create(": {{repo: "erp", path: "OrderController.java", line: 65}}}
	if got := s.cgSites("erp", "scip-java maven maven/com.shop/erp 1.0 com/shop/OrderServiceImpl#create()."); len(got) != 1 || got[0].line != 65 {
		t.Errorf("cgSites: %+v", got)
	}
}

// A Kafka node is named after its cluster, asked once; brokers are aliases, so
// a service configured with only some of them finds the same node.
func TestKafkaClusterName(t *testing.T) {
	cfg := map[string]map[string]string{
		"order":   {"spring.kafka.bootstrap-servers": "k1:9092,k2:9092"},
		"invoice": {"spring.kafka.bootstrap-servers": "k2:9092"},
	}
	in := Input{Domain: "shop", DomainOf: map[string]string{"order": "shop", "invoice": "shop"},
		Raw: []extract.Edge{
			{Src: "order", Kind: "kafka", Channel: "orders", Role: "produce", Repo: "order", Path: "O.java", Line: 3},
			{Dst: "invoice", Kind: "kafka", Channel: "orders", Role: "consume", Repo: "invoice", Path: "I.java", Line: 5},
		},
		Config: func(svc string) map[string]string { return cfg[svc] }}
	m, qs, _, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Subject["brokers"] != "k1:9092,k2:9092" || qs[0].Pending != 2 {
		t.Fatalf("one cluster question for overlapping broker sets, both call sites waiting: %+v", qs)
	}
	if hasEdge(m, "order", "kafka/") {
		t.Errorf("no id is made up from broker hosts: %+v", m.Edges)
	}
	o, err := override(t, "version: weaver/edges.override/v1.0\nnodes:\n  - {id: kafka/orders, type: kafka, props: {brokers: [k1:9092, k2:9092]}, aliases: [k1:9092, k2:9092]}\n")
	if err != nil {
		t.Fatal(err)
	}
	in.Override = o
	m, qs, _, _ = Build(in)
	if len(qs) != 0 || !hasEdge(m, "order", "kafka/orders") || !hasEdge(m, "kafka/orders", "invoice") {
		t.Errorf("answered: both services resolve to kafka/orders, the partial broker list included: %+v %+v", qs, m.Edges)
	}
}
