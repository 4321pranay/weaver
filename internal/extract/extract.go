// Package extract is the static service-edge extractor for Go + Java/Spring.
//
// It finds the runtime wiring SCIP can't see — REST routes/clients, Kafka
// produce/consume, Redis keys, gRPC stubs, DB tables — as raw produce/consume
// rows. The repo directory name is the service name.
//
// This is a HEURISTIC. Runtime-computed topics/URLs and dynamic dispatch are
// invisible statically: those become <DYNAMIC_DISPATCH>/<RUNTIME_PARAMETER>
// markers, or get listed by hand in edges.override.yaml.
package extract

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Edge is one raw extraction. produce: Src=service; consume: Dst=service.
type Edge struct {
	Src, Dst, Kind, Channel, Role, Repo string
	// call site, for the edges map: repo-relative path, 0-based line (-1 when
	// the edge has no single line), and the produce call's criticality (§5.1)
	Path string
	Line int
	Call string
	Host string // a REST call's target host, when its URL names one
	// Match "name": a call recognised by its method name only, in a repo with
	// no SCIP index (weaker than a SCIP-resolved call). "" = SCIP-resolved or
	// an annotation / route.
	Match string
}

// Site is a 0-based source line in a repo.
type Site struct {
	Path string
	Line int
}

// SymbolValues: the string value of the constant referenced as ref (as written:
// `ORDERS`, `Topics.ORDERS`) at 0-based line/col in service's file path.
type SymbolValues interface {
	Lookup(service, path string, line, col int, ref string) (string, bool)
}

// Wrapper is a SCIP-resolved custom method that hides a transport sink.
type Wrapper struct{ Kind, Role string }

// Hit: what SCIP says a call site is (scip.ClientCalls). Resources, when set,
// are the exact resources (generated gRPC code names them); else the resource is
// argument Arg at the call site, -1 meaning its first literal argument.
type Hit struct {
	Kind, Role string
	Arg        int
	Resources  []string
}

// Extractor holds what is shared across repos in one run.
type Extractor struct {
	// Wrappers: service -> method name -> sink, only for wrapper methods SCIP saw
	// that service reference (scip.WrappersUsed); nil = source-only.
	Wrappers map[string]map[string]Wrapper
	// Consts: cross-file constants over every source tree in the build
	// (CollectConstants). nil = the repo's parent dir, i.e. its sibling services.
	Consts *Consts
	// Symbols resolves a constant reference exactly from SCIP: the symbol at
	// that file/line/column, its value read at its definition. Name-based Consts
	// stay the fallback (repos SCIP couldn't index). nil = names only.
	Symbols SymbolValues
	// Calls: repo -> (path, line) -> method name -> what SCIP resolved that call
	// to (scip.ClientCalls). A repo in here takes its calls from it alone; a
	// repo missing from it falls back to method names, marked Match "name".
	Calls map[string]map[Site]map[string][]Hit

	mu     sync.Mutex
	consts map[string]Consts // dir -> CollectConstants(dir), memoized
}

var annKind = map[string]Wrapper{
	"KafkaListener":  {"kafka", "consume"},
	"GetMapping":     {"rest", "consume"},
	"PostMapping":    {"rest", "consume"},
	"PutMapping":     {"rest", "consume"},
	"DeleteMapping":  {"rest", "consume"},
	"RequestMapping": {"rest", "consume"},
}

var (
	reDBProduce  = regexp.MustCompile(`(?i)(?:UPDATE|INSERT\s+INTO|DELETE\s+FROM)\s+([a-zA-Z0-9_]+)`)
	reDBConsume  = regexp.MustCompile(`(?i)FROM\s+([a-zA-Z0-9_]+)`)
	reProfile    = regexp.MustCompile(`@Profile\("([^"]+)"\)`)
	reValueVar   = regexp.MustCompile(`@Value\("\$\{([^}]+)\}"\)[^;]+String\s+(\w+)`)
	reGetenvVar  = regexp.MustCompile(`(\w+)\s*:=\s*os\.Getenv\("([^"]+)"\)`)
	reJavaLit    = regexp.MustCompile(`\bString\s+(\w+)\s*=\s*"([^"]+)"`)
	reGoLit      = regexp.MustCompile(`\b(\w+)\s*:?=\s*"([^"]+)"`)
	reFormatVar  = regexp.MustCompile(`\b(\w+)\s*:?=\s*((?:fmt\.Sprintf|String\.format)\([^)]*\))`)
	reLineCmt    = regexp.MustCompile(`(?m)//.*$`)
	reWriter     = regexp.MustCompile(`\b(?:NewWriter|WriterConfig)\b`)
	reTopicField = regexp.MustCompile(`\bTopic\s*:\s*(\w+\.\w+)\b`)
)

func (x *Extractor) collect(dir string) Consts {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.consts == nil {
		x.consts = map[string]Consts{}
	}
	if c, ok := x.consts[dir]; ok {
		return c
	}
	c := CollectConstants(dir)
	x.consts[dir] = c
	return c
}

// ExtractRepo extracts one service (test code skipped). Cross-file constants:
// the repo's own, then x.Consts or else the repo's PARENT dir, so sibling services
// (a shared Constants module) resolve too.
func (x *Extractor) ExtractRepo(repoDir string) []Edge {
	return x.ExtractService(repoDir, filepath.Base(strings.TrimRight(repoDir, "/")), "")
}

// ExtractService extracts the service whose code is root in repoDir ("" = the
// whole repo): a monorepo lists one service per root. Only files under root
// give edges, and its config is root's; constants come from the whole repo, and
// paths stay relative to repoDir, as SCIP's are.
func (x *Extractor) ExtractService(repoDir, service, root string) []Edge {
	repoDir = strings.TrimRight(repoDir, "/")
	config := ServiceConfig(repoDir, root)
	// constants: this repo's own first, then the wider table (all sources in the
	// build, or the sibling services); ambiguous names resolve to nothing
	global := x.Consts
	if global == nil {
		parent := filepath.Dir(repoDir)
		if st, err := os.Stat(parent); err != nil || !st.IsDir() {
			parent = repoDir
		}
		g := x.collect(parent)
		global = &g
	}
	consts := x.collect(repoDir).Over(*global)
	getters := collectGetterConfigs(filepath.Join(repoDir, root), config)
	var edges []Edge

	activeProfile := config["spring.profiles.active"]
	if !has(config, "spring.profiles.active") {
		activeProfile = "default"
	}

	for _, sf := range iterSrc(repoDir) {
		if root != "" && !strings.HasPrefix(sf.rel, root+"/") {
			continue
		}
		text := sf.text
		profileTag := ""
		if m := reProfile.FindStringSubmatch(text); m != nil && m[1] != activeProfile {
			profileTag = " [Condition: profile=" + m[1] + "]"
		}

		// variable -> config key (@Value / os.Getenv)
		vars := map[string]string{}
		for _, m := range reValueVar.FindAllStringSubmatch(text, -1) {
			vars[m[2]] = m[1]
		}
		for _, m := range reGetenvVar.FindAllStringSubmatch(text, -1) {
			vars[m[1]] = m[2]
		}
		// variable -> literal value, same file (SAVAT's data-flow idea, source-level).
		// ponytail: same-file, string literals only; no cross-branch DFA.
		lits := map[string]string{}
		for _, m := range reJavaLit.FindAllStringSubmatch(text, -1) {
			lits[m[1]] = m[2]
		}
		for _, m := range reGoLit.FindAllStringSubmatch(text, -1) {
			setDefault(lits, m[1], m[2])
		}
		for _, m := range reFormatVar.FindAllStringSubmatch(text, -1) {
			if f, ok := resolveFormat(m[2]); ok {
				setDefault(lits, m[1], f)
			}
		}
		mp := maps{config: config, vars: vars, lits: lits, consts: consts, getters: getters}
		lang := "go"
		if strings.HasSuffix(sf.name, ".java") || strings.HasSuffix(sf.name, ".kt") {
			lang = "java"
		}
		f := parse([]byte(text), lang)

		var add func(kind, role, raw string, unquoted bool, line int)
		calls, gated := x.Calls[service]
		match := ""
		// call: a method call matched by NAME, only in a repo SCIP couldn't index
		call := func(kind, role string, a Arg) {
			match = "name"
			defer func() { match = "" }()
			add(kind, role, a.Value, a.Unquoted, a.Line)
		}
		// hits: what SCIP resolved this call to; ok=false when the repo has no SCIP
		var hits func(c Call) ([]Hit, bool)
		if gated {
			hits = func(c Call) ([]Hit, bool) { return calls[Site{sf.rel, c.Line}][c.Method], true }
		} else {
			hits = func(Call) ([]Hit, bool) { return nil, false }
		}
		add = func(kind, role, raw string, unquoted bool, line int) {
			if raw == "" {
				return
			}
			m := mp
			if x.Symbols != nil && line >= 0 {
				m.exact = func(ref string) (string, bool) {
					col := strings.Index(sf.lines[line], ref) // the reference on its line
					if col < 0 {
						return "", false
					}
					return x.Symbols.Lookup(service, sf.rel, line, col+strings.LastIndex(ref, ".")+1, ref)
				}
			}
			host := ""
			m.host = &host
			ch := normalizeChannel(kind, raw, unquoted, m) + profileTag
			e := Edge{Kind: kind, Channel: ch, Role: role, Repo: service, Path: sf.rel, Line: line, Match: match, Host: host}
			if (role == "produce" || kind == "redis" || kind == "db") && line >= 0 { // a call, not a handler
				lt := sf.lines[line]
				e.Call = f.criticality(line, len(lt)-len(strings.TrimLeft(lt, " \t")), len(strings.TrimRight(lt, " \t\r")), kind)
			}
			if role == "produce" {
				e.Src = service
			} else {
				e.Dst = service
			}
			edges = append(edges, e)
		}

		x.extractFile(f, service, text, add, call, hits)
		f.close()
	}
	return edges
}

func (x *Extractor) extractFile(f *file, service, text string, add func(kind, role, raw string, unquoted bool, line int), call func(kind, role string, a Arg), hits func(Call) ([]Hit, bool)) {
	lang := f.lang
	argAt := func(args []Arg, i int) (Arg, bool) {
		if len(args) > i {
			return args[i], true
		}
		return Arg{}, false
	}

	// 1. Annotations (Java). An @FeignClient interface's mappings are CLIENT
	// calls (produce), not server routes.
	if lang == "java" {
		anns := f.annotations()
		feign := false
		for _, a := range anns {
			feign = feign || a.Name == "FeignClient"
		}
		for _, a := range anns {
			k, ok := annKind[a.Name]
			if !ok {
				continue
			}
			keys := []string{"path", "value", "default"}
			if a.Name == "KafkaListener" {
				keys = []string{"topics", "value", "default"}
			}
			if a.OnType && k.Kind == "rest" {
				continue // a class-level @RequestMapping is its methods' prefix, not a route
			}
			for _, key := range keys {
				if v, ok := a.Kwargs[key]; ok {
					role := k.Role
					if feign && k.Kind == "rest" {
						role = "produce"
					}
					if k.Kind == "rest" && a.Prefix != "" && !v.Unquoted {
						v.Value = joinPath(a.Prefix, v.Value)
					}
					add(k.Kind, role, v.Value, v.Unquoted, v.Line)
					break
				}
			}
		}
	}

	// 2. Method calls. WebClient `.uri(path)` counts only when WebClient is in the file.
	webclient := lang == "java" && strings.Contains(text, "WebClient")
	goIdx := 0
	if lang == "go" {
		goIdx = 1 // Go: ctx first (SendMessage(ctx, topic), rdb.Set(ctx, key, v))
	}
	for _, c := range f.calls() {
		m, args := c.Method, c.Args
		if lang == "go" && (m == "GET" || m == "POST" || m == "PUT" || m == "DELETE" || m == "HandleFunc" || m == "Handle") {
			// a route declared (gin / net/http mux), not a call: syntax decides
			if a, ok := argAt(args, 0); ok {
				add("rest", "consume", a.Value, a.Unquoted, a.Line)
			}
			continue
		}
		if hs, ok := hits(c); ok {
			// SCIP decides what the call is; the source gives only its arguments
			for _, h := range hs {
				if len(h.Resources) > 0 {
					for _, r := range h.Resources {
						add(h.Kind, h.Role, r, false, c.Line)
					}
					continue
				}
				if a, ok := hitArg(args, h.Arg); ok {
					add(h.Kind, h.Role, a.Value, a.Unquoted, a.Line)
				}
			}
			continue
		}
		// no SCIP for this repo: method names decide (Match "name")
		if w, ok := x.Wrappers[service][m]; ok {
			// SCIP resolved this custom method to a transport sink; the channel is
			// the first literal arg at the call site (skips a Go ctx), else arg-0.
			var ch *Arg
			for i := range args {
				if !args[i].Unquoted {
					ch = &args[i]
					break
				}
			}
			if ch == nil && len(args) > 0 {
				ch = &args[0]
			}
			if ch != nil {
				call(w.Kind, w.Role, *ch)
			}
			continue
		}
		switch {
		case m == "uri" && webclient:
			if a, ok := argAt(args, 0); ok {
				call("rest", "produce", a)
			}
		case m == "send" || m == "SendMessage":
			if a, ok := argAt(args, goIdx); ok {
				call("kafka", "produce", a)
			}
		case m == "getForObject" || m == "postForObject" || m == "exchange" ||
			(c.Obj == "http" && (m == "Get" || m == "Post")) ||
			m == "NewRequest" || m == "NewRequestWithContext":
			// Go http.NewRequest(method, url, ...) has the url at arg-1.
			i := 0
			if m == "NewRequest" || m == "NewRequestWithContext" {
				i = 1
			}
			if a, ok := argAt(args, i); ok {
				call("rest", "produce", a)
			}
		case (m == "Set" || m == "set") && c.Obj != "http":
			if a, ok := argAt(args, goIdx); ok {
				call("redis", "produce", a)
			}
		case (m == "Get" || m == "get") && c.Obj != "System" && c.Obj != "os" && c.Obj != "http":
			// chi/gin r.Get("/path", h) is a route (goChiRoutes owns it), not a redis key.
			route := lang == "go" && len(args) > 0 && !args[0].Unquoted && strings.HasPrefix(args[0].Value, "/")
			if a, ok := argAt(args, goIdx); ok && !route {
				call("redis", "consume", a)
			}
		case strings.HasPrefix(m, "New") && strings.HasSuffix(m, "Client"):
			call("grpc", "produce", Arg{Value: m[3 : len(m)-6], Line: c.Line})
		case strings.HasPrefix(m, "Register") && strings.HasSuffix(m, "Server"):
			call("grpc", "consume", Arg{Value: m[8 : len(m)-6], Line: c.Line})
		}
	}

	if lang == "go" {
		// 2b. chi/gin routes with their r.Route(prefix) group composed in.
		for _, r := range f.goChiRoutes() {
			add("rest", "consume", r, false, -1)
		}
		// 3. Kafka consumers: struct fields (Topic: "x") and []string topic vars.
		for _, kv := range f.goStructFields() {
			if strings.Contains(strings.ToLower(kv.Key), "topic") {
				for _, v := range kv.Vals {
					add("kafka", "consume", v, false, -1)
				}
			}
		}
		for _, kv := range f.goTopicVars() {
			if strings.Contains(strings.ToLower(kv.Key), "topic") {
				for _, v := range kv.Vals {
					add("kafka", "consume", v, false, -1)
				}
			}
		}
		// Producer: kafka.WriterConfig{Topic: cfg.X} — a config-struct ref, resolved
		// via the constant map. Line comments stripped so a doc-comment Topic: misses.
		code := reLineCmt.ReplaceAllString(text, "")
		if reWriter.MatchString(code) {
			for _, m := range reTopicField.FindAllStringSubmatch(code, -1) {
				add("kafka", "produce", m[1], true, -1)
			}
		}
	}

	// 4. SQL in string literals.
	for _, s := range f.stringLiterals() {
		if m := reDBProduce.FindStringSubmatch(s); m != nil {
			add("db", "produce", m[1], false, -1)
		}
		if m := reDBConsume.FindStringSubmatch(s); m != nil {
			add("db", "consume", m[1], false, -1)
		}
	}
}

// ExtractAll extracts every service dir directly under reposDir, in name order.
func (x *Extractor) ExtractAll(reposDir string) []Edge {
	ents, _ := os.ReadDir(reposDir)
	var all []Edge
	for _, e := range ents {
		if e.IsDir() {
			all = append(all, x.ExtractRepo(filepath.Join(reposDir, e.Name()))...)
		}
	}
	return all
}

// WriteEdges replaces the service_edges table in db.
func WriteEdges(db *sql.DB, edges []Edge) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DROP TABLE IF EXISTS service_edges"); err != nil {
		return err
	}
	if _, err := tx.Exec("CREATE TABLE service_edges (src TEXT, dst TEXT, kind TEXT, channel TEXT, role TEXT, repo TEXT)"); err != nil {
		return err
	}
	st, err := tx.Prepare("INSERT INTO service_edges VALUES (?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer st.Close()
	for _, e := range edges {
		if _, err := st.Exec(e.Src, e.Dst, e.Kind, e.Channel, e.Role, e.Repo); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// hitArg: a hit's resource argument: index i, or (i < 0) the first literal
// argument, else argument 0.
func hitArg(args []Arg, i int) (Arg, bool) {
	if i >= 0 {
		if i < len(args) {
			return args[i], true
		}
		return Arg{}, false
	}
	for _, a := range args {
		if !a.Unquoted {
			return a, true
		}
	}
	if len(args) > 0 {
		return args[0], true
	}
	return Arg{}, false
}

// joinPath: a type's mapping prefix and a member's path, one slash between.
func joinPath(prefix, path string) string {
	return "/" + strings.Trim(strings.TrimRight(prefix, "/")+"/"+strings.TrimLeft(path, "/"), "/")
}
