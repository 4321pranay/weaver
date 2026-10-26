package extract

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	_ "modernc.org/sqlite"
)

func norm(kind, ch string, unq bool, m maps) string { return normalizeChannel(kind, ch, unq, m) }

func TestNormalizeChannel(t *testing.T) {
	lit := map[string]string{"baseUrl": "http://payment", "topicVar": "payments.authorized"}
	cases := []struct {
		name, kind, ch string
		unq            bool
		m              maps
		want           string
	}{
		{"var + literal concat", "rest", `baseUrl + "/v1/charge"`, true, maps{lits: lit}, "/v1/charge"},
		{"bare known var", "kafka", "topicVar", true, maps{lits: lit}, "payments.authorized"},
		{"redis key keeps its pattern", "redis", `"session:"+userID`, true, maps{}, "session:{}"},
		{"rest tail hole", "rest", `"http://ledger/v1/ledger/" + id`, true, maps{}, "/v1/ledger/{}"},
		{"cross-file dotted const", "kafka", "Constants.Topics.CART_EVENTS", true, maps{consts: map[string]string{"CART_EVENTS": "cart.events"}}, "cart.events"},
		{"String.format template", "rest", `String.format("/v1/users/%s/geo", x)`, true, maps{}, "/v1/users/{}/geo"},
		{"SpEL bean getter", "kafka", "#{@topicConfig.orders}", false, maps{consts: map[string]string{"orders": "orders.created"}}, "orders.created"},
		{"cloud stream binding", "kafka", "priceUpdates-out-0", false,
			maps{config: map[string]string{"spring.cloud.stream.bindings.priceUpdates-out-0.destination": "price.updates"}}, "price.updates"},
		{"ConfigurationProperties getter", "rest", `props.getLedgerUrl() + "/v1/audit"`, true, maps{getters: map[string]string{"getLedgerUrl": "http://ledger"}}, "/v1/audit"},
		{"viper struct field ref", "kafka", "cfg.InventoryTopic", true, maps{consts: map[string]string{"InventoryTopic": "inventory.events"}}, "inventory.events"},
		{"config placeholder", "kafka", "${topic.x}", false, maps{config: map[string]string{"topic.x": "x.events"}}, "x.events"},
		{"unknown placeholder", "kafka", "${nope.key}", false, maps{}, "<RUNTIME_PARAMETER: nope.key>"},
		{"SpEL T() constant", "kafka", "#{T(Topics).ORDER_PLACED}", false, maps{consts: map[string]string{"ORDER_PLACED": "order.placed"}}, "order.placed"},
		{"SpEL T() unknown: no guess", "kafka", "#{T(Topics).ORDER_PLACED}", false, maps{}, "<DYNAMIC_DISPATCH: #{T(Topics).ORDER_PLACED}>"},
		{"unresolved dotted const: no guess", "kafka", "Topics.ORDERS_CREATED", true, maps{}, "<DYNAMIC_DISPATCH: Topics.ORDERS_CREATED>"},
	}
	for _, c := range cases {
		if got := norm(c.kind, c.ch, c.unq, c.m); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// honest miss: a LEADING unknown var has no prefix worth keeping
	if got := norm("rest", `unknownVar + "/x"`, true, maps{lits: lit}); got != "<DYNAMIC_DISPATCH: unknownVar + \"/x\">" {
		t.Errorf("leading unknown: %q", got)
	}
}

func TestResolveConcatAndFormat(t *testing.T) {
	if got, _ := resolveConcat(`a + "/x/" + b`, map[string]string{"a": "http://h", "b": "42"}, nil); got != "http://h/x/42" {
		t.Errorf("multi-part: %q", got)
	}
	if _, ok := resolveConcat("onlyLiteral", nil, nil); ok {
		t.Error("non-concat should not resolve")
	}
	if got, _ := resolveConcat(`"session:" + userID`, nil, nil); got != "session:{}" {
		t.Errorf("tail hole: %q", got)
	}
	if got, _ := resolveFormat(`fmt.Sprintf("profile:%s", id)`); got != "profile:{}" {
		t.Errorf("Sprintf: %q", got)
	}
}

func TestCollectConstantsViperChain(t *testing.T) {
	d := t.TempDir()
	src := "package m\nfunc f(){ viper.SetDefault(\"kafka.inventory\",\"inventory.events\")\n" +
		"inventory := viper.GetString(\"kafka.inventory\")\nc.InventoryTopic = inventory }\n"
	if err := os.WriteFile(filepath.Join(d, "config.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CollectConstants(d).Vals["InventoryTopic"]; got != "inventory.events" {
		t.Errorf("viper chain: %q", got)
	}
}

func TestTreeSitterPasses(t *testing.T) {
	java := parse([]byte(`class C {
		@PostMapping(path="/api/v1/orders")
		void p(){}
		void a(){ restTemplate.exchange("http://payment/v1/charge", x); }
		void b(){ kafkaTemplate.send(System.getenv("ORDER_TOPIC"), e); }
		void c(){ kafkaTemplate.send(topicFor(ctx), m); }
	}`), "java")
	defer java.close()
	var sends []Arg
	for _, c := range java.calls() {
		if c.Method == "exchange" && noLine(c.Args[0]) != (Arg{Value: "http://payment/v1/charge", Unquoted: false}) {
			t.Errorf("exchange arg: %+v", c.Args[0])
		}
		if c.Method == "send" {
			sends = append(sends, noLine(c.Args[0]))
		}
	}
	if !slices.Contains(sends, Arg{Value: "${ORDER_TOPIC}", Unquoted: false}) || !slices.Contains(sends, Arg{Value: "topicFor(ctx)", Unquoted: true}) {
		t.Errorf("send args: %+v", sends)
	}
	anns := java.annotations()
	if len(anns) != 1 || noLine(anns[0].Kwargs["path"]) != (Arg{Value: "/api/v1/orders", Unquoted: false}) {
		t.Errorf("annotations: %+v", anns)
	}

	g := parse([]byte("package m\nfunc f(){ http.Get(baseURL + \"/v1/charge\"); w.SendMessage(ctx, os.Getenv(\"OUT_TOPIC\")) }"), "go")
	defer g.close()
	found := false
	for _, c := range g.calls() {
		found = found || (c.Method == "SendMessage" && len(c.Args) > 1 && noLine(c.Args[1]) == Arg{Value: "${OUT_TOPIC}", Unquoted: false})
	}
	if !found {
		t.Error("Go os.Getenv arg not resolved")
	}

	// a package-level topic list, not a struct field
	v := parse([]byte("package m\nvar consumerTopics = []string{\"payments.authorized\", \"orders.created\"}"), "go")
	defer v.close()
	tv := v.goTopicVars()
	if len(tv) != 1 || tv[0].Key != "consumerTopics" || !slices.Equal(tv[0].Vals, []string{"payments.authorized", "orders.created"}) {
		t.Errorf("topic vars: %+v", tv)
	}

	r := parse([]byte("package m\nfunc f(){ r.Route(\"/v1\", func(r chi.Router){ r.Get(\"/sessions\", h) }) }"), "go")
	defer r.close()
	if got := r.goChiRoutes(); !slices.Equal(got, []string{"/v1/sessions"}) {
		t.Errorf("chi routes: %v", got)
	}
}

// Producer + consumer on one topic across two repos, plus an override row, all
// through WriteEdges into a real SQLite table.
func TestExtractOverridesAndWrite(t *testing.T) {
	d := t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(d+"/repos/payment/P.java", `kafkaTemplate.send("payments.authorized", evt);`)
	write(d+"/repos/ledger/L.java", "@KafkaListener(topics = \"payments.authorized\")\nvoid on(){}")
	// an override row, as edgemap.Override.Rows hands it over
	ov := Edge{Src: "checkout", Kind: "rest", Channel: "/charge", Role: "produce", Repo: "checkout", Line: -1}
	edges := append((&Extractor{}).ExtractAll(d+"/repos"), ov)
	db, err := sql.Open("sqlite", d+"/kb.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := WriteEdges(db, edges); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT kind, channel, role FROM service_edges")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[[3]string]bool{}
	for rows.Next() {
		var k [3]string
		if err := rows.Scan(&k[0], &k[1], &k[2]); err != nil {
			t.Fatal(err)
		}
		got[k] = true
	}
	for _, want := range [][3]string{
		{"kafka", "payments.authorized", "produce"},
		{"kafka", "payments.authorized", "consume"},
		{"rest", "/charge", "produce"}, // override loaded verbatim
	} {
		if !got[want] {
			t.Errorf("missing %v in %v", want, got)
		}
	}
}

// The real-repo collision: two test files each with a LOCAL `String topic = ...`.
// Locals and test code are not constants; a real name clash is ambiguous, and a
// service's own constant beats another repo's.
func TestConstantsScope(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/src/main/java/A.java", `class A {
		static final String ORDERS = "a.orders";
		static final String SHARED = "a.shared";
		void f() { String local = "not.a.constant"; }
	}`)
	write("a/src/test/java/ATest.java", `class ATest { static final String ORDERS = "test.orders"; }`)
	write("b/B.java", `class B { public static final String SHARED = "b.shared"; static final String ONLY_B = "b.only"; }`)
	write("b/const.go", "package b\nconst Topic = \"b.topic\"\nfunc f() { inner := \"x\" }\n")
	write("c/C.java", `interface C { String SHARED = "c.shared"; }`)

	all := CollectConstants(root)
	if all.Vals["ORDERS"] != "a.orders" {
		t.Errorf("test file leaked into constants: %q", all.Vals["ORDERS"])
	}
	for _, k := range []string{"local", "inner"} {
		if _, ok := all.Vals[k]; ok {
			t.Errorf("local variable %q treated as a constant", k)
		}
	}
	if all.Vals["Topic"] != "b.topic" || all.Vals["ONLY_B"] != "b.only" {
		t.Errorf("Go const / plain static final missing: %v", all.Vals)
	}
	if _, ok := all.Vals["SHARED"]; ok || !all.Ambiguous["SHARED"] {
		t.Errorf("SHARED has 3 values: must be ambiguous, got %v", all.Vals["SHARED"])
	}
	// service a: its own SHARED wins over the ambiguous global one
	if a := CollectConstants(filepath.Join(root, "a")).Over(all); a["SHARED"] != "a.shared" || a["ONLY_B"] != "b.only" {
		t.Errorf("a's view: SHARED=%q ONLY_B=%q", a["SHARED"], a["ONLY_B"])
	}
}

func TestIsTest(t *testing.T) {
	for p, want := range map[[2]string]bool{
		{"svc/src/test/java/x", "A.java"}: true, {"svc/pkg", "a_test.go"}: true,
		{"svc/src/main/java", "OrderTests.java"}: true, {"svc/internal/testdata", "x.go"}: true,
		{"svc/src/main/java", "Order.java"}: false, {"svc/contest", "a.go"}: false,
	} {
		if got := IsTest(p[0], p[1]); got != want {
			t.Errorf("IsTest(%s, %s) = %v", p[0], p[1], got)
		}
	}
}

// Real YAML: comments, quotes and flow style work; every bad row fails loudly
// with its line instead of being dropped.
func noLine(a Arg) Arg { a.Line = 0; return a }

func TestSCIPCallGate(t *testing.T) {
	d := t.TempDir() + "/shop"
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "class Cart {\n  void save() {\n    user.set(\"name\");\n    redis.opsForValue().set(\"cart:1\", v);\n  }\n}\n"
	if err := os.WriteFile(d+"/Cart.java", []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	redis := func(edges []Edge) (out []string) {
		for _, e := range edges {
			if e.Kind == "redis" {
				out = append(out, e.Channel+"|"+e.Match)
			}
		}
		return out
	}
	// SCIP resolved only line 3 (0-based) to a Redis client
	gated := &Extractor{Calls: map[string]map[Site]map[string][]Hit{"shop": {{"Cart.java", 3}: {"set": {{Kind: "redis", Role: "produce", Arg: 0}}}}}}
	if got := redis(gated.ExtractRepo(d)); !slices.Equal(got, []string{"cart:1|"}) {
		t.Errorf("with SCIP: %v", got)
	}
	// no SCIP for the repo: names decide, and the edges say so
	if got := redis((&Extractor{}).ExtractRepo(d)); !slices.Equal(got, []string{"cart:1|name", "name|name"}) {
		t.Errorf("without SCIP: %v", got)
	}
}

// A monorepo service: only files under its root give its edges, under its own
// name, with paths still relative to the repo.
func TestExtractServiceRoot(t *testing.T) {
	d := t.TempDir() + "/shop"
	for p, src := range map[string]string{
		"app/cart/Cart.java":       "class Cart {\n  void save() {\n    redis.opsForValue().set(\"cart:1\", v);\n  }\n}\n",
		"app/catalog/Catalog.java": "class Catalog {\n  void save() {\n    redis.opsForValue().set(\"item:1\", v);\n  }\n}\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, p), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, e := range (&Extractor{}).ExtractService(d, "cart", "app/cart") {
		got = append(got, e.Repo+" "+e.Path+" "+e.Channel)
	}
	if !slices.Equal(got, []string{"cart app/cart/Cart.java cart:1"}) {
		t.Errorf("cart's edges: %v", got)
	}
}

// A controller's class-level @RequestMapping prefixes its methods' routes (and
// is no route itself); a Feign client's path prefixes its calls.
func TestJavaRoutePrefix(t *testing.T) {
	d := t.TempDir() + "/admin"
	files := map[string]string{
		"UmsAdminController.java": "@Controller\n@RequestMapping(\"/admin\")\npublic class UmsAdminController {\n" +
			"  @PostMapping(\"/login\")\n  public R login() { return null; }\n  @GetMapping(\"info\")\n  public R info() { return null; }\n}\n",
		"PortalClient.java": "@FeignClient(name = \"mall-portal\", path = \"/sso/\")\npublic interface PortalClient {\n" +
			"  @PostMapping(\"/login\")\n  R login();\n}\n",
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for p, src := range files {
		if err := os.WriteFile(filepath.Join(d, p), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, e := range (&Extractor{}).ExtractRepo(d) {
		if e.Kind == "rest" {
			got = append(got, e.Role+" "+e.Channel)
		}
	}
	slices.Sort(got)
	if want := []string{"consume /admin/info", "consume /admin/login", "produce /sso/login"}; !slices.Equal(got, want) {
		t.Errorf("routes: %v, want %v", got, want)
	}
}

// A service's config includes the environment of the Compose service built
// from its folder, wherever the compose file sits in the repo.
func TestServiceConfigCompose(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"order/src/main/resources/application.properties": "spring.kafka.bootstrap-servers=localhost:9092\nserver.port=8080\n",
		"docker/docker-compose.yml": "services:\n  order:\n    build: ../order\n    environment:\n      SPRING_KAFKA_BOOTSTRAP_SERVERS: kafka:9092\n" +
			"  shipping:\n    build:\n      context: ../shipping\n    environment:\n      - SPRING_DATASOURCE_URL=jdbc:postgresql://db/shipping\n",
	}
	for p, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "shipping"), 0o755); err != nil {
		t.Fatal(err)
	}
	order := ServiceConfig(repo, "order")
	if order["SPRING_KAFKA_BOOTSTRAP_SERVERS"] != "kafka:9092" || order["server.port"] != "8080" || order["SPRING_DATASOURCE_URL"] != "" {
		t.Errorf("order: %v", order)
	}
	if ship := ServiceConfig(repo, "shipping"); ship["SPRING_DATASOURCE_URL"] != "jdbc:postgresql://db/shipping" {
		t.Errorf("shipping (list form, build.context): %v", ship)
	}
	if got := EnvKeys("spring.kafka.bootstrap-servers"); !slices.Contains(got, "SPRING_KAFKA_BOOTSTRAP_SERVERS") || !slices.Contains(got, "SPRING_KAFKA_BOOTSTRAPSERVERS") {
		t.Errorf("env keys: %v", got)
	}
}
