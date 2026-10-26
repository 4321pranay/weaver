package build

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"weaver/internal/edgemap"
	"weaver/internal/facts"
	"weaver/internal/scip"
	"weaver/internal/snapshot"

	"gopkg.in/yaml.v3"
)

// springProbe runs WEAVER_SPRING_PROBE (tools/spring-probe) on each Maven or
// Gradle build root of each service and returns the production-config keys it couldn't settle as
// `config` questions. spring-probe keeps no state of its own here: the
// service's answers, which live only in questions.yaml, go in on stdin
// (--answers -) and its capture, open keys included, comes back on stdout,
// both YAML like weaver's own files. Address keys are left out: weaver's own `address`
// questions cover instances. A failing probe is logged, never fails the build.
func springProbe(l Layout, d string, ms []snapshot.Manifest, qf *edgemap.Questions, log io.Writer) ([]edgemap.Question, map[string][]facts.Entry) {
	probe := os.Getenv("WEAVER_SPRING_PROBE")
	if probe == "" {
		return nil, nil
	}
	// a monorepo's services share a clone: each build root is captured once,
	// with all their answers, and each service takes its own apps' questions
	var clones []string
	byClone := map[string][]snapshot.Manifest{}
	for _, m := range ms {
		dir := m.Dir(l.Sources(d))
		if byClone[dir] == nil {
			clones = append(clones, dir)
		}
		byClone[dir] = append(byClone[dir], m)
	}
	var out []edgemap.Question
	entries := map[string][]facts.Entry{} // what Spring maps, per service: facts with source spring-probe:<level>
	for _, repo := range clones {
		svcs := byClone[repo]
		for _, r := range scip.Roots(repo) {
			if r.Tool != "scip-java" {
				continue
			}
			qs, eps, err := capture(probe, repo, r.Dir, svcs, qf, log)
			if err != nil {
				fmt.Fprintf(log, "  !! %s: spring-probe capture %s: %v\n", svcs[0].ServiceName, filepath.Join(".", r.Dir), err)
				continue
			}
			for _, m := range svcs {
				mine := serviceQuestions(repo, r.Dir, m, qs)
				fmt.Fprintf(log, "  %s: spring-probe captured %s, %d config questions open\n", m.ServiceName, filepath.Join(".", r.Dir), len(mine))
				out = append(out, mine...)
				entries[m.ServiceName] = append(entries[m.ServiceName], serviceEntries(r.Dir, m, eps)...)
			}
		}
	}
	return out, entries
}

// probeEntry: one entry point as Spring maps it (spring-probe's capture).
type probeEntry struct {
	App      string `yaml:"app"`
	Kind     string `yaml:"kind"`
	Resource string `yaml:"resource"`
	Source   string `yaml:"source"`
}

// serviceEntries: the entry points of a service's own apps, as facts. A REST
// route is its path ("GET /owners" -> /owners), as weaver's routes are; only
// the kinds the map matches on (rest, grpc, kafka) are kept.
func serviceEntries(rel string, m snapshot.Manifest, eps []probeEntry) []facts.Entry {
	var out []facts.Entry
	for _, e := range eps {
		app := filepath.ToSlash(filepath.Join(rel, e.App))
		if m.Root != "" && app != m.Root && !strings.HasPrefix(app, m.Root+"/") && !strings.HasPrefix(m.Root, app+"/") {
			continue
		}
		res := e.Resource
		switch e.Kind {
		case "rest":
			if _, path, ok := strings.Cut(res, " "); ok {
				res = path
			}
		case "grpc", "kafka":
		default:
			continue
		}
		out = append(out, facts.Entry{Kind: e.Kind, Resource: res, Source: "spring-probe:" + e.Source})
	}
	return out
}

// probeQ: one of spring-probe's open production-config questions.
type probeQ struct {
	App     string   `yaml:"app"`
	Key     string   `yaml:"key"`
	Kind    string   `yaml:"kind"`
	Status  string   `yaml:"status"`
	Options []string `yaml:"options"`
	UsedBy  []string `yaml:"usedBy"`
	Apps    []string `yaml:"apps"` // where a repo-level (grouped) question came from
}

// capture runs spring-probe on one Java build root of a clone: the repo itself
// (rel "") or a folder in it, for a repo whose Maven or Gradle project isn't at
// the top. Answers name apps from the repo root; spring-probe, run on the
// folder, takes them with the folder prefix stripped.
func capture(probe, repo, rel string, svcs []snapshot.Manifest, qf *edgemap.Questions, log io.Writer) ([]probeQ, []probeEntry, error) {
	ans := map[string]edgemap.Answered{}
	for _, m := range svcs {
		for k, v := range qf.ConfigAnswers(m.ServiceName) {
			app, key, ok := strings.Cut(k, ":")
			switch {
			case !ok: // the whole repo
				ans[k] = v
			case rel == "" || app == rel:
				ans[strings.TrimPrefix(k, rel+":")] = v
			case strings.HasPrefix(app, rel+"/"):
				ans[strings.TrimPrefix(app, rel+"/")+":"+key] = v
			}
		}
	}
	in, _ := yaml.Marshal(ans)
	m := svcs[0]
	cmd := exec.Command(probe, "capture", filepath.Join(repo, rel), "--service", m.ServiceName, "--commit", m.SHA, "--run", m.Run, "--answers", "-")
	cmd.Stdin, cmd.Stderr = bytes.NewReader(in), log
	var res struct {
		Questions   []probeQ     `yaml:"questions"`
		Entrypoints []probeEntry `yaml:"entrypoints"`
	}
	b, err := cmd.Output()
	if err == nil {
		err = yaml.Unmarshal(b, &res)
	}
	return res.Questions, res.Entrypoints, err
}

// serviceQuestions: the open questions of one service. A monorepo service
// (root set) takes only its own apps' questions, asked as its app's.
func serviceQuestions(repo, rel string, m snapshot.Manifest, qs []probeQ) []edgemap.Question {
	var out []edgemap.Question
	for _, q := range qs {
		if q.Status == "answered" || q.Kind == "address" {
			continue
		}
		app := filepath.ToSlash(filepath.Join(rel, q.App)) // "" / "." both mean the root
		if m.Root != "" {
			apps := q.Apps
			if len(apps) == 0 {
				apps = []string{q.App}
			}
			mine := false
			for _, a := range apps {
				a = filepath.ToSlash(filepath.Join(rel, a))
				mine = mine || a == m.Root || strings.HasPrefix(a, m.Root+"/") || strings.HasPrefix(m.Root, a+"/")
			}
			if !mine {
				continue
			}
			app = m.Root
		}
		var sites []string
		for _, u := range q.UsedBy {
			if s := probeSite(filepath.Join(repo, app), q.Key, u); s != "" {
				sites = append(sites, m.ServiceName+":"+filepath.ToSlash(filepath.Join(app, s)))
			}
		}
		out = append(out, edgemap.ConfigQuestion(m.ServiceName, app, q.Key, q.Kind, q.Options, sites))
	}
	return out
}

// probeSite turns one of spring-probe's usedBy entries into <path>:<line> in
// the repo: "[kind ]com.x.Class[#method]" is the class's source file, a file
// name is that file; the line is the first that names the key (its last part
// in YAML), else the method, else the class. "" when there is no source.
// ponytail: text search, not SCIP; good enough to point a person at the code.
func probeSite(repo, key, used string) string {
	if i := strings.LastIndex(used, " "); i >= 0 {
		used = used[i+1:]
	}
	class, method, _ := strings.Cut(used, "#")
	method, _, _ = strings.Cut(method, "(")
	want := filepath.Base(class)
	looks := []string{key}
	if strings.HasSuffix(class, ".java") || strings.Contains(class, "/") || !strings.Contains(class, ".") ||
		strings.HasSuffix(class, ".yml") || strings.HasSuffix(class, ".yaml") || strings.HasSuffix(class, ".properties") {
		looks = append(looks, key[strings.LastIndex(key, ".")+1:]+":")
	} else {
		if j := strings.Index(class, "$"); j >= 0 { // nested class: the outer file
			class = class[:j]
		}
		want = strings.ReplaceAll(class, ".", "/") + ".java"
		simple := class[strings.LastIndex(class, ".")+1:]
		if method != "" && method != "<init>" {
			looks = append(looks, " "+method+"(")
		}
		looks = append(looks, "class "+simple, "interface "+simple)
	}
	var file string
	_ = filepath.WalkDir(repo, func(p string, e os.DirEntry, err error) error {
		if err != nil || file != "" {
			return filepath.SkipDir
		}
		if e.IsDir() && (e.Name() == "target" || e.Name() == "build" || strings.HasPrefix(e.Name(), ".")) && p != repo {
			return filepath.SkipDir
		}
		if !e.IsDir() && strings.HasSuffix(filepath.ToSlash(p), "/"+want) && !strings.Contains(p, "/test/") {
			file = p
		}
		return nil
	})
	if file == "" {
		return ""
	}
	rel, _ := filepath.Rel(repo, file)
	for _, look := range looks {
		if n := lineOf(file, look); n > 0 {
			return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), n)
		}
	}
	return filepath.ToSlash(rel) + ":1"
}

func lineOf(path, s string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.Contains(sc.Text(), s) {
			return n
		}
	}
	return 0
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
