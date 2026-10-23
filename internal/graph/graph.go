// Package graph resolves raw produce/consume rows into service->service hops
// (a producer meets a consumer on the same kind + channel) and renders the
// {nodes, edges} JSON the graph UI reads.
package graph

import (
	"sort"

	"weaver/internal/extract"
)

// Hop is one resolved edge. SrcRepo/DstRepo carry provenance in snapshots.
type Hop struct {
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
	SrcRepo string `json:"src_repo,omitempty"`
	DstRepo string `json:"dst_repo,omitempty"`
}

func (h Hop) Key() [4]string { return [4]string{h.Src, h.Dst, h.Kind, h.Channel} }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// pairs calls fn for every producer x consumer on the same kind + channel.
func pairs(raw []extract.Edge, fn func(p, c extract.Edge)) {
	type kc struct{ kind, channel string }
	cons := map[kc][]extract.Edge{}
	for _, c := range raw {
		if c.Role == "consume" {
			k := kc{c.Kind, c.Channel}
			cons[k] = append(cons[k], c)
		}
	}
	for _, p := range raw {
		if p.Role == "produce" {
			for _, c := range cons[kc{p.Kind, p.Channel}] {
				fn(p, c)
			}
		}
	}
}

// Resolve: cross-service hops, deduped and sorted. An unknown side is "?";
// self-hops (src == dst) are internal and dropped — see ResolveInternal.
// ponytail: exact channel match only.
func Resolve(raw []extract.Edge) []Hop {
	seen := map[[4]string]bool{}
	var out []Hop
	pairs(raw, func(p, c extract.Edge) {
		h := Hop{Src: or(p.Src, "?"), Dst: or(c.Dst, "?"), Kind: p.Kind, Channel: p.Channel}
		if h.Src != h.Dst && !seen[h.Key()] {
			seen[h.Key()] = true
			out = append(out, h)
		}
	})
	sort.Slice(out, func(i, j int) bool { return less(out[i].Key(), out[j].Key()) })
	return out
}

// ResolveWithProvenance is Resolve keeping each side's repo, in pairing order.
func ResolveWithProvenance(raw []extract.Edge) []Hop {
	seen := map[[4]string]bool{}
	var out []Hop
	pairs(raw, func(p, c extract.Edge) {
		h := Hop{Src: or(p.Src, "?"), Dst: or(c.Dst, "?"), Kind: p.Kind, Channel: p.Channel, SrcRepo: p.Repo, DstRepo: c.Repo}
		if h.Src != h.Dst && !seen[h.Key()] {
			seen[h.Key()] = true
			out = append(out, h)
		}
	})
	return out
}

// Internal is a same-service dependency: a service producing AND consuming one
// channel (calls its own route, consumes its own topic). Kept apart from the
// cross-service graph — intra-service coupling is a different concern.
type Internal struct{ Service, Kind, Channel string }

func ResolveInternal(raw []extract.Edge) []Internal {
	seen := map[Internal]bool{}
	var out []Internal
	pairs(raw, func(p, c extract.Edge) {
		if s := or(p.Src, "?"); s != "?" && s == or(c.Dst, "?") {
			i := Internal{p.Src, p.Kind, p.Channel}
			if !seen[i] {
				seen[i] = true
				out = append(out, i)
			}
		}
	})
	return out
}

func less(a, b [4]string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

type Node struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Domain string `json:"domain"`
	Label  string `json:"label,omitempty"`
}

type Edge struct {
	Src       string  `json:"src"`
	Dst       string  `json:"dst"`
	Kind      string  `json:"kind"`
	Channel   string  `json:"channel"`
	Method    string  `json:"method"`
	Signature string  `json:"signature"`
	Contract  *string `json:"contract"` // request/response schema: not captured from source
}

type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// infraKind: transports through stateful middleware — with infra=true the channel
// becomes its own node (topic / store / table). rest + grpc stay direct.
var infraKind = map[string]bool{"kafka": true, "redis": true, "db": true}

// annotate fills method + signature. REST verbs come from restMethod (path ->
// verb, GET when unknown); bodies/schemas aren't captured, so contract is null.
func annotate(e Edge, restMethod map[string]string) Edge {
	ch := e.Channel
	switch e.Kind {
	case "rest":
		e.Method = or(restMethod[ch], "GET")
		e.Signature = e.Method + " " + ch
	case "grpc":
		e.Method, e.Signature = "rpc", ch+".<rpc>"
	case "kafka":
		e.Method, e.Signature = "publish/subscribe", "topic "+ch
	case "redis":
		e.Method, e.Signature = "GET/SET", "key "+ch
	case "db":
		e.Method, e.Signature = "SQL", "table "+ch
	}
	return e
}

// ToGraph renders hops for the UI. domainOf: service -> domain (may be nil).
func ToGraph(hops []Hop, infra bool, domainOf, restMethod map[string]string) Graph {
	nodes := map[string]Node{}
	for _, h := range hops {
		for _, s := range []string{h.Src, h.Dst} {
			nodes[s] = Node{ID: s, Type: "service", Domain: domainOf[s]}
		}
	}
	var edges []Edge
	seen := map[[4]string]bool{}
	add := func(src, dst, kind, ch string) {
		// one topic with N consumers resolves to N pairs whose producer->topic half
		// is identical — emit it once.
		k := [4]string{src, dst, kind, ch}
		if !seen[k] {
			seen[k] = true
			edges = append(edges, annotate(Edge{Src: src, Dst: dst, Kind: kind, Channel: ch}, restMethod))
		}
	}
	for _, h := range hops {
		if infra && infraKind[h.Kind] {
			id := h.Kind + ":" + h.Channel // A --produce--> [channel] --consume--> B
			if _, ok := nodes[id]; !ok {
				nodes[id] = Node{ID: id, Type: h.Kind, Label: h.Channel}
			}
			add(h.Src, id, h.Kind, h.Channel)
			add(id, h.Dst, h.Kind, h.Channel)
		} else {
			add(h.Src, h.Dst, h.Kind, h.Channel)
		}
	}
	g := Graph{Nodes: []Node{}, Edges: edges}
	for _, n := range nodes {
		g.Nodes = append(g.Nodes, n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].ID < g.Nodes[j].ID })
	if g.Edges == nil {
		g.Edges = []Edge{}
	}
	return g
}
