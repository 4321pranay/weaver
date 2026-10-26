package edgemap

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"weaver/internal/extract"
)

// Override is a domain's edges.override.yaml (spec §6.1): human corrections
// and answers, merged into the generated map on every build.
type Override struct {
	Nodes []ONode
	Edges []OEdge
	// Skipped: rows left out because they are invalid, each as file:line: reason.
	// A bad row never fails the build (edges-spec §6); it is reported instead.
	Skipped []string
	path    string
}

// ONode: id is required; everything else is optional.
type ONode struct {
	ID      string
	Type    string
	Props   map[string]any
	Aliases []string
	line    int
}

// OEdge: from, to, kind, resource are required; op and call optional.
type OEdge struct {
	From, To, Kind, Resource, Op, Call string
	line                               int
}

var (
	nodeTypes = map[string]bool{"service": true, "external": true, "kafka": true, "redis": true, "ceph": true, "database": true}
	edgeKinds = map[string]bool{"rest": true, "grpc": true, "kafka": true, "redis": true, "db": true, "s3": true}
	calls     = map[string]bool{"required": true, "optional": true, "fallback": true, "async": true, "ignore": true}
	// op by kind: the values allowed, and those that mean infra -> service
	opsOf = map[string][]string{
		"kafka": {"produce", "consume"},
		"redis": {"read", "write", "readwrite", "publish", "subscribe"},
		"db":    {"read", "write", "readwrite"},
		"s3":    {"read", "write", "readwrite"},
	}
	inbound = map[string]bool{"consume": true, "subscribe": true}
)

// OverrideTemplate is written once per domain, so the file is where people look.
const OverrideTemplate = `# Human corrections and answers for this domain's service map (docs/edges-spec.md §6).
# Merged into edges.yaml on every weaver build; never overwritten by weaver.
#
# nodes: add one, or patch a generated one (match on id or any alias).
#   An answer to "kafka address?" is a node whose aliases name the question:
#   the config key ("${KAFKA_BOOTSTRAP_SERVERS}") or "<type>@<service>".
# edges: match on from, to, kind, resource. A match changes its op / call;
#   no match adds the edge. call: ignore removes it.
#
# kind: rest | grpc | kafka | redis | db | s3
# op:   produce | consume (kafka) · read | write | readwrite (db, s3, redis keys)
#       publish | subscribe (redis pub/sub)
# call: required | optional | fallback | async | ignore
#
# version: weaver/edges.override/v1.0
# nodes:
#   - id: kafka/kafka-1.prod,kafka-2.prod
#     type: kafka
#     props: {brokers: [kafka-1.prod:9092, kafka-2.prod:9092]}
#     aliases: ["${KAFKA_BOOTSTRAP_SERVERS}", kafka@checkout]
# edges:
#   - {from: checkout, to: payment, kind: rest, resource: POST /pay, call: optional}
#   - {from: checkout, to: legacy, kind: rest, resource: GET /ping, call: ignore}
`

// LoadOverride reads and validates edges.override.yaml. A missing or
// comment-only file is an empty override. A file that can't be read as the
// schema is an error; an invalid row is skipped and listed in Skipped with its
// line, never dropped silently.
func LoadOverride(path string) (*Override, error) {
	o := &Override{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return o, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return o, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.SequenceNode {
		return nil, fmt.Errorf("%s:%d: old format (a list of src/dst/role rows); write `version: weaver/edges.override/v1.0`, `nodes:`, `edges:` with from/to rows (docs/edges-spec.md §6.1)", path, root.Line)
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s:%d: expected version, nodes, edges", path, root.Line)
	}
	bad := func(n *yaml.Node, f string, a ...any) error {
		return fmt.Errorf("%s:%d: %s", path, n.Line, fmt.Sprintf(f, a...))
	}
	skip := func(err error) { o.Skipped = append(o.Skipped, err.Error()) }
	for i := 0; i < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "version":
			if err := CheckFormat(v.Value, FormatOverride); err != nil {
				return nil, bad(v, "%v", err)
			}
		case "nodes", "edges":
			if v.Kind != yaml.SequenceNode {
				return nil, bad(v, "%s: expected a list", k.Value)
			}
			for _, item := range v.Content {
				if err := checkFields(item, k.Value, bad); err != nil {
					skip(err)
					continue
				}
				if k.Value == "nodes" {
					var n struct {
						ID, Type string
						Props    map[string]any
						Aliases  []string
					}
					if err := item.Decode(&n); err != nil {
						skip(bad(item, "%v", err))
						continue
					}
					switch {
					case n.ID == "":
						skip(bad(item, "a node needs id"))
						continue
					case strings.HasPrefix(n.ID, "unknown/") || n.Type == "unknown":
						skip(bad(item, "%s is a placeholder for an open question: answer it with weaver answer", n.ID))
						continue
					case n.Type != "" && !nodeTypes[n.Type]:
						skip(bad(item, "type %q: must be service, external, kafka, redis, ceph or database", n.Type))
						continue
					}
					o.Nodes = append(o.Nodes, ONode{n.ID, n.Type, n.Props, n.Aliases, item.Line})
					continue
				}
				var e struct{ From, To, Kind, Resource, Op, Call string }
				if err := item.Decode(&e); err != nil {
					skip(bad(item, "%v", err))
					continue
				}
				var why string
				switch {
				case e.From == "" || e.To == "" || e.Kind == "" || e.Resource == "":
					why = "an edge needs from, to, kind and resource"
				case strings.HasPrefix(e.From, "unknown/") || strings.HasPrefix(e.To, "unknown/"):
					why = "an edge can't point at a placeholder for an open question: answer it with weaver answer"
				case !edgeKinds[e.Kind]:
					why = fmt.Sprintf("kind %q: must be rest, grpc, kafka, redis, db or s3", e.Kind)
				case e.Call == "unknown":
					why = "call: unknown is generated only; say required, optional, fallback, async or ignore"
				case e.Call != "" && !calls[e.Call]:
					why = fmt.Sprintf("call %q: must be required, optional, fallback, async or ignore", e.Call)
				case e.Op != "" && !contains(opsOf[e.Kind], e.Op):
					why = fmt.Sprintf("op %q is not valid for kind %s (%s)", e.Op, e.Kind, strings.Join(opsOf[e.Kind], ", "))
				}
				if why != "" {
					skip(bad(item, "%s", why))
					continue
				}
				o.Edges = append(o.Edges, OEdge{e.From, e.To, e.Kind, e.Resource, e.Op, e.Call, item.Line})
			}
		default:
			return nil, bad(k, "unknown key %q (allowed: version, nodes, edges)", k.Value)
		}
	}
	o.duplicates()
	return o, nil
}

var allowed = map[string][]string{
	"nodes": {"id", "type", "props", "aliases"},
	"edges": {"from", "to", "kind", "resource", "op", "call"},
}

// checkFields: a row is a map of known fields; generated-only fields
// (callers, handler, match, resolvedBy) are refused by name.
func checkFields(item *yaml.Node, list string, bad func(*yaml.Node, string, ...any) error) error {
	if item.Kind != yaml.MappingNode {
		return bad(item, "each %s row is a map", list)
	}
	for i := 0; i < len(item.Content); i += 2 {
		f := item.Content[i].Value
		switch {
		case contains(allowed[list], f):
		case f == "callers" || f == "handler" || f == "match" || f == "resolvedBy":
			return bad(item.Content[i], "%s is generated only; the override can't set it", f)
		default:
			return bad(item.Content[i], "unknown field %q (allowed: %s)", f, strings.Join(allowed[list], ", "))
		}
	}
	return nil
}

// duplicates: one identity twice with different values, or one alias on two
// nodes (edges-spec §6): the later row is skipped, the first one kept.
func (o *Override) duplicates() {
	edges := map[[4]string]OEdge{}
	var keepE []OEdge
	for _, e := range o.Edges {
		res, _ := resource(e.Kind, e.Resource)
		k := [4]string{e.From, e.To, e.Kind, res}
		if p, ok := edges[k]; ok && (p.Op != e.Op || p.Call != e.Call) {
			o.Skipped = append(o.Skipped, fmt.Sprintf("%s:%d: edge %s also at line %d with other values", o.path, e.line, ref(k), p.line))
			continue
		}
		edges[k] = e
		keepE = append(keepE, e)
	}
	o.Edges = keepE
	owner := map[string]ONode{}
	var keepN []ONode
next:
	for _, n := range o.Nodes {
		for _, a := range append([]string{n.ID}, n.Aliases...) {
			if p, ok := owner[a]; ok && p.ID != n.ID {
				o.Skipped = append(o.Skipped, fmt.Sprintf("%s:%d: %q names both %s (line %d) and %s", o.path, n.line, a, p.ID, p.line, n.ID))
				continue next
			}
		}
		for _, a := range append([]string{n.ID}, n.Aliases...) {
			owner[a] = n
		}
		keepN = append(keepN, n)
	}
	o.Nodes = keepN
}

// aliasOwners: every override node alias -> its node id, for answers to
// address questions.
func (o *Override) aliasOwners() map[string]string {
	m := map[string]string{}
	for _, n := range o.Nodes {
		for _, a := range n.Aliases {
			m[a] = n.ID
		}
	}
	return m
}

// merge applies the override to the generated map (spec §6.2).
func (b *builder) merge() ([]string, error) {
	o := b.in.Override
	var warns []string
	at := func(line int) string { return fmt.Sprintf("%s:%d", o.path, line) }

	// nodes: match on id or alias, then patch; else add
	for _, on := range o.Nodes {
		names := map[string]bool{on.ID: true}
		for _, a := range on.Aliases {
			names[a] = true
		}
		var hit *Node
		for _, n := range b.nodes {
			if names[n.ID] || anyIn(n.Aliases, names) {
				hit = n
				break
			}
		}
		if hit == nil {
			if on.Type == "" {
				return nil, fmt.Errorf("%s: node %s matches no generated node, so it needs a type", at(on.line), on.ID)
			}
			b.nodes[on.ID] = &Node{ID: on.ID, Type: on.Type, Props: on.Props, Aliases: on.Aliases}
			continue
		}
		if hit.ID != on.ID {
			b.rename(hit.ID, on.ID)
		}
		if on.Type != "" {
			hit.Type = on.Type
		}
		for k, v := range on.Props {
			if hit.Props == nil {
				hit.Props = map[string]any{}
			}
			hit.Props[k] = v // `~` (nil) records "can't be known"
		}
		hit.Aliases = union(hit.Aliases, on.Aliases)
	}

	// edges: match on identity, then patch; else add
	for _, oe := range o.Edges {
		res, _ := resource(oe.Kind, oe.Resource)
		k := [4]string{oe.From, oe.To, oe.Kind, res}
		if ed, ok := b.edges[k]; ok {
			if oe.Call == "ignore" {
				delete(b.edges, k)
				continue
			}
			if oe.Op != "" {
				ed.Op = oe.Op
			}
			if oe.Call != "" {
				ed.Call = oe.Call
				for i := range ed.Callers {
					ed.Callers[i].Call = oe.Call
				}
			}
			continue
		}
		if oe.Call == "ignore" {
			warns = append(warns, fmt.Sprintf("%s: %s matches no edge; nothing to ignore", at(oe.line), ref(k)))
			continue
		}
		if oe.Op != "" || oe.Call != "" {
			warns = append(warns, fmt.Sprintf("%s: %s matches no generated edge; added (typo, or removed from code?)", at(oe.line), ref(k)))
		}
		for _, end := range []string{oe.From, oe.To} {
			if b.nodes[end] == nil {
				if b.in.DomainOf[end] == "" {
					return nil, fmt.Errorf("%s: %s is no node: not a service, and not a node in the map or the override", at(oe.line), end)
				}
				b.service(end)
			}
		}
		ed := b.edge(oe.From, oe.To, oe.Kind, res, oe.Op)
		ed.Call = oe.Call
	}

	// every edge's op agrees with its direction (spec §4.1); service <-> service has none
	var keys [][4]string
	for k := range b.edges {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return ref(keys[i]) < ref(keys[j]) })
	for _, k := range keys {
		e := b.edges[k]
		isInfra := func(id string) bool {
			n := b.nodes[id]
			if n.Type == "unknown" { // a placeholder is infrastructure when it stands for an instance
				k := fmt.Sprint(n.Props["kind"])
				return k != "rest" && k != "grpc"
			}
			return n.Type != "service" && n.Type != "external"
		}
		fi, ti := isInfra(e.From), isInfra(e.To)
		switch {
		case !fi && !ti && e.Op != "":
			return nil, fmt.Errorf("edge %s: an edge between services has no op", ref(k))
		case (fi || ti) && e.Op == "":
			return nil, fmt.Errorf("edge %s: an edge to or from infrastructure needs an op (%s)", ref(k), strings.Join(opsOf[e.Kind], ", "))
		case fi && !inbound[e.Op], ti && inbound[e.Op]:
			return nil, fmt.Errorf("edge %s: op %s contradicts its direction", ref(k), e.Op)
		}
	}
	return warns, nil
}

// rename moves a node to a new id and redirects its edges.
func (b *builder) rename(from, to string) {
	n := b.nodes[from]
	delete(b.nodes, from)
	n.Aliases = union(n.Aliases, []string{from})
	n.ID = to
	b.nodes[to] = n
	for k, e := range b.edges {
		if e.From != from && e.To != from {
			continue
		}
		delete(b.edges, k)
		if e.From == from {
			e.From = to
		}
		if e.To == from {
			e.To = to
		}
		nk := [4]string{e.From, e.To, e.Kind, e.Resource}
		b.edges[nk], b.sites[nk] = e, b.sites[k]
	}
}

// Rows: the override's edges as produce / consume rows, for the parts of weaver
// still built on them (kb.db service_edges, versions, graph.html).
func (o *Override) Rows(domainOf map[string]string) []extract.Edge {
	var out []extract.Edge
	for _, e := range o.Edges {
		if e.Call == "ignore" {
			continue
		}
		res, _ := resource(e.Kind, e.Resource)
		if domainOf[e.From] != "" {
			out = append(out, extract.Edge{Src: e.From, Kind: e.Kind, Channel: res, Role: "produce", Repo: e.From, Line: -1})
		}
		if domainOf[e.To] != "" {
			out = append(out, extract.Edge{Dst: e.To, Kind: e.Kind, Channel: res, Role: "consume", Repo: e.To, Line: -1})
		}
	}
	return out
}

func ref(k [4]string) string { return fmt.Sprintf("%s → %s %s %s", k[0], k[1], k[2], k[3]) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func anyIn(list []string, set map[string]bool) bool {
	for _, x := range list {
		if set[x] {
			return true
		}
	}
	return false
}
