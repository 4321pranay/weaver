// Package edgemap builds a domain's service map as docs/edges-spec.md defines it:
// nodes (services and the infrastructure instances they use) and edges with
// their call sites, criticality and code references. It is generated from the
// extracted rows plus the SCIP data in kb.db, and written as edges.yaml.
package edgemap

import (
	"crypto/sha1"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"weaver/internal/extract"
)

type Ref struct {
	Symbol string `yaml:"symbol,omitempty"`
	At     string `yaml:"at"`
}

type Entry struct {
	Kind     string `yaml:"kind"`
	Resource string `yaml:"resource"`
	Symbol   string `yaml:"symbol"`
	At       string `yaml:"at"`
}

type Caller struct {
	At       string `yaml:"at"`
	Call     string `yaml:"call"`
	Method   *Ref   `yaml:"method,omitempty"`
	CalledBy []Ref  `yaml:"called_by,omitempty"`
	Entries  any    `yaml:"entries,omitempty"` // []Entry, or "unknown" when the walk was cut off
}

type Edge struct {
	From     string `yaml:"from"`
	To       string `yaml:"to"`
	Kind     string `yaml:"kind"`
	Resource string `yaml:"resource"`
	Op       string `yaml:"op,omitempty"`
	Match    string `yaml:"match,omitempty"` // "name": found by method name only (repo without SCIP)
	Call     string `yaml:"call,omitempty"`
	Question string `yaml:"question,omitempty"` // the open question this edge waits on; its call is then unknown
	// ReviewedBy: llm when weave-ai checked the edge against the code and agreed;
	// human when it disagreed and a person kept it (analyzer-spec §5)
	ReviewedBy string   `yaml:"reviewedBy,omitempty"`
	Handler    *Ref     `yaml:"handler,omitempty"`
	Callers    []Caller `yaml:"callers,omitempty"`
}

type Node struct {
	ID      string         `yaml:"id"`
	Type    string         `yaml:"type"`
	Props   map[string]any `yaml:"props,omitempty"`
	Aliases []string       `yaml:"aliases,omitempty"`
}

type Map struct {
	Version string `yaml:"version"`       // FormatEdges
	Run     string `yaml:"run,omitempty"` // the build's run id (RunID)
	// EntryStats: per service, the entries walk over EVERY call site of the
	// domain, held-back ones included — {found, none, unknown}. Not written.
	EntryStats map[string][3]int `yaml:"-"`
	Nodes      []Node            `yaml:"nodes"`
	Edges      []Edge            `yaml:"edges"`
}

// Question is a gap weaver can't fill from code or config (spec §6.3). Its ID
// is stable: the same gap on the next run is the same question. Pending counts
// the call sites waiting on it: they are in the map with call: unknown.
type Question struct {
	ID      string            `yaml:"id"`
	Type    string            `yaml:"type"` // address | target | resource | config | client
	Ask     string            `yaml:"ask"`
	Subject map[string]string `yaml:"subject"`
	Expect  string            `yaml:"expect"` // what `weaver answer` takes
	Pending int               `yaml:"pending"`
	Sites   []string          `yaml:"sites,omitempty"`
}

const maxSites = 10

// File formats: owner/format/vMAJOR.MINOR. Each file
// format is versioned on its own: a minor version adds compatibly, a major
// version breaks.
const (
	FormatEdges     = "weaver/edges/v1.0" // edges.yaml and edge snapshots
	FormatOverride  = "weaver/edges.override/v1.0"
	FormatQuestions = "weaver/questions/v1.0"
	FormatReleases  = "weaver/releases/v1.0"
)

// CheckFormat accepts got when it is want's format at want's major version
// with a minor up to want's; anything else is refused with the reason.
func CheckFormat(got, want string) error {
	split := func(v string) (name string, major, minor int, ok bool) {
		i := strings.LastIndex(v, "/v")
		if i < 0 {
			return "", 0, 0, false
		}
		_, err := fmt.Sscanf(v[i+2:], "%d.%d", &major, &minor)
		return v[:i], major, minor, err == nil
	}
	gn, gM, gm, ok1 := split(got)
	wn, wM, wm, _ := split(want)
	switch {
	case !ok1:
		return fmt.Errorf("version %q: expected %s", got, want)
	case gn != wn:
		return fmt.Errorf("version %q is a %s file, not %s", got, gn, wn)
	case gM != wM || gm > wm:
		return fmt.Errorf("version %q: this weaver reads %s up to v%d.%d", got, wn, wM, wm)
	}
	return nil
}

// Input: the domain, every extracted row of the build (all domains, so
// cross-domain calls match), and per-service lookups.
type Input struct {
	Domain   string
	DomainOf map[string]string // service -> domain, all domains
	Raw      []extract.Edge
	Config   func(service string) map[string]string
	KBs      []*sql.DB // SCIP data of every domain
	Override *Override // the domain's edges.override.yaml (nil = none)
	// Answers: answered `resource` questions, by question id (from questions.yaml)
	Answers map[string]string
	// CallGraph: per service, interface call sites and the concrete methods they
	// reach (tools/gocg, tools/javacg); lets the entries walk cross injected interfaces
	CallGraph map[string][]CGEdge
}

// CGEdge is one interface call site: File (Go, repo-relative) or Caller (Java,
// `pkg/Class#method(`), its 1-based Line, and the methods it can reach in SCIP's
// descriptor form (`pkg`/Type#Method( or pkg/Class#method().
type CGEdge struct {
	File    string   `json:"file"`
	Caller  string   `json:"caller"`
	Line    int      `json:"line"`
	Targets []string `json:"targets"`
}

// Build the domain's map: extracted edges, then the override merged in (spec
// §6). It returns the questions that held parts back, and warnings (spec §7);
// an invalid override is an error.
func Build(in Input) (Map, []Question, []string, error) {
	if in.Override == nil {
		in.Override = &Override{}
	}
	b := &builder{in: in, nodes: map[string]*Node{}, edges: map[[4]string]*Edge{},
		sites: map[[4]string]map[string]bool{}, asked: map[string]*Question{}, kafkaAsked: map[string]*Question{}, scip: newSCIP(in.KBs, in.CallGraph),
		answered: in.Override.aliasOwners()}
	mine := func(s string) bool { return in.DomainOf[s] == in.Domain }

	// consumers of rest / grpc, by kind + normalised resource
	type kr struct{ kind, res string }
	servers := map[kr][]extract.Edge{}
	for _, e := range in.Raw {
		if e.Role == "consume" && (e.Kind == "rest" || e.Kind == "grpc") {
			if res, ok := resource(e.Kind, e.Channel); ok {
				servers[kr{e.Kind, res}] = append(servers[kr{e.Kind, res}], e)
			}
		}
	}
	for _, e := range in.Raw {
		res, ok := resource(e.Kind, e.Channel)
		resQ := "" // the resource question this call site waits on, if any
		if !ok {
			if !mine(e.Repo) || (e.Role == "consume" && (e.Kind == "rest" || e.Kind == "grpc")) {
				continue // a route it serves is no dependency: nothing to ask
			}
			q := Question{Type: "resource", Subject: map[string]string{"service": e.Repo, "kind": e.Kind, "expression": e.Channel},
				Ask:    fmt.Sprintf("%s: which %s does this %s call use? Not resolvable from code: %s", e.Repo, resourceWord[e.Kind], e.Kind, e.Channel),
				Expect: "the " + resourceWord[e.Kind] + ", e.g. orders.created; {} for runtime parts"}
			if res, ok = in.Answers[qid(q)]; !ok {
				res, resQ = "{}", b.ask(q, e) // written as {}, waiting on the answer
			}
		}
		switch {
		case e.Kind == "rest" || e.Kind == "grpc":
			if e.Role != "produce" {
				continue
			}
			if resQ != "" { // what it calls is the question: no server can be matched yet
				b.service(e.Src)
				b.caller(b.edge(e.Src, b.unknownNode(e.Kind, resQ), e.Kind, res, ""), e)
				continue
			}
			var to []extract.Edge
			for _, c := range servers[kr{e.Kind, res}] {
				if c.Dst != e.Src && (mine(e.Src) || mine(c.Dst)) {
					to = append(to, c)
				}
			}
			if len(to) == 0 && mine(e.Src) {
				// nothing in any domain serves it: a host no service owns is external
				// (spec §3.3 rule 5); with no host, ask
				host := hostName(e.Host)
				if host == "" || in.DomainOf[host] != "" {
					q := b.ask(Question{Type: "target", Subject: map[string]string{"service": e.Src, "kind": e.Kind, "resource": res},
						Ask:    fmt.Sprintf("%s calls %s %s, which no service serves: which node is it?", e.Src, e.Kind, res),
						Expect: "a service name, or external/<host>"}, e)
					b.service(e.Src)
					b.caller(b.edge(e.Src, b.unknownNode(e.Kind, q), e.Kind, res, ""), e)
					continue
				}
				n := b.addInstance(Node{ID: "external/" + host, Type: "external", Props: map[string]any{"host": e.Host}})
				b.service(e.Src)
				b.caller(b.edge(e.Src, n, e.Kind, res, ""), e)
				continue
			}
			seen := map[string]bool{}
			for _, c := range to {
				if seen[c.Dst] {
					continue
				}
				seen[c.Dst] = true
				b.service(e.Src)
				b.service(c.Dst)
				ed := b.edge(e.Src, c.Dst, e.Kind, res, "")
				b.caller(ed, e)
				if ed.Handler == nil {
					ed.Handler = b.handler(c)
				}
			}
		case !mine(e.Repo):
		case e.Kind == "kafka":
			n, _ := b.instance(e, "kafka") // unknown/<id> when the instance is the question
			b.service(e.Repo)
			var ed *Edge
			if e.Role == "produce" {
				ed = b.edge(e.Repo, n, "kafka", res, "produce")
				b.caller(ed, e)
			} else if ed = b.edge(n, e.Repo, "kafka", res, "consume"); ed.Handler == nil {
				ed.Handler = b.handler(e)
			}
			if resQ != "" && ed.Question == "" {
				ed.Question = resQ
			}
		case e.Kind == "redis" || e.Kind == "db":
			typ := map[string]string{"redis": "redis", "db": "database"}[e.Kind]
			n, _ := b.instance(e, typ)
			b.service(e.Repo)
			op := "read"
			if e.Role == "produce" {
				op = "write"
			}
			ed := b.edge(e.Repo, n, e.Kind, res, op)
			if ed.Op != op {
				ed.Op = "readwrite"
			}
			if resQ != "" && ed.Question == "" {
				ed.Question = resQ
			}
			b.caller(ed, e)
		}
	}
	m, qs, warns, err := b.result()
	if err == nil {
		m.EntryStats = b.entryStats(mine)
	}
	return m, qs, warns, err
}

// entryStats runs the entries walk over every call site of the domain's
// services, whether or not its edge made it into the map.
func (b *builder) entryStats(mine func(string) bool) map[string][3]int {
	out := map[string][3]int{}
	for _, e := range b.in.Raw {
		if !mine(e.Repo) || e.Line < 0 || !(e.Role == "produce" || e.Kind == "redis" || e.Kind == "db") {
			continue
		}
		m, ok := b.scip.methodAt(e.Repo, e.Path, e.Line)
		if !ok {
			continue
		}
		s := out[e.Repo]
		switch v := b.scip.entries(e.Repo, m.symbol, b.entryPoints(e.Repo)).(type) {
		case string:
			s[2]++
		case []Entry:
			if len(v) == 0 {
				s[1]++
			} else {
				s[0]++
			}
		}
		out[e.Repo] = s
	}
	return out
}

type builder struct {
	in    Input
	nodes map[string]*Node
	edges map[[4]string]*Edge
	sites map[[4]string]map[string]bool // edge -> caller `at`s seen
	asked map[string]*Question
	scip  *scipIndex
	// answered: override node aliases -> node id (`${KEY}`, `kafka@checkout`)
	answered map[string]string
	// kafkaAsked: broker -> the open cluster question it belongs to, so broker
	// sets that overlap share one question until it is answered
	kafkaAsked map[string]*Question
}

// ask records a question for site and returns its id. The site is not held
// back: its edge is still written, waiting on the question (questions-spec).
func (b *builder) ask(q Question, site extract.Edge) string {
	q.ID = qid(q)
	if b.asked[q.ID] == nil {
		b.asked[q.ID] = &q
	}
	p := b.asked[q.ID]
	p.Pending++
	if len(p.Sites) < maxSites {
		p.Sites = append(p.Sites, site.Repo+":"+at(site.Path, site.Line))
	}
	return q.ID
}

// unknownNode: the placeholder for an end a question is about (edges-spec
// §2.1), replaced by the answer on the next build.
func (b *builder) unknownNode(kind, qid string) string {
	id := "unknown/" + qid
	if b.nodes[id] == nil {
		b.nodes[id] = &Node{ID: id, Type: "unknown", Props: map[string]any{"kind": kind, "question": qid}}
	}
	return id
}

// qid: a stable id from the question's type and subject.
func qid(q Question) string {
	keys := make([]string, 0, len(q.Subject))
	for k := range q.Subject {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha1.New()
	h.Write([]byte(q.Type))
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, q.Subject[k])
	}
	return fmt.Sprintf("q-%x", h.Sum(nil)[:4])
}

func (b *builder) service(s string) {
	if b.nodes[s] == nil {
		b.nodes[s] = &Node{ID: s, Type: "service", Props: map[string]any{"domain": b.in.DomainOf[s]}}
	}
}

func (b *builder) edge(from, to, kind, res, op string) *Edge {
	k := [4]string{from, to, kind, res}
	if b.edges[k] == nil {
		b.edges[k] = &Edge{From: from, To: to, Kind: kind, Resource: res, Op: op}
		b.sites[k] = map[string]bool{}
		for _, end := range []string{from, to} {
			if q, ok := strings.CutPrefix(end, "unknown/"); ok {
				b.edges[k].Question = q
			}
		}
	}
	return b.edges[k]
}

func (b *builder) caller(ed *Edge, e extract.Edge) {
	k := [4]string{ed.From, ed.To, ed.Kind, ed.Resource}
	a := at(e.Path, e.Line)
	if b.sites[k][a] {
		return
	}
	b.sites[k][a] = true
	if e.Match != "" {
		ed.Match = e.Match
	}
	c := Caller{At: a, Call: e.Call}
	if c.Call == "" {
		c.Call = "unknown"
	}
	if m, ok := b.scip.methodAt(e.Repo, e.Path, e.Line); ok {
		c.Method = &m.ref
		c.CalledBy = b.scip.calledBy(m.symbol)
		c.Entries = b.scip.entries(e.Repo, m.symbol, b.entryPoints(e.Repo))
	}
	ed.Callers = append(ed.Callers, c)
}

func (b *builder) handler(e extract.Edge) *Ref {
	if m, ok := b.scip.methodAt(e.Repo, e.Path, e.Line); ok {
		return &m.ref
	}
	if e.Line >= 0 {
		return &Ref{At: at(e.Path, e.Line)}
	}
	return nil
}

// entryPoints of a service (spec §4.5): the methods serving its rest / grpc /
// kafka consume rows.
// ponytail: @Scheduled jobs and main() are not detected yet.
func (b *builder) entryPoints(repo string) map[string]Entry {
	if ep, ok := b.scip.eps[repo]; ok {
		return ep
	}
	ep := map[string]Entry{}
	for _, e := range b.in.Raw {
		if e.Repo != repo || e.Role != "consume" || e.Line < 0 || !(e.Kind == "rest" || e.Kind == "grpc" || e.Kind == "kafka") {
			continue
		}
		res, ok := resource(e.Kind, e.Channel)
		if !ok {
			continue
		}
		if m, ok := b.scip.methodAt(repo, e.Path, e.Line); ok {
			if _, dup := ep[m.symbol]; !dup {
				ep[m.symbol] = Entry{Kind: e.Kind, Resource: res, Symbol: m.ref.Symbol, At: m.ref.At}
			}
		}
	}
	b.scip.eps[repo] = ep
	return ep
}

func (b *builder) result() (Map, []Question, []string, error) {
	warns, err := b.merge()
	if err != nil {
		return Map{}, nil, nil, err
	}
	m := Map{Version: FormatEdges, Nodes: []Node{}, Edges: []Edge{}}
	for _, n := range b.nodes {
		m.Nodes = append(m.Nodes, *n)
	}
	for _, e := range b.edges {
		e.Call = edgeCall(e)
		if e.Question != "" {
			e.Call = "unknown" // counts as required until its question is answered
		}
		m.Edges = append(m.Edges, *e)
	}
	sort.Slice(m.Nodes, func(i, j int) bool { return m.Nodes[i].ID < m.Nodes[j].ID })
	sort.Slice(m.Edges, func(i, j int) bool {
		a, c := m.Edges[i], m.Edges[j]
		return a.From+"\x00"+a.To+"\x00"+a.Kind+"\x00"+a.Resource < c.From+"\x00"+c.To+"\x00"+c.Kind+"\x00"+c.Resource
	})
	var qs []Question
	for _, q := range b.asked {
		qs = append(qs, *q)
	}
	sort.Slice(qs, func(i, j int) bool { return qs[i].Ask < qs[j].Ask })
	for i := range qs {
		sort.Strings(qs[i].Sites)
	}
	return m, qs, warns, nil
}

// edgeCall derives the edge's criticality from its callers (spec §5).
func edgeCall(e *Edge) string {
	if len(e.Callers) == 0 {
		return ""
	}
	best := ""
	for _, c := range e.Callers {
		switch c.Call {
		case "required", "unknown":
			return "required"
		case "fallback":
			best = "fallback"
		case "optional":
			if best != "fallback" {
				best = "optional"
			}
		case "async":
			if best == "" {
				best = "async"
			}
		}
	}
	return best
}

var (
	reScheme = regexp.MustCompile(`^[a-z]+://[^/]*`)
	reParam  = regexp.MustCompile(`\{[^}]*\}|/:[A-Za-z_]\w*|/\d+(/|$)`)
)

var (
	reRuntime = regexp.MustCompile(`<RUNTIME_PARAMETER: [^>]*>`)
	reProfile = regexp.MustCompile(` \[Condition: [^\]]*\]$`)
)

var resourceWord = map[string]string{"rest": "endpoint", "grpc": "method", "kafka": "topic", "redis": "key", "db": "table", "s3": "prefix"}

// resource normalises an extracted channel (spec §4.2): REST scheme and host
// stripped, path parameters and runtime values as {}. ok=false when nothing of
// it is known: such a call site is written as {} and asked about, never guessed.
func resource(kind, ch string) (string, bool) {
	ch = reProfile.ReplaceAllString(ch, "")
	if strings.HasPrefix(ch, "<DYNAMIC_DISPATCH") || ch == "" {
		return "", false
	}
	ch = reRuntime.ReplaceAllString(ch, "{}")
	if strings.Trim(ch, "{}/") == "" {
		return "", false // only runtime parts
	}
	if kind != "rest" {
		return ch, true
	}
	ch = reScheme.ReplaceAllString(ch, "")
	return reParam.ReplaceAllStringFunc(ch, func(p string) string {
		switch {
		case strings.HasPrefix(p, "/:"):
			return "/{}"
		case strings.HasPrefix(p, "/"):
			return "/{}" + strings.TrimLeft(p, "/0123456789")
		}
		return "{}"
	}), true
}

// hostName: a URL host without its port, lower-cased.
func hostName(h string) string {
	if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func at(path string, line int) string {
	if line < 0 {
		return path
	}
	return fmt.Sprintf("%s:%d", path, line+1)
}

// Write the map as YAML, atomically.
func Write(path string, m Map) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	head := "# Generated by weaver build (docs/edges-spec.md). Do not edit: corrections go in edges.override.yaml.\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(head), b...), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- infrastructure instances (spec §3.3) ----

var instanceKeys = map[string][]string{
	"kafka": {"spring.kafka.bootstrap-servers", "spring.kafka.producer.bootstrap-servers",
		"spring.kafka.consumer.bootstrap-servers", "spring.cloud.stream.kafka.binder.brokers",
		"kafka.bootstrap-servers", "kafka.brokers", "KAFKA_BOOTSTRAP_SERVERS", "KAFKA_BROKERS"},
	"database": {"spring.datasource.url", "spring.datasource.master.url", "spring.datasource.dynamic.datasource.master.url",
		"spring.r2dbc.url", "spring.data.mongodb.uri", "DATABASE_URL", "DB_URL", "POSTGRES_URL", "MYSQL_URL"},
	"redis": {"spring.redis.url", "spring.data.redis.url", "spring.redis.host", "spring.data.redis.host",
		"REDIS_URL", "REDIS_ADDR", "REDIS_HOST"},
}

var rePlace = regexp.MustCompile(`\$\{([^}:]+)(?::([^}]*))?\}`)

// expand ${key:default} against the config; ok=false if a placeholder is left.
func expand(cfg map[string]string, v string) (string, bool) {
	for i := 0; i < 5 && strings.Contains(v, "${"); i++ {
		v = rePlace.ReplaceAllStringFunc(v, func(p string) string {
			m := rePlace.FindStringSubmatch(p)
			if c, ok := cfg[m[1]]; ok {
				return c
			}
			if env := os.Getenv(m[1]); env != "" {
				return env
			}
			if strings.Contains(p, ":") {
				return m[2]
			}
			return p
		})
	}
	return v, !strings.Contains(v, "${") && v != ""
}

// instance: the node id of the service's kafka / database / redis, from its
// config. Unresolvable -> a question, and the placeholder node unknown/<id>
// with ok=false: the edge still goes to it, waiting on the answer.
// ponytail: one instance per type per repo (config merged over the repo); a
// monorepo with two databases gets the last one.
func (b *builder) instance(e extract.Edge, typ string) (string, bool) {
	svc := e.Repo
	if id, ok := b.answered[typ+"@"+svc]; ok { // answered in the override
		return id, true
	}
	cfg := b.config(svc)
	key, raw := "", ""
	for _, k := range instanceKeys[typ] {
		v, ok := cfg[k]
		for _, e := range extract.EnvKeys(k) { // set by the environment (Compose), relaxed binding
			if ev, eok := cfg[e]; eok {
				v, ok = ev, true
			}
		}
		if ok {
			key, raw = k, v
			break
		}
	}
	if key == "" {
		q := b.ask(Question{Type: "address", Subject: map[string]string{"service": svc, "nodeType": typ},
			Ask:    fmt.Sprintf("%s: which %s instance does it use? No config key found", svc, typ),
			Expect: fmt.Sprintf("a node id, e.g. %s/<name>; saved as a node with alias %s@%s", typ, typ, svc)}, e)
		return b.unknownNode(typ, q), false
	}
	for _, a := range []string{raw, "${" + key + "}"} {
		if id, ok := b.answered[a]; ok {
			return id, true
		}
	}
	v, ok := expand(cfg, raw)
	if !ok {
		q := b.ask(Question{Type: "address", Subject: map[string]string{"nodeType": typ, "configKey": raw},
			Ask:    fmt.Sprintf("which %s instance is %s? It doesn't resolve from config", typ, raw),
			Expect: fmt.Sprintf("a node id, e.g. %s/<name>; saved as a node with alias %s", typ, raw)}, e)
		return b.unknownNode(typ, q), false
	}
	var n Node
	switch typ {
	case "kafka":
		var hosts, brokers []string
		for _, br := range strings.Split(v, ",") {
			br = strings.TrimSpace(strings.TrimPrefix(br, "PLAINTEXT://"))
			if br == "" {
				continue
			}
			if !strings.Contains(br, ":") {
				br += ":9092"
			}
			brokers = append(brokers, br)
			hosts = append(hosts, br[:strings.LastIndex(br, ":")])
		}
		sort.Strings(brokers)
		// the id names the cluster (edges-spec §2.3 rule 4): a broker it already
		// has finds it, so a changed or partial broker list keeps the same node
		for _, br := range brokers {
			if id, ok := b.answered[br]; ok {
				return id, true
			}
		}
		for _, id := range sortedNodeIDs(b.nodes) {
			if nd := b.nodes[id]; nd.Type == "kafka" && len(intersect(nd.Aliases, brokers)) > 0 {
				nd.Aliases = union(nd.Aliases, brokers)
				return id, true
			}
		}
		cq := Question{Type: "address", Subject: map[string]string{"nodeType": typ, "brokers": strings.Join(brokers, ",")},
			Ask:    fmt.Sprintf("which Kafka cluster are brokers %s? Name it once", strings.Join(brokers, ", ")),
			Expect: "a node id, e.g. kafka/orders; saved with its brokers as aliases"}
		for _, br := range brokers {
			if p := b.kafkaAsked[br]; p != nil {
				cq = *p // overlaps a cluster already asked about: the same question
				break
			}
		}
		q := b.ask(cq, e)
		for _, br := range brokers {
			b.kafkaAsked[br] = b.asked[q]
		}
		return b.unknownNode(typ, q), false
	case "database":
		engine, host, db := parseDSN(v)
		if db == "" {
			if d := cfg["spring.data.mongodb.database"]; d != "" {
				db = d
			} else {
				q := b.ask(Question{Type: "address", Subject: map[string]string{"nodeType": typ, "configKey": raw},
					Ask:    fmt.Sprintf("which database does %s name? Its URL has no database name", raw),
					Expect: fmt.Sprintf("a node id, e.g. database/<name>; saved as a node with alias %s", raw)}, e)
				return b.unknownNode(typ, q), false
			}
		}
		n = Node{ID: "database/" + db, Type: "database", Props: map[string]any{"engine": engine, "database": db, "host": host}}
	case "redis":
		host := v
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			host = u.Host
		}
		if !strings.Contains(host, ":") {
			port := cfg[strings.TrimSuffix(key, "host")+"port"]
			if p, ok := expand(cfg, port); ok {
				host += ":" + p
			} else {
				host += ":6379"
			}
		}
		n = Node{ID: "redis/" + host[:strings.LastIndex(host, ":")], Type: "redis", Props: map[string]any{"host": host}}
	}
	n.Aliases = append(n.Aliases, "${"+key+"}")
	return b.addInstance(n), true
}

// addInstance keeps one node per instance; two instances with one name get
// their host appended (spec §3.3 rule 3).
func (b *builder) addInstance(n Node) string {
	old := b.nodes[n.ID]
	switch {
	case old == nil:
		b.nodes[n.ID] = &n
		return n.ID
	case fmt.Sprint(old.Props["host"]) == fmt.Sprint(n.Props["host"]):
		old.Aliases = union(old.Aliases, n.Aliases)
		return n.ID
	}
	n.ID += "@" + fmt.Sprint(n.Props["host"])
	if b.nodes[n.ID] == nil {
		b.nodes[n.ID] = &n
	}
	return n.ID
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(a, b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// parseDSN: jdbc:mysql://h:3306/db?x, postgres://u:p@h/db, mongodb://h/db.
func parseDSN(v string) (engine, host, db string) {
	v = strings.TrimPrefix(v, "jdbc:")
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" {
		return "", "", ""
	}
	engine = strings.Split(u.Scheme, "+")[0] // mongodb+srv -> mongodb
	if engine == "postgresql" {
		engine = "postgres"
	}
	return engine, u.Host, strings.Trim(u.Path, "/")
}

func (b *builder) config(svc string) map[string]string {
	if b.scip.cfg[svc] == nil {
		b.scip.cfg[svc] = b.in.Config(svc)
	}
	return b.scip.cfg[svc]
}

// Path of a service's checkout, for Config.
func SourceConfig(sources func(svc string) (repo, root string)) func(string) map[string]string {
	return func(svc string) map[string]string {
		repo, root := sources(svc)
		return extract.ServiceConfig(filepath.Clean(repo), root)
	}
}

func sortedNodeIDs(m map[string]*Node) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// EdgeRef: an edge's reference, from → to kind resource (mcp-spec §2).
func EdgeRef(e Edge) string { return fmt.Sprintf("%s → %s %s %s", e.From, e.To, e.Kind, e.Resource) }
