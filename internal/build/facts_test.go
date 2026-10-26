package build

import (
	"path/filepath"
	"reflect"
	"testing"

	"weaver/internal/edgemap"
	"weaver/internal/extract"
	"weaver/internal/facts"
	"weaver/internal/snapshot"
)

// What the edge mapper gets back from a service's facts is exactly what the
// analyzer reported: the facts file is the whole contract.
func TestFactsRoundTrip(t *testing.T) {
	rows := []extract.Edge{
		{Dst: "pay", Kind: "rest", Channel: "/charge", Role: "consume", Repo: "pay", Path: "C.java", Line: 4},
		{Src: "pay", Kind: "kafka", Channel: "payments.done", Role: "produce", Repo: "pay", Path: "P.java", Line: 9, Call: "async"},
		{Dst: "pay", Kind: "redis", Channel: "cart:{}", Role: "consume", Repo: "pay", Path: "R.go", Line: 2, Call: "optional", Match: "name"},
		{Src: "pay", Kind: "rest", Channel: "https://api.x/v1", Role: "produce", Repo: "pay", Path: "X.java", Line: 0, Host: "api.x"},
	}
	cg := []edgemap.CGEdge{{Caller: "pay/Svc#run(", Line: 12, Targets: []string{"pay/Impl#run("}}}
	gaps := []edgemap.Question{edgemap.GapQuestion("config", map[string]string{"service": "pay", "key": "cache.type", "kind": "condition"}, "pay: what is cache.type?", "the value", nil)}
	dir := t.TempDir()
	f := toFacts(snapshot.Manifest{ServiceName: "pay", Run: "run-1"}, "weave-java", rows, cg, map[string]string{"a.b": "c"}, gaps)
	if err := facts.Write(dir, f); err != nil {
		t.Fatal(err)
	}
	back, err := facts.Read(dir, "pay")
	if err != nil {
		t.Fatal(err)
	}
	r, c, g := fromFacts(back)
	if len(r) != len(rows) {
		t.Fatalf("rows: %+v", r)
	}
	want := map[string]extract.Edge{}
	for _, e := range rows {
		want[e.Kind+e.Channel] = e
	}
	for _, e := range r {
		if !reflect.DeepEqual(e, want[e.Kind+e.Channel]) {
			t.Errorf("row changed through facts:\n got %+v\nwant %+v", e, want[e.Kind+e.Channel])
		}
	}
	if !reflect.DeepEqual(c, cg) || len(g) != 1 || g[0].ID != gaps[0].ID || back.ConfigMap()["a.b"] != "c" {
		t.Errorf("callgraph %+v, gaps %+v, config %+v", c, g, back.Config)
	}
	if back.Entrypoints[0].Source != "syntax" || back.Calls[1].Source != "name" || filepath.Base(dir) == "" {
		t.Errorf("sources: %+v %+v", back.Entrypoints, back.Calls)
	}
}

// spring-probe's entry points become the service's facts, only for its own
// apps, as weaver's routes (path only), and only where the source found none.
func TestSpringProbeEntries(t *testing.T) {
	eps := []probeEntry{
		{App: "mall-admin", Kind: "rest", Resource: "POST /admin/login", Source: "static"},
		{App: "mall-admin", Kind: "event", Resource: "OrderPaid", Source: "static"},
		{App: "mall-portal", Kind: "rest", Resource: "GET /cart/list", Source: "aot"},
	}
	got := serviceEntries("", snapshot.Manifest{ServiceName: "mall-admin", Root: "mall-admin"}, eps)
	if len(got) != 1 || got[0].Resource != "/admin/login" || got[0].Source != "spring-probe:static" {
		t.Fatalf("mall-admin's own REST route only: %+v", got)
	}
	have := []facts.Entry{{Kind: "rest", Resource: "/admin/login", At: "A.java:3", Source: "syntax"}}
	if all := addEntries(have, append(got, facts.Entry{Kind: "rest", Resource: "/admin/info", Source: "spring-probe:static"})); len(all) != 2 || all[0].At != "A.java:3" {
		t.Errorf("a route found in the source is kept with its location; a new one is added: %+v", all)
	}
}
