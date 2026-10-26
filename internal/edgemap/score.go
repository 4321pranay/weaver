package edgemap

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Expected: a corpus's hand-verified edges (docs/analyzer-spec.md §6).
type Expected struct {
	Edges []struct {
		From, To, Kind, Resource string
	} `yaml:"edges"`
}

// KindScore: precision and recall of one edge kind.
type KindScore struct {
	Kind                   string
	Found, Expected, Right int
	Waiting                int // edges waiting on a question (call: unknown): not scored
	Missed, Extra          []string
}

// Score compares a built map with the expected edges, per kind. An edge waiting
// on a question (unknown/ end, call: unknown) is neither right nor wrong.
func Score(m Map, expectedPath string) ([]KindScore, error) {
	b, err := os.ReadFile(expectedPath)
	if err != nil {
		return nil, err
	}
	var x Expected
	if err := yaml.Unmarshal(b, &x); err != nil {
		return nil, fmt.Errorf("%s: %w", expectedPath, err)
	}
	by := map[string]*KindScore{}
	get := func(k string) *KindScore {
		if by[k] == nil {
			by[k] = &KindScore{Kind: k}
		}
		return by[k]
	}
	want := map[string]bool{}
	for _, e := range x.Edges {
		want[EdgeRef(Edge{From: e.From, To: e.To, Kind: e.Kind, Resource: e.Resource})] = true
		get(e.Kind).Expected++
	}
	have := map[string]bool{}
	for _, e := range m.Edges {
		s := get(e.Kind)
		if e.Question != "" || strings.HasPrefix(e.From, "unknown/") || strings.HasPrefix(e.To, "unknown/") {
			s.Waiting++
			continue
		}
		r := EdgeRef(e)
		have[r] = true
		s.Found++
		if want[r] {
			s.Right++
		} else {
			s.Extra = append(s.Extra, r)
		}
	}
	for _, e := range x.Edges {
		r := EdgeRef(Edge{From: e.From, To: e.To, Kind: e.Kind, Resource: e.Resource})
		if !have[r] {
			get(e.Kind).Missed = append(get(e.Kind).Missed, r)
		}
	}
	out := []KindScore{}
	for _, s := range by {
		sort.Strings(s.Missed)
		sort.Strings(s.Extra)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, nil
}
