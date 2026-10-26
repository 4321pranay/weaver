// Package facts is the contract between the language analyzers and the edge
// mapper (docs/analyzer-spec.md §2): per service, what it serves, what it
// calls, how its calls connect, the config it reads, and what couldn't be
// established. The mapper reads facts and nothing else of the analyzers.
package facts

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const Format = "weaver/facts/v1.0"

// Facts of one service in one build.
type Facts struct {
	Version     string   `yaml:"version"`
	Service     string   `yaml:"service"`
	Root        string   `yaml:"root,omitempty"`
	Run         string   `yaml:"run,omitempty"`
	Analyzer    Analyzer `yaml:"analyzer"`
	Entrypoints []Entry  `yaml:"entrypoints"`
	Calls       []Call   `yaml:"calls"`
	Callgraph   []Hop    `yaml:"callgraph"`
	Config      []Value  `yaml:"config"`
	Gaps        []Gap    `yaml:"gaps"`
}

// Analyzer: which analyzers produced the facts, e.g. weave-java.
type Analyzer struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version,omitempty"`
}

// Entry: something the service serves or reacts to.
type Entry struct {
	Kind     string `yaml:"kind"`
	Resource string `yaml:"resource"`
	At       string `yaml:"at,omitempty"` // path:line, 1-based
	Source   string `yaml:"source"`
}

// Call: one call to another system.
type Call struct {
	Kind     string   `yaml:"kind"`
	Role     string   `yaml:"role"`
	Resource Resource `yaml:"resource"`
	At       string   `yaml:"at,omitempty"`
	Host     string   `yaml:"host,omitempty"`
	Call     string   `yaml:"call,omitempty"` // criticality (edges-spec §4), "" when unsure
	Source   string   `yaml:"source"`
}

// Resource: what the call names, as the analyzer read it.
type Resource struct {
	Expr string `yaml:"expr"`
}

// Hop: a call site whose targets the source doesn't show (interfaces,
// injected beans, functions passed as values).
type Hop struct {
	File   string   `yaml:"file,omitempty"`
	Caller string   `yaml:"caller,omitempty"`
	Line   int      `yaml:"line"`
	To     []string `yaml:"to"`
	Source string   `yaml:"source"`
}

// Value: one config key the service's code reads.
type Value struct {
	Key    string `yaml:"key"`
	Value  string `yaml:"value"`
	Source string `yaml:"source"`
}

// Gap: what an analyzer couldn't establish, typed as a question.
type Gap struct {
	Type    string            `yaml:"type"`
	Subject map[string]string `yaml:"subject"`
	Ask     string            `yaml:"ask"`
	Expect  string            `yaml:"expect,omitempty"`
	Sites   []string          `yaml:"sites,omitempty"`
	Source  string            `yaml:"source"`
}

// At formats a 0-based line as path:line (1-based); "" for no single line.
func At(path string, line int) string {
	if path == "" || line < 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", path, line+1)
}

// Site splits an `at` back into a path and a 0-based line (-1 when none).
func Site(at string) (string, int) {
	i := strings.LastIndex(at, ":")
	if i < 0 {
		return at, -1
	}
	n, err := strconv.Atoi(at[i+1:])
	if err != nil {
		return at, -1
	}
	return at[:i], n - 1
}

// ConfigList: a config map as sorted facts.
func ConfigList(m map[string]string, source string) []Value {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Value, 0, len(keys))
	for _, k := range keys {
		out = append(out, Value{Key: k, Value: m[k], Source: source})
	}
	return out
}

// ConfigMap: the facts' config as a map.
func (f Facts) ConfigMap() map[string]string {
	m := map[string]string{}
	for _, v := range f.Config {
		m[v.Key] = v.Value
	}
	return m
}

// Write saves the facts as dir/<service>.yaml.
func Write(dir string, f Facts) error {
	f.Version = Format
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, f.Service+".yaml"), b, 0o644)
}

// Read loads dir/<service>.yaml.
func Read(dir, service string) (Facts, error) {
	var f Facts
	b, err := os.ReadFile(filepath.Join(dir, service+".yaml"))
	if err != nil {
		return f, err
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s: %w", service, err)
	}
	if f.Version != Format {
		return f, fmt.Errorf("%s: facts version %q, want %s", service, f.Version, Format)
	}
	return f, nil
}
