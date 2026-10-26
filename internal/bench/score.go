// Package bench scores the edge extractor against hand-labeled ground truth, and
// generates the synthetic benchmark corpora.
//
// Score runs the extractor over <dir>/services, resolves producer<->consumer
// pairs, and compares to <dir>/ground_truth.yaml: precision, recall on catchable
// edges, recall per difficulty tier, and junk (raw channels in no truth edge).
// `runtime`-tier edges are expected static misses, reported apart.
package bench

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"weaver/internal/extract"
	"weaver/internal/graph"
)

type Truth struct{ Src, Dst, Kind, Channel, Tier string }

func (t Truth) key() [4]string { return [4]string{t.Src, t.Dst, t.Kind, t.Channel} }

// LoadGroundTruth parses the flat `- key: val` blocks under `edges:`.
func LoadGroundTruth(path string) ([]Truth, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []map[string]string
	cur := map[string]string{}
	inEdges := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.TrimRight(sc.Text(), " \t\r\n")
		t := strings.TrimSpace(s)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(s, "edges:") {
			inEdges = true
			continue
		}
		if !inEdges {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(s, " \t"), "- ") {
			if len(cur) > 0 {
				rows = append(rows, cur)
			}
			cur = map[string]string{}
			s = strings.Replace(s, "- ", "  ", 1)
		}
		if k, v, ok := strings.Cut(strings.TrimSpace(s), ":"); ok {
			cur[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	if len(cur) > 0 {
		rows = append(rows, cur)
	}
	out := make([]Truth, 0, len(rows))
	for _, r := range rows {
		out = append(out, Truth{r["src"], r["dst"], r["kind"], r["channel"], r["tier"]})
	}
	return out, sc.Err()
}

type Result struct {
	Pass               bool
	Predicted, TruthN  int
	Correct, False     int
	Catchable, Caught  int
	Junk               int
	Tiers              map[string][2]int // tier -> {got, total}
	Missed, FalseEdges []string
	JunkLines          []string
}

// Score runs the extractor over dir/services against dir/ground_truth.yaml.
func Score(x *extract.Extractor, dir string) (Result, error) {
	truth, err := LoadGroundTruth(filepath.Join(dir, "ground_truth.yaml"))
	if err != nil {
		return Result{}, err
	}
	raw := x.ExtractAll(filepath.Join(dir, "services"))
	predicted := map[[4]string]bool{}
	for _, h := range graph.Resolve(raw) {
		k := h.Key()
		if strings.HasPrefix(k[3], "<") { // unresolved on both sides: an honest miss, never a claimed edge
			continue
		}
		k[3] = prefixForm(k[2], k[3])
		predicted[k] = true
	}
	truthKeys, tierOf := map[[4]string]bool{}, map[[4]string]string{}
	truthCh := map[[2]string]bool{}
	for _, t := range truth {
		truthKeys[t.key()] = true
		tierOf[t.key()] = t.Tier
		truthCh[[2]string{t.Kind, t.Channel}] = true
	}

	r := Result{Predicted: len(predicted), TruthN: len(truthKeys), Tiers: map[string][2]int{}}
	for k := range predicted {
		if truthKeys[k] {
			r.Correct++
		} else {
			r.False++
			r.FalseEdges = append(r.FalseEdges, fmt.Sprintf("  %s -> %s (%s %s)", k[0], k[1], k[2], k[3]))
		}
	}
	var keys [][4]string
	for k := range truthKeys {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return lessKey(keys[i], keys[j]) })
	for _, k := range keys {
		got := predicted[k]
		t := r.Tiers[tierOf[k]]
		if got {
			t[0]++
		}
		t[1]++
		r.Tiers[tierOf[k]] = t
		// catchable: resolvable service->service — not runtime tier, both ends named
		if tierOf[k] != "runtime" && k[0] != "" && k[1] != "" {
			r.Catchable++
			if got {
				r.Caught++
			}
		}
		if !got {
			r.Missed = append(r.Missed, fmt.Sprintf("  [%-8s] %s -> %s (%s %s)", tierOf[k], k[0], k[1], k[2], k[3]))
		}
	}
	// junk: a raw channel in no truth edge of its kind. <RUNTIME / <DYNAMIC markers
	// are honest misses, not junk.
	junk := map[string]bool{}
	for _, e := range raw {
		if !strings.Contains(e.Channel, "<RUNTIME") && !strings.Contains(e.Channel, "<DYNAMIC") && !truthCh[[2]string{e.Kind, prefixForm(e.Kind, e.Channel)}] {
			r.Junk++
			junk[fmt.Sprintf("  %s %s (%s)", e.Kind, e.Channel, e.Role)] = true
		}
	}
	for j := range junk {
		r.JunkLines = append(r.JunkLines, j)
	}
	sort.Strings(r.FalseEdges)
	sort.Strings(r.JunkLines)
	r.Pass = r.False == 0 && r.Junk == 0 && r.Caught == r.Catchable
	return r, nil
}

func pct(a, b int) string {
	if b == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(a)/float64(b))
}

// Print writes the human report.
func (r Result) Print(w io.Writer) {
	fmt.Fprintf(w, "predicted edges: %d   ground truth: %d\n", r.Predicted, r.TruthN)
	fmt.Fprintf(w, "precision:            %s  (%d correct / %d false)\n", pct(r.Correct, r.Predicted), r.Correct, r.False)
	fmt.Fprintf(w, "recall (catchable):   %s  (excludes runtime-tier)\n", pct(r.Caught, r.Catchable))
	fmt.Fprintln(w, "\nby tier (recall):")
	for _, t := range []string{"symbol", "config", "literal", "template", "runtime"} {
		if v, ok := r.Tiers[t]; ok {
			note := ""
			if t == "runtime" {
				note = "  <- expected static miss"
			}
			fmt.Fprintf(w, "  %-9s %d/%d%s\n", t, v[0], v[1], note)
		}
	}
	section := func(title string, lines []string) {
		if len(lines) > 0 {
			fmt.Fprintln(w, "\n"+title)
			for _, l := range lines {
				fmt.Fprintln(w, l)
			}
		}
	}
	section("MISSED edges:", r.Missed)
	section("FALSE edges:", r.FalseEdges)
	section(fmt.Sprintf("JUNK extractions (%d) — channel not in any truth edge:", r.Junk), r.JunkLines)
	verdict := "FAIL"
	if r.Pass {
		verdict = "PASS"
	}
	fmt.Fprintf(w, "\n%s: catchable=%d/%d, false=%d, junk=%d\n", verdict, r.Caught, r.Catchable, r.False, r.Junk)
}

func lessKey(a, b [4]string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// prefixForm: the ground truth writes redis keys as prefixes (`session:`); the
// extractor keeps the key pattern (`session:{}`, edges spec §4.2).
func prefixForm(kind, ch string) string {
	if kind == "redis" {
		return strings.TrimSuffix(ch, "{}")
	}
	return ch
}
