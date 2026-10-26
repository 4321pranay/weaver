// Package snapshot versions the service graph across builds. Each build appends
// one JSON line to each domain's knowledge/<domain>/versions.jsonl: per-repo commit SHAs, per-service source
// hashes, domains, and the resolved edges tagged with producer/consumer repo, so
// a graph change traces to the commit that caused it. This file is the contract
// falcon reads.
//
// ponytail: append-only JSONL, full graph per line (~10KB/version at 50 repos);
// switch to per-edge deltas only if the log gets large.
package snapshot

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"weaver/internal/extract"
	"weaver/internal/graph"
)

type Snapshot struct {
	Version int64               `json:"version"`
	Run     string              `json:"run,omitempty"` // the build's run id (sources.go RunID)
	TS      string              `json:"ts"`
	Commits map[string]string   `json:"commits"` // repo -> short SHA
	Hashes  map[string]string   `json:"hashes"`  // repo -> source content hash
	Domains map[string][]string `json:"domains"` // domain -> services
	Edges   []graph.Hop         `json:"edges"`
}

// Manifest is one cloned service, as `weaver build` writes knowledge/<domain>/manifest.json.
type Manifest struct {
	Domain      string `json:"domain"`
	ServiceName string `json:"serviceName"`
	GitURL      string `json:"gitUrl"`
	Branch      string `json:"branch"`
	SHA         string `json:"sha"`
	Run         string `json:"run,omitempty"`   // the build that used it
	Root        string `json:"root,omitempty"`  // the service's folder in its repo (a monorepo); "" = the repo
	Clone       string `json:"clone,omitempty"` // the clone's folder when shared (a monorepo); "" = <service>
}

// Dir: the service's clone, sourcesDir/<service>, or the monorepo clone its
// services share. SCIP, call graphs and paths (always relative to it) see the
// whole repo, so a monorepo's shared code and build roots still resolve.
func (m Manifest) Dir(sourcesDir string) string {
	if m.Clone != "" {
		return filepath.Join(sourcesDir, m.Clone)
	}
	return filepath.Join(sourcesDir, m.ServiceName)
}

// Code: the service's own code, its root folder in the clone ("" = all of it):
// what its edges, config, release version and source hash come from.
func (m Manifest) Code(sourcesDir string) string {
	return filepath.Join(m.Dir(sourcesDir), m.Root)
}

// ReadManifest reads dir/manifest.json.
func ReadManifest(dir string) ([]Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m []Manifest
	return m, json.Unmarshal(b, &m)
}

// ServiceHash: digest of a service's source files (name + bytes, sorted), so a
// diff flags a service that changed INTERNALLY while its edges stayed put.
// ponytail: whole-dir digest, not per-method.
func ServiceHash(repoDir string) string {
	files := map[string][]string{}
	_ = filepath.WalkDir(repoDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && extract.IsSource(d.Name()) {
			dir := filepath.Dir(p)
			files[dir] = append(files[dir], d.Name())
		}
		return nil
	})
	dirs := make([]string, 0, len(files))
	for d := range files {
		if !strings.Contains(d, "/.git") && !strings.Contains(d, "/vendor/") && !strings.Contains(d, "/node_modules/") {
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	h := sha256.New()
	for _, d := range dirs {
		names := files[d]
		sort.Strings(names)
		for _, n := range names {
			if b, err := os.ReadFile(filepath.Join(d, n)); err == nil {
				h.Write([]byte(n))
				h.Write(b)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// RepoCommit: short HEAD SHA, or "nogit".
func RepoCommit(repoDir string) string {
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "--short", "HEAD").Output()
	if s := strings.TrimSpace(string(out)); err == nil && s != "" {
		return s
	}
	return "nogit"
}

// ForDomain snapshots one domain: commits + source hashes of its services
// (Manifest.Code), and every resolved hop touching one of them — hops are
// resolved across ALL domains first, so a call into another domain stays.
func ForDomain(domain string, manifest []Manifest, sourcesDir string, hops []graph.Hop) Snapshot {
	s := Snapshot{Commits: map[string]string{}, Hashes: map[string]string{},
		Domains: map[string][]string{}, Edges: []graph.Hop{}}
	mine := map[string]bool{}
	for _, m := range manifest {
		d := m.Code(sourcesDir)
		s.Commits[m.ServiceName] = RepoCommit(d)
		s.Hashes[m.ServiceName] = ServiceHash(d)
		s.Domains[domain] = append(s.Domains[domain], m.ServiceName)
		mine[m.ServiceName] = true
	}
	for _, h := range hops {
		if mine[h.SrcRepo] || mine[h.DstRepo] {
			s.Edges = append(s.Edges, h)
		}
	}
	now := time.Now()
	s.Version, s.TS = now.Unix(), now.Format("2006-01-02T15:04:05")
	return s
}

func Append(log string, s Snapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func LoadAll(log string) ([]Snapshot, error) {
	f, err := os.Open(log)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Snapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var s Snapshot
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			return nil, fmt.Errorf("%s: %w", log, err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

type Cause struct {
	Repo *string `json:"repo"` // null when neither side has a repo
	From string  `json:"from"`
	To   string  `json:"to"`
}

type Changed struct {
	Src         string `json:"src"`
	Dst         string `json:"dst"`
	Kind        string `json:"kind"`
	FromChannel string `json:"from_channel"`
	ToChannel   string `json:"to_channel"`
	Cause       Cause  `json:"cause"`
}

type EdgeChange struct {
	graph.Hop
	Cause Cause `json:"cause"`
}

type InternalChange struct {
	Service  string            `json:"service"`
	FromHash string            `json:"from_hash"`
	ToHash   string            `json:"to_hash"`
	Commit   map[string]string `json:"commit"`
}

type Ref struct {
	Version int64  `json:"version"`
	TS      string `json:"ts"`
}

type Diff struct {
	From     Ref              `json:"from"`
	To       Ref              `json:"to"`
	Added    []EdgeChange     `json:"added"`
	Removed  []EdgeChange     `json:"removed"`
	Changed  []Changed        `json:"changed"`
	Internal []InternalChange `json:"internal"` // changed inside, edges unchanged
}

func get(m map[string]string, k, def string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

// Compare diffs v1 -> v2: added / removed edges, changed (same src/dst/kind,
// channel moved), and internally-changed services (hash moved, no edge of theirs
// did). Each change names the repo whose commit moved.
func Compare(v1, v2 Snapshot) Diff {
	c1, c2 := v1.Commits, v2.Commits
	cause := func(srcRepo, dstRepo string) Cause {
		var ordered, movers []string
		for _, r := range []string{srcRepo, dstRepo} {
			if r != "" {
				ordered = append(ordered, r)
				if get(c1, r, "\x00") != get(c2, r, "\x00") {
					movers = append(movers, r)
				}
			}
		}
		pick := append(movers, ordered...)
		if len(pick) == 0 {
			return Cause{From: "-", To: "-"}
		}
		r := pick[0]
		return Cause{Repo: &r, From: get(c1, r, "-"), To: get(c2, r, "-")}
	}

	a, b := map[[4]string]graph.Hop{}, map[[4]string]graph.Hop{}
	for _, e := range v1.Edges {
		a[e.Key()] = e
	}
	for _, e := range v2.Edges {
		b[e.Key()] = e
	}
	minus := func(x, y map[[4]string]graph.Hop) []EdgeChange {
		var out []EdgeChange
		for k, e := range x {
			if _, ok := y[k]; !ok {
				out = append(out, EdgeChange{e, cause(e.SrcRepo, e.DstRepo)})
			}
		}
		sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Key(), out[j].Key()) })
		return out
	}
	added, removed := minus(b, a), minus(a, b)

	byTriple := func(edges []graph.Hop) map[[3]string]map[string]bool {
		m := map[[3]string]map[string]bool{}
		for _, e := range edges {
			t := [3]string{e.Src, e.Dst, e.Kind}
			if m[t] == nil {
				m[t] = map[string]bool{}
			}
			m[t][e.Channel] = true
		}
		return m
	}
	ta, tb := byTriple(v1.Edges), byTriple(v2.Edges)
	var triples [][3]string
	for t := range ta {
		if _, ok := tb[t]; ok {
			triples = append(triples, t)
		}
	}
	sort.Slice(triples, func(i, j int) bool {
		return strings.Join(triples[i][:], "\x00") < strings.Join(triples[j][:], "\x00")
	})
	changed := []Changed{}
	moved := map[[4]string]bool{}
	for _, t := range triples {
		gone, came := setMinus(ta[t], tb[t]), setMinus(tb[t], ta[t])
		for i := 0; i < len(gone) && i < len(came); i++ {
			changed = append(changed, Changed{Src: t[0], Dst: t[1], Kind: t[2], FromChannel: gone[i], ToChannel: came[i], Cause: cause(t[0], t[1])})
			moved[[4]string{t[0], t[1], t[2], gone[i]}] = true
			moved[[4]string{t[0], t[1], t[2], came[i]}] = true
		}
	}
	// a channel move already counted as changed isn't also an add + remove
	drop := func(es []EdgeChange) []EdgeChange {
		out := []EdgeChange{}
		for _, e := range es {
			if !moved[e.Key()] {
				out = append(out, e)
			}
		}
		return out
	}
	added, removed = drop(added), drop(removed)

	touched := map[string]bool{}
	for _, e := range append(append([]EdgeChange(nil), added...), removed...) {
		touched[e.Src], touched[e.Dst] = true, true
	}
	for _, e := range changed {
		touched[e.Src], touched[e.Dst] = true, true
	}
	var svcs []string
	for s := range v1.Hashes {
		if _, ok := v2.Hashes[s]; ok {
			svcs = append(svcs, s)
		}
	}
	sort.Strings(svcs)
	internal := []InternalChange{}
	for _, s := range svcs {
		if v1.Hashes[s] != v2.Hashes[s] && !touched[s] {
			internal = append(internal, InternalChange{Service: s, FromHash: v1.Hashes[s], ToHash: v2.Hashes[s],
				Commit: map[string]string{"from": get(v1.Commits, s, "-"), "to": get(v2.Commits, s, "-")}})
		}
	}
	return Diff{From: Ref{v1.Version, v1.TS}, To: Ref{v2.Version, v2.TS},
		Added: added, Removed: removed, Changed: changed, Internal: internal}
}

func setMinus(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func lessKey(a, b [4]string) bool {
	return strings.Join(a[:], "\x00") < strings.Join(b[:], "\x00")
}
