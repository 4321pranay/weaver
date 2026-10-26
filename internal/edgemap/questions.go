package edgemap

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Questions is knowledge/<domain>/questions.yaml: every question any build of
// the domain has raised, with the runs that raised it, its status and its
// answer, plus a log of the runs. It is kept across builds, so problems can be
// tracked over all of them. Only `weaver answer` answers a question.
type Questions struct {
	Version   string  `yaml:"version"`
	Runs      []Run   `yaml:"runs"`
	Questions []QItem `yaml:"questions"`
}

// Run is one build of the domain, as far as its questions go.
type Run struct {
	ID       string `yaml:"id"`
	Build    int    `yaml:"build,omitempty"` // the generation this build wrote (weaver-architecture §2)
	At       string `yaml:"at"`
	Services int    `yaml:"services"`
	Raised   int    `yaml:"raised"`   // questions this run raised
	New      int    `yaml:"new"`      // of which first raised by this run
	Pending  int    `yaml:"pending"`  // call sites waiting on an answer, written with call: unknown
	Resolved int    `yaml:"resolved"` // answered questions this run no longer raised
	Obsolete int    `yaml:"obsolete"` // unanswered questions whose gap went away
}

// QItem: a question with its history. Status:
//
//	open      raised, not answered
//	answered  answered with `weaver answer`, not yet built
//	resolved  answered, and the next build no longer raised it
//	obsolete  the gap went away without an answer (code changed)
//	never     answered "-": can't be known, never asked again
type QItem struct {
	Question `yaml:",inline"`
	Status   string     `yaml:"status"`
	FirstRun string     `yaml:"firstRun"`
	LastRun  string     `yaml:"lastRun"` // the last run that raised it
	Answer   *Answered  `yaml:"answer,omitempty"`
	History  []Answered `yaml:"previousAnswers,omitempty"` // answers a later build still raised the question after
}

type Answered struct {
	Value string `yaml:"value" json:"value"`
	By    string `yaml:"by" json:"by"`
	At    string `yaml:"at" json:"at"`
}

func LoadQuestions(path string) (*Questions, error) {
	q := &Questions{Version: FormatQuestions}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, q); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := CheckFormat(q.Version, FormatQuestions); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return q, nil
}

// Answers: answered resource questions, by id, for Input.Answers.
func (q *Questions) Answers() map[string]string {
	m := map[string]string{}
	for _, it := range q.Questions {
		if it.Type == "resource" && it.Answer != nil && (it.Status == "answered" || it.Status == "resolved") {
			m[it.ID] = it.Answer.Value
		}
	}
	return m
}

// ConfigQuestion: a Spring service's production-config key that spring-probe
// couldn't settle: the active profile, a key a bean condition reads, or a value
// that ends up in a fact (topic, path, URL, cron). sites are where the code
// uses it. app is the module of a multi-module repo ("." for the repo root),
// the Spring application the key belongs to. The answer is kept here and
// handed to spring-probe by the next build.
func ConfigQuestion(service, app, key, kind string, options, sites []string) Question {
	q := Question{Type: "config", Subject: map[string]string{"service": service, "key": key, "kind": kind}}
	who := service
	if app != "." {
		q.Subject["app"], who = app, service+"/"+app
	}
	switch kind {
	case "profile":
		q.Ask = fmt.Sprintf("%s: which Spring profile is active in production?", who)
	case "condition":
		q.Ask = fmt.Sprintf("%s: what is %s in production? It decides which beans exist", who, key)
	default:
		q.Ask = fmt.Sprintf("%s: what is %s in production? The repo's config may only have a dev value", who, key)
	}
	q.Expect = "the production value; - if it can't be known"
	if len(options) > 0 {
		q.Expect = "one of: " + strings.Join(options, ", ") + "; - if it can't be known"
	}
	if len(sites) > maxSites {
		sites = sites[:maxSites]
	}
	q.Sites, q.ID = sites, qid(q)
	return q
}

// ClientQuestion: an external type the domain's services call that looks like
// a client of another system but that no rule classifies (scip.ClientCalls).
// Its answer is the kind of system it talks to; the next build classifies its
// calls by it, exactly as a built-in client API.
func ClientQuestion(typ string, calls int, services, methods, sites []string) Question {
	name := typ[strings.LastIndexAny(strings.TrimSuffix(typ, "#"), "/ ")+1:]
	q := Question{Type: "client", Subject: map[string]string{"type": typ},
		Ask: fmt.Sprintf("%s (%s) is called %d times by %s: does it talk to another system, and which kind?",
			strings.TrimSuffix(name, "#"), strings.Join(methods, ", "), calls, strings.Join(services, ", ")),
		Expect: "one of: " + strings.Join(clientKinds, ", ") + "; - if it isn't a client of another system"}
	if len(sites) > maxSites {
		sites = sites[:maxSites]
	}
	q.Sites, q.ID = sites, qid(q)
	return q
}

// clientKinds: what a `client` answer can name: an edge kind (edges-spec §3.1).
var clientKinds = []string{"rest", "grpc", "kafka", "redis", "db", "s3"}

// ClientAnswers: answered client questions, type -> kind, for scip.ClientCalls.
func (q *Questions) ClientAnswers() map[string]string {
	m := map[string]string{}
	for _, it := range q.Questions {
		if it.Type == "client" && it.Answer != nil && (it.Status == "answered" || it.Status == "resolved") {
			m[it.Subject["type"]] = it.Answer.Value
		}
	}
	return m
}

// ConfigAnswers: a service's answered config questions, by key, as
// spring-probe takes them: "<app>:<key>" for a module's application.
func (q *Questions) ConfigAnswers(service string) map[string]Answered {
	m := map[string]Answered{}
	for _, it := range q.Questions {
		if it.Type == "config" && it.Subject["service"] == service && it.Answer != nil && (it.Status == "answered" || it.Status == "resolved") {
			k := it.Subject["key"]
			if a := it.Subject["app"]; a != "" {
				k = a + ":" + k
			}
			m[k] = *it.Answer
		}
	}
	return m
}

// Record merges one run's questions into the file and appends the run.
func (q *Questions) Record(runID string, build int, now time.Time, services int, raised []Question) Run {
	run := Run{ID: runID, Build: build, At: now.UTC().Format(time.RFC3339), Services: services, Raised: len(raised)}
	idx := map[string]int{}
	for i, it := range q.Questions {
		idx[it.ID] = i
	}
	seen := map[string]bool{}
	for _, r := range raised {
		seen[r.ID] = true
		run.Pending += r.Pending
		i, ok := idx[r.ID]
		if !ok {
			q.Questions = append(q.Questions, QItem{Question: r, Status: "open", FirstRun: runID, LastRun: runID})
			run.New++
			continue
		}
		it := &q.Questions[i]
		it.Question, it.LastRun = r, runID
		switch it.Status {
		case "obsolete":
			it.Status = "open"
		case "answered", "resolved": // the answer didn't close the gap: ask again
			it.History = append(it.History, *it.Answer)
			it.Answer, it.Status = nil, "open"
		}
	}
	for i := range q.Questions {
		it := &q.Questions[i]
		if seen[it.ID] {
			continue
		}
		switch it.Status {
		case "open":
			it.Status = "obsolete"
			run.Obsolete++
		case "answered":
			it.Status = "resolved"
			run.Resolved++
		}
	}
	q.Runs = append(q.Runs, run)
	sort.SliceStable(q.Questions, func(i, j int) bool { return q.Questions[i].Ask < q.Questions[j].Ask })
	return run
}

func (q *Questions) Write(path string) error {
	q.Version = FormatQuestions
	b, err := yaml.Marshal(q)
	if err != nil {
		return err
	}
	head := "# Questions raised by weaver builds of this domain (docs/questions-spec.md).\n" +
		"# Answer only with: weaver answer <domain> <id> <value>. Do not edit.\n"
	return writeAtomic(path, append([]byte(head), b...))
}

// Answer records value as the answer to question id, by who, and writes what
// the answer means into the override: an address becomes a node with the
// question's alias, a target an edge. A resource answer is applied by the next
// build from questions.yaml (the override has no per-call-site rows). "-"
// records that it can't be known.
func Answer(questionsPath, overridePath, id, value, by string, now time.Time) (*QItem, error) {
	q, err := LoadQuestions(questionsPath)
	if err != nil {
		return nil, err
	}
	var it *QItem
	for i := range q.Questions {
		if q.Questions[i].ID == id {
			it = &q.Questions[i]
		}
	}
	switch {
	case it == nil:
		return nil, fmt.Errorf("no question %s in %s", id, questionsPath)
	case it.Status == "obsolete" || it.Status == "resolved":
		return nil, fmt.Errorf("%s is %s: nothing to answer", id, it.Status)
	case strings.TrimSpace(value) == "":
		return nil, fmt.Errorf("empty answer")
	}
	value = strings.TrimSpace(value)
	if value != "-" {
		if err := applyAnswer(overridePath, it.Question, value); err != nil {
			return nil, err
		}
	}
	it.Answer = &Answered{Value: value, By: by, At: now.UTC().Format(time.RFC3339)}
	it.Status = "answered"
	if value == "-" {
		it.Status = "never"
	}
	return it, q.Write(questionsPath)
}

// applyAnswer validates the answer for its question type and writes it to the
// override; the override is re-validated, and left untouched on any error.
func applyAnswer(path string, q Question, value string) error {
	var add string
	switch q.Type {
	case "address":
		typ := q.Subject["nodeType"]
		if !strings.HasPrefix(value, typ+"/") || len(value) == len(typ)+1 {
			return fmt.Errorf("an address answer is a node id: %s/<name>", typ)
		}
		if bs := q.Subject["brokers"]; bs != "" { // a Kafka cluster: its brokers are its aliases
			var as []string
			for _, br := range strings.Split(bs, ",") {
				as = append(as, quote(br))
			}
			add = fmt.Sprintf("  - {id: %s, type: kafka, props: {brokers: [%s]}, aliases: [%s]}\n", quote(value), strings.Join(as, ", "), strings.Join(as, ", "))
			return writeOverride(path, add, "", value, strings.Split(bs, ",")...)
		}
		alias := q.Subject["configKey"]
		if alias == "" {
			alias = typ + "@" + q.Subject["service"]
		}
		add = fmt.Sprintf("  - {id: %s, type: %s, aliases: [%s]}\n", quote(value), typ, quote(alias))
		return writeOverride(path, add, "", value, alias)
	case "target":
		node := ""
		if strings.HasPrefix(value, "external/") {
			node = fmt.Sprintf("  - {id: %s, type: external}\n", quote(value))
		}
		add = fmt.Sprintf("  - {from: %s, to: %s, kind: %s, resource: %s}\n",
			quote(q.Subject["service"]), quote(value), q.Subject["kind"], quote(q.Subject["resource"]))
		return writeOverride(path, node, add, "", "")
	case "config": // kept in questions.yaml; the next build hands it to spring-probe
		return nil
	case "review": // keep: the next build marks the edge reviewedBy: human; drop: the override ignores it
		switch value {
		case "keep":
			return nil
		case "drop":
			add = fmt.Sprintf("  - {from: %s, to: %s, kind: %s, resource: %s, call: ignore}\n",
				quote(q.Subject["from"]), quote(q.Subject["to"]), q.Subject["kind"], quote(q.Subject["resource"]))
			return writeOverride(path, "", add, "")
		}
		return fmt.Errorf("a review answer is keep or drop")
	case "client": // kept in questions.yaml; the next build classifies the type's calls by it
		for _, k := range clientKinds {
			if value == k {
				return nil
			}
		}
		return fmt.Errorf("a client answer is one of: %s (or - if it isn't one)", strings.Join(clientKinds, ", "))
	case "resource":
		if strings.HasPrefix(value, "<") {
			return fmt.Errorf("a resource answer is the value itself, e.g. orders.created")
		}
		return nil
	}
	return fmt.Errorf("question type %q can't be answered", q.Type)
}

// writeOverride adds a node row and/or an edge row to edges.override.yaml.
// When a node with that id is already there, only the aliases are added to it.
func writeOverride(path, nodeRow, edgeRow, nodeID string, aliases ...string) error {
	o, err := LoadOverride(path)
	if err != nil {
		return err
	}
	prev := o
	var add struct {
		Nodes []map[string]any `yaml:"nodes"`
		Edges []map[string]any `yaml:"edges"`
	}
	if err := yaml.Unmarshal([]byte("nodes:\n"+nodeRow+"edges:\n"+edgeRow), &add); err != nil {
		return err
	}
	nodes, edges := []map[string]any{}, []map[string]any{}
	for _, n := range o.Nodes {
		if nodeID != "" && n.ID == nodeID {
			for _, alias := range aliases {
				if alias != "" && !contains(n.Aliases, alias) {
					n.Aliases = append(n.Aliases, alias)
				}
			}
			add.Nodes = nil // merged into the existing node
		}
		nodes = append(nodes, nodeMap(n))
	}
	for _, e := range o.Edges {
		edges = append(edges, edgeMap(e))
	}
	nodes, edges = append(nodes, add.Nodes...), append(edges, add.Edges...)
	body, err := yaml.Marshal(map[string]any{"version": FormatOverride, "nodes": nodes, "edges": edges})
	if err != nil {
		return err
	}
	head := OverrideTemplate
	if b, err := os.ReadFile(path); err == nil {
		head = leadingComments(string(b))
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(head+string(body)), 0o644); err != nil {
		return err
	}
	// the answer must not add a row the override would skip (rows already
	// skipped before it are the file's own problem, reported by the build)
	if o, err := LoadOverride(tmp); err != nil || len(o.Skipped) > len(prev.Skipped) {
		if err == nil {
			err = fmt.Errorf("%s", o.Skipped[len(o.Skipped)-1])
		}
		os.Remove(tmp)
		return fmt.Errorf("answer refused, the override would be invalid: %w", err)
	}
	return os.Rename(tmp, path)
}

func nodeMap(n ONode) map[string]any {
	m := map[string]any{"id": n.ID}
	if n.Type != "" {
		m["type"] = n.Type
	}
	if len(n.Props) > 0 {
		m["props"] = n.Props
	}
	if len(n.Aliases) > 0 {
		m["aliases"] = n.Aliases
	}
	return m
}

func edgeMap(e OEdge) map[string]any {
	m := map[string]any{"from": e.From, "to": e.To, "kind": e.Kind, "resource": e.Resource}
	if e.Op != "" {
		m["op"] = e.Op
	}
	if e.Call != "" {
		m["call"] = e.Call
	}
	return m
}

// leadingComments: the file's comment header, kept when weaver rewrites it.
func leadingComments(s string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		if !strings.HasPrefix(l, "#") && strings.TrimSpace(l) != "" {
			break
		}
		b.WriteString(l)
	}
	return b.String()
}

func quote(s string) string {
	b, _ := yaml.Marshal(s)
	return strings.TrimSpace(string(b))
}

// GapQuestion: a question from an analyzer's gap (docs/analyzer-spec.md §2),
// with its stable id.
func GapQuestion(typ string, subject map[string]string, ask, expect string, sites []string) Question {
	q := Question{Type: typ, Subject: subject, Ask: ask, Expect: expect, Sites: sites}
	q.ID = qid(q)
	return q
}

// ReviewQuestion: weave-ai disputes an edge (analyzer-spec §5). The edge stays
// in the map, call: unknown, until a person keeps or drops it.
func ReviewQuestion(e Edge, reason string, evidence []string) Question {
	q := Question{Type: "review", Subject: map[string]string{"edge": EdgeRef(e), "from": e.From, "to": e.To, "kind": e.Kind, "resource": e.Resource},
		Ask:    fmt.Sprintf("weave-ai disputes %s: %s", EdgeRef(e), reason),
		Expect: "keep (the edge is right) or drop (it isn't)", Sites: evidence}
	if len(q.Sites) > maxSites {
		q.Sites = q.Sites[:maxSites]
	}
	q.ID = qid(q)
	return q
}

// ReviewAnswers: answered review questions, edge reference -> keep or drop.
func (q *Questions) ReviewAnswers() map[string]string {
	m := map[string]string{}
	for _, it := range q.Questions {
		if it.Type == "review" && it.Answer != nil && (it.Status == "answered" || it.Status == "resolved") {
			m[it.Subject["edge"]] = it.Answer.Value
		}
	}
	return m
}
