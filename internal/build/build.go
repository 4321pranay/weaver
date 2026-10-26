// Package build clones every service in sources/<domain>/sources.yaml at its branch
// into sources/<domain>/, and builds
// weaver's static knowledge into the knowledge folder, one folder per domain:
// kb.db (SCIP symbols, service edges, domains table), graph versions, and the
// Zoekt code-search index falcon queries.
package build

import (
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"weaver/internal/edgemap"
	"weaver/internal/extract"
	"weaver/internal/facts"
	"weaver/internal/graph"
	"weaver/internal/scip"
	"weaver/internal/search"
	"weaver/internal/snapshot"
	"weaver/internal/ui"
	"weaver/internal/weaveai"
)

// Service is one entry under `services:` in a sources.yaml. Root, optional, is
// the service's folder in its repo: a monorepo is listed once per service, each
// entry with the same gitUrl and its own root.
type Service struct {
	Domain, ServiceName, GitURL, Branch, Root string
}

// Clone: the folder under sources/<domain> the service's repo is cloned into.
// A service without root has its own, named after it; services with a root
// share one clone per gitUrl + branch (.repo-<name>-<hash>), so a monorepo is
// cloned, indexed and captured once, not once per service.
func (s Service) Clone() string {
	if s.Root == "" {
		return s.ServiceName
	}
	name := strings.TrimSuffix(s.GitURL[strings.LastIndexAny(s.GitURL, "/:")+1:], ".git")
	if !safeName.MatchString(name) {
		name = "repo"
	}
	h := sha1.Sum([]byte(s.GitURL + "\x00" + s.Branch))
	return fmt.Sprintf(".repo-%s-%x", name, h[:4])
}

// LoadSources reads one domain's sources/<domain>/sources.yaml; the folder
// names the domain:
//
//	services:
//	  - serviceName: payment-gateway
//	    gitUrl: https://bitbucket.internal/scm/pay/payment-gateway.git
//	    branch: release/2.4
//	    root: services/gateway   # optional: the service's folder in a monorepo
func LoadSources(path, domain string) ([]Service, error) {
	if !safeName.MatchString(domain) || domain == "." || domain == ".." {
		return nil, fmt.Errorf("domain %q: use letters, digits, '.', '_' or '-' (it is a folder name)", domain)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Services []struct {
			ServiceName string `yaml:"serviceName"`
			GitURL      string `yaml:"gitUrl"`
			Branch      string `yaml:"branch"`
			Root        string `yaml:"root"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []Service
	seen := map[string]bool{}
	for _, s := range cfg.Services {
		if s.ServiceName == "" || s.GitURL == "" || s.Branch == "" {
			return nil, fmt.Errorf("%s: every service needs serviceName, gitUrl and branch (got %q)", path, s.ServiceName)
		}
		if !safeName.MatchString(s.ServiceName) || s.ServiceName == "." || s.ServiceName == ".." {
			return nil, fmt.Errorf("%s: serviceName %q: use letters, digits, '.', '_' or '-' (it becomes a folder name)", path, s.ServiceName)
		}
		if !gitURL.MatchString(s.GitURL) {
			return nil, fmt.Errorf("%s: %s: gitUrl %q must start with https://, http://, ssh://, git@ or file://", path, s.ServiceName, s.GitURL)
		}
		if strings.HasPrefix(s.Branch, "-") {
			return nil, fmt.Errorf("%s: %s: branch %q can't start with '-'", path, s.ServiceName, s.Branch)
		}
		if seen[s.ServiceName] {
			return nil, fmt.Errorf("%s: serviceName %s listed twice", path, s.ServiceName)
		}
		root := filepath.ToSlash(filepath.Clean(strings.TrimSpace(s.Root)))
		if root == "." {
			root = ""
		}
		if filepath.IsAbs(root) || root == ".." || strings.HasPrefix(root, "../") {
			return nil, fmt.Errorf("%s: %s: root %q must be a folder inside the repo", path, s.ServiceName, s.Root)
		}
		seen[s.ServiceName] = true
		out = append(out, Service{domain, s.ServiceName, s.GitURL, strings.TrimPrefix(s.Branch, "refs/heads/"), root})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no services under `services:`", path)
	}
	return out, nil
}

var (
	safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	// a git URL, never something git would parse as an option
	gitURL = regexp.MustCompile(`^(https?://|ssh://|file://|git@)[^\s]+$`)
)

// askpass answers git's prompts from env, so credentials never sit in a URL,
// .git/config or the process list.
const askpass = "#!/bin/sh\ncase \"$1\" in Username*) echo \"$WEAVER_GIT_USER\";; *) echo \"$WEAVER_GIT_PASS\";; esac\n"

type git struct{ env []string }

func (g git) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = g.env
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}

// Sync clones each service fresh (depth 1) at its configured branch into
// sourcesDir/<Service.Clone> and drops clones of services no longer configured. A
// service whose branch doesn't exist, or whose clone fails, is logged and skipped.
func Sync(services []Service, sourcesDir, user, password string, log io.Writer) ([]snapshot.Manifest, error) {
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "weaver-askpass")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	ap := filepath.Join(tmp, "askpass.sh")
	if err := os.WriteFile(ap, []byte(askpass), 0o700); err != nil {
		return nil, err
	}
	g := git{append(os.Environ(), "GIT_ASKPASS="+ap, "GIT_TERMINAL_PROMPT=0",
		"WEAVER_GIT_USER="+user, "WEAVER_GIT_PASS="+password)}

	done := []snapshot.Manifest{}
	configured := map[string]bool{}
	cloned := map[string]snapshot.Manifest{} // a shared clone, fetched once per build
	for _, s := range services {
		configured[s.Clone()] = true
		var m snapshot.Manifest
		var err error
		if c, ok := cloned[s.Clone()]; ok {
			m, err = sharedClone(c, s, sourcesDir)
		} else if m, err = cloneBranch(g, s, sourcesDir); err == nil {
			cloned[s.Clone()] = m
		}
		if err != nil {
			// a failed fetch never costs the last good copy: build from it, loudly
			if prev := Scan([]Service{s}, sourcesDir, io.Discard); len(prev) == 1 {
				fmt.Fprintf(log, "  !! %s: %v — using the previous clone (%s @ %.10s)\n", s.ServiceName, err, prev[0].Branch, prev[0].SHA)
				done = append(done, prev[0])
			} else {
				fmt.Fprintf(log, "  !! %s: %v\n", s.ServiceName, err)
			}
			continue
		}
		fmt.Fprintf(log, ">> %s: %s @ %s\n", s.ServiceName, m.Branch, m.SHA[:10])
		done = append(done, m)
	}
	// prune only services REMOVED from sources.yaml, never ones that failed
	ents, _ := os.ReadDir(sourcesDir)
	for _, e := range ents {
		if e.IsDir() && !configured[e.Name()] && (!strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), ".repo-")) {
			_ = os.RemoveAll(filepath.Join(sourcesDir, e.Name()))
		}
	}
	return done, nil
}

// cloneBranch clones into a temp folder next to the destination and swaps it in
// only once the clone succeeded, so a failure leaves the previous copy intact.
func cloneBranch(g git, s Service, sourcesDir string) (snapshot.Manifest, error) {
	// check first: a missing branch is a config mistake worth naming, not a clone error
	// ("--" ends git's options: a URL can never be read as one)
	heads, err := g.run("ls-remote", "--heads", "--", s.GitURL, "refs/heads/"+s.Branch)
	if err != nil {
		return snapshot.Manifest{}, err
	}
	if strings.TrimSpace(heads) == "" {
		return snapshot.Manifest{}, fmt.Errorf("branch %s not found, skipped", s.Branch)
	}
	branch := s.Branch
	dest := filepath.Join(sourcesDir, s.Clone())
	tmp := filepath.Join(sourcesDir, ".clone-"+strings.TrimPrefix(s.Clone(), "."))
	_ = os.RemoveAll(tmp)
	if _, err := g.run("clone", "--quiet", "--depth", "1", "--branch", branch, "--", s.GitURL, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return snapshot.Manifest{}, err
	}
	sha, err := g.run("-C", tmp, "rev-parse", "HEAD")
	if err != nil {
		_ = os.RemoveAll(tmp)
		return snapshot.Manifest{}, err
	}
	if st, err := os.Stat(filepath.Join(tmp, s.Root)); s.Root != "" && (err != nil || !st.IsDir()) {
		_ = os.RemoveAll(tmp)
		return snapshot.Manifest{}, fmt.Errorf("root %s not in the repo at %s, skipped", s.Root, branch)
	}
	if err := os.RemoveAll(dest); err != nil {
		return snapshot.Manifest{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return snapshot.Manifest{}, err
	}
	return snapshot.Manifest{Domain: s.Domain, ServiceName: s.ServiceName, GitURL: s.GitURL,
		Branch: branch, SHA: strings.TrimSpace(sha), Root: s.Root, Clone: clonedAs(s)}, nil
}

// sharedClone: another service of a clone fetched earlier in this build.
func sharedClone(c snapshot.Manifest, s Service, sourcesDir string) (snapshot.Manifest, error) {
	if st, err := os.Stat(filepath.Join(sourcesDir, s.Clone(), s.Root)); err != nil || !st.IsDir() {
		return snapshot.Manifest{}, fmt.Errorf("root %s not in the repo at %s, skipped", s.Root, c.Branch)
	}
	return snapshot.Manifest{Domain: s.Domain, ServiceName: s.ServiceName, GitURL: s.GitURL,
		Branch: c.Branch, SHA: c.SHA, Root: s.Root, Clone: clonedAs(s)}, nil
}

// clonedAs: Manifest.Clone, "" when the clone is the service's own folder.
func clonedAs(s Service) string {
	if c := s.Clone(); c != s.ServiceName {
		return c
	}
	return ""
}

// Scan reads what is actually in sourcesDir for each configured service — the
// facts --no-fetch builds from. A git checkout reports its own branch + commit
// (a branch other than the configured one is warned about); a plain copy gets
// the configured branch and commit "nogit"; a missing folder is reported and
// skipped. Only <service>/.git counts: git would otherwise walk up to a parent repo.
func Scan(services []Service, sourcesDir string, log io.Writer) []snapshot.Manifest {
	g := git{os.Environ()}
	out := []snapshot.Manifest{}
	for _, s := range services {
		dir := filepath.Join(sourcesDir, s.Clone())
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			fmt.Fprintf(log, "  !! %s: not in %s, skipped\n", s.ServiceName, sourcesDir)
			continue
		}
		if st, err := os.Stat(filepath.Join(dir, s.Root)); s.Root != "" && (err != nil || !st.IsDir()) {
			fmt.Fprintf(log, "  !! %s: root %s not in %s, skipped\n", s.ServiceName, s.Root, dir)
			continue
		}
		m := snapshot.Manifest{Domain: s.Domain, ServiceName: s.ServiceName, GitURL: s.GitURL, Branch: s.Branch, SHA: "nogit", Root: s.Root, Clone: clonedAs(s)}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			if b, err := g.run("-C", dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
				if b = strings.TrimSpace(b); b != s.Branch {
					fmt.Fprintf(log, "  !! %s: checked out on %s, sources.yaml says %s\n", s.ServiceName, b, s.Branch)
				}
				m.Branch = b
			}
			if sha, err := g.run("-C", dir, "rev-parse", "HEAD"); err == nil {
				m.SHA = strings.TrimSpace(sha)
			}
		}
		fmt.Fprintf(log, ">> %s: %s @ %.10s (existing source)\n", s.ServiceName, m.Branch, m.SHA)
		out = append(out, m)
	}
	return out
}

// LoadDomains replaces the domains table in kb.
func LoadDomains(kb *sql.DB, manifest []snapshot.Manifest) error {
	for _, q := range []string{"DROP TABLE IF EXISTS domains",
		"CREATE TABLE domains (domain TEXT, service TEXT, git_url TEXT, branch TEXT, sha TEXT, run TEXT)"} {
		if _, err := kb.Exec(q); err != nil {
			return err
		}
	}
	for _, m := range manifest {
		if _, err := kb.Exec("INSERT INTO domains VALUES (?,?,?,?,?,?)", m.Domain, m.ServiceName, m.GitURL, m.Branch, m.SHA, m.Run); err != nil {
			return err
		}
	}
	return nil
}

// Layout: the sources weaver reads (input) and the knowledge folder it writes
// (output), each with one folder per domain.
//
//	<sources>/<domain>/sources.yaml      the domain's services: name, git url, branch
//	<sources>/<domain>/<service>/        clones at each service's branch
//	<root>/<domain>/kb.db                SCIP symbols + service edges + domains table
//	<root>/<domain>/zoekt/               code-search shards
//	<root>/<domain>/manifest.json        service -> git url, branch, sha
//	<root>/<domain>/versions.jsonl       graph versions (falcon's drift input)
//	<root>/<domain>/edges.override.yaml  hand-listed runtime-only edges (yours to edit)
//	<root>/<domain>/graph.html           service map page (only with Options.Graph)
//	<root>/<domain>/scip/                per-repo .scip / .db (intermediate)
type Layout struct{ Root, SourcesRoot string }

func (l Layout) Domain(d string) string  { return filepath.Join(l.Root, d) }
func (l Layout) Sources(d string) string { return filepath.Join(l.SourcesRoot, d) }
func (l Layout) SourcesFile(d string) string {
	return filepath.Join(l.SourcesRoot, d, "sources.yaml")
}

// Configured lists the domains that have a sources/<domain>/sources.yaml, sorted.
func (l Layout) Configured() []string {
	fs, _ := filepath.Glob(filepath.Join(l.SourcesRoot, "*", "sources.yaml"))
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, filepath.Base(filepath.Dir(f)))
	}
	return out
}
func (l Layout) Scip(d string) string  { return filepath.Join(l.Root, d, "scip") }
func (l Layout) KB(d string) string    { return filepath.Join(l.Root, d, "kb.db") }
func (l Layout) Zoekt(d string) string { return filepath.Join(l.Root, d, "zoekt") }
func (l Layout) Versions(d string) string {
	return filepath.Join(l.Root, d, "versions.jsonl")
}

// A build writes its map, history, releases and page into a new generation,
// knowledge/<domain>/generations/<n>/, and switches knowledge/<domain>/current
// to it at the end (weaver-architecture §2). Readers go through current; a
// domain built before generations reads its old top-level files.
func (l Layout) Generations(d string) string { return filepath.Join(l.Root, d, "generations") }
func (l Layout) Gen(d string, n int) string {
	return filepath.Join(l.Generations(d), strconv.Itoa(n))
}
func (l Layout) Current(d string) string { return filepath.Join(l.Root, d, "current") }

// read: name in the current generation, or the domain folder before generations.
func (l Layout) read(d, name string) string {
	if _, err := os.Stat(filepath.Join(l.Current(d), name)); err == nil {
		return filepath.Join(l.Current(d), name)
	}
	return filepath.Join(l.Root, d, name)
}
func (l Layout) Graph(d string) string     { return l.read(d, "graph.html") }
func (l Layout) EdgeMap(d string) string   { return l.read(d, "edges.yaml") }
func (l Layout) Releases(d string) string  { return l.read(d, "releases.yaml") }
func (l Layout) Questions(d string) string { return filepath.Join(l.Root, d, "questions.yaml") }
func (l Layout) History(d string) string   { return l.read(d, "edges.history") }

// BuildID: the build id of the domain's current generation, 0 before any.
func (l Layout) BuildID(d string) int {
	t, err := os.Readlink(l.Current(d))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(filepath.Base(t))
	return n
}
func (l Layout) Overrides(d string) string {
	return filepath.Join(l.Root, d, "edges.override.yaml")
}

// Domains lists the built domains (folders holding a manifest.json), sorted.
func (l Layout) Domains() []string {
	ms, _ := filepath.Glob(filepath.Join(l.Root, "*", "manifest.json"))
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, filepath.Base(filepath.Dir(m)))
	}
	return out
}

// Options for Run.
type Options struct {
	Fetch          bool          // clone/refresh sources; false = rebuild from existing sources + manifests
	User, Password string        // git credentials when Fetch
	ScipTimeout    time.Duration // per build root; 0 = no limit
	Reindex        bool          // ignore the SCIP cache: index every build root again
	Graph          bool          // also render graph.html (a page for humans; nothing reads it)
	Log            io.Writer
	// Tools: weaver's read-only tools over a build in progress (its kb.db,
	// Zoekt and releases), for weave-ai. nil = weave-ai off.
	Tools func(domain, kbPath, zoektDir string, releases map[string]string) weaveai.Tools
}

// Run builds the knowledge of the domains the services belong to. Each domain's
// outputs stay in its own folder, but cross-domain facts are resolved over all
// of them: SCIP wrapper methods, cross-file constants, and produce<->consume
// matching — so a payments -> ordering call is kept in both domains' snapshots.
// Domains already built but not in this run are read (never written) for that:
// their kb.db edges and wrappers, their sources' constants.
func Run(services []Service, l Layout, o Options) error {
	log := o.Log
	unlock, err := lock(l.Root)
	if err != nil {
		return err
	}
	defer unlock()
	var domains []string
	byDomain := map[string][]Service{}
	for _, s := range services {
		if byDomain[s.Domain] == nil {
			domains = append(domains, s.Domain)
		}
		byDomain[s.Domain] = append(byDomain[s.Domain], s)
	}

	// 1. sources per domain: clone them, or take what is on disk. The manifest is
	// output — a record of what this build used, never trusted as input.
	manifests := map[string][]snapshot.Manifest{}
	for _, d := range domains {
		fmt.Fprintf(log, "== %s\n", d)
		var m []snapshot.Manifest
		if o.Fetch {
			var err error
			if m, err = Sync(byDomain[d], l.Sources(d), o.User, o.Password, log); err != nil {
				return err
			}
		} else {
			m = Scan(byDomain[d], l.Sources(d), log)
		}
		if len(m) == 0 {
			return fmt.Errorf("domain %s: none of its %d services is available — nothing built, previous knowledge kept", d, len(byDomain[d]))
		}
		if err := os.MkdirAll(l.Domain(d), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(l.Overrides(d)); os.IsNotExist(err) {
			if err := os.WriteFile(l.Overrides(d), []byte(edgemap.OverrideTemplate), 0o644); err != nil {
				return err
			}
		}
		// the run id: what this build of the domain built (git URL + commit per service)
		run := edgemap.RunID(m)
		for i := range m {
			m[i].Run = run
		}
		manifests[d] = m // written only once the domain's kb.db is in place (step 4)
	}
	var others []string
	for _, d := range l.Domains() {
		if byDomain[d] == nil {
			others = append(others, d)
		}
	}
	if len(others) > 0 {
		fmt.Fprintf(log, ">> cross-domain links from existing knowledge of: %v\n", others)
	}

	// 2. SCIP per domain into a fresh kb.db.tmp — the live kb.db stays readable
	// until step 4 swaps the new one in
	kbs := map[string]*sql.DB{}
	defer func() {
		for _, db := range kbs {
			db.Close()
		}
	}()
	for _, d := range domains {
		db, err := scipKB(l, d, manifests[d], o, log)
		if err != nil {
			return fmt.Errorf("domain %s: %w", d, err)
		}
		kbs[d] = db
	}

	// 3. cross-domain resolution inputs: wrapper symbols from every domain (a
	// shared library may live in another), scoped below to the services that use them
	wrappers := map[string]extract.Wrapper{}
	var srcDirs []string
	for _, d := range domains {
		w, err := scip.WrapperSymbols(kbs[d])
		if err != nil {
			return err
		}
		for k, v := range w {
			if _, ok := wrappers[k]; !ok {
				wrappers[k] = v
			}
		}
		srcDirs = append(srcDirs, l.Sources(d))
	}

	// other built domains: read-only context
	domainOf := map[string]string{}
	for _, d := range domains {
		for _, m := range manifests[d] {
			domainOf[m.ServiceName] = d
		}
	}
	// cloneDir: a service's clone, what SCIP paths are relative to; codeDir: its
	// own code, its root folder in a monorepo (Manifest.Dir / Code). A service
	// of another domain, not in this run, is sources/<domain>/<service>.
	codes, clones, roots := map[string]string{}, map[string]string{}, map[string]string{}
	for _, d := range domains {
		for _, m := range manifests[d] {
			codes[m.ServiceName], clones[m.ServiceName], roots[m.ServiceName] = m.Code(l.Sources(d)), m.Dir(l.Sources(d)), m.Root
		}
	}
	cloneDir := func(svc string) string {
		if dir, ok := clones[svc]; ok {
			return dir
		}
		return filepath.Join(l.Sources(domainOf[svc]), svc)
	}
	codeDir := func(svc string) string {
		if dir, ok := codes[svc]; ok {
			return dir
		}
		return cloneDir(svc)
	}
	var all []extract.Edge
	var readKBs []*sql.DB
	for _, d := range others {
		m, _ := snapshot.ReadManifest(l.Domain(d))
		for _, s := range m {
			domainOf[s.ServiceName] = d
		}
		if st, err := os.Stat(l.Sources(d)); err == nil && st.IsDir() {
			srcDirs = append(srcDirs, l.Sources(d))
		}
		e, w, db, err := existingKnowledge(l.KB(d))
		if err != nil {
			fmt.Fprintf(log, "  !! %s: existing knowledge unreadable (%v) — its cross-domain links are missing from this build\n", d, err)
			continue
		}
		defer db.Close()
		readKBs = append(readKBs, db)
		all = append(all, e...)
		for k, v := range w {
			if _, ok := wrappers[k]; !ok {
				wrappers[k] = v
			}
		}
	}
	used := map[string]map[string]extract.Wrapper{}
	for _, d := range domains {
		u, err := scip.WrappersUsed(kbs[d], wrappers)
		if err != nil {
			return err
		}
		for svc, w := range u {
			used[svc] = w
		}
	}
	consts := extract.CollectConstants(srcDirs...)
	// constants resolved exactly from SCIP (every domain's kb), names as the fallback
	allKBs := append([]*sql.DB(nil), readKBs...)
	for _, d := range domains {
		allKBs = append(allKBs, kbs[d])
	}
	symbols, err := scip.NewConstValues(allKBs, cloneDir)
	if err != nil {
		return err
	}
	// calls from SCIP: what each call site resolves to decides, never its method
	// name (user.set(...) is no Redis write); client types answered with
	// `weaver answer` count too, and unknown ones become `client` questions
	answeredClients := map[string]string{}
	for _, d := range domains {
		qf, err := edgemap.LoadQuestions(l.Questions(d))
		if err != nil {
			return err
		}
		for t, k := range qf.ClientAnswers() {
			answeredClients[t] = k
		}
	}
	owns := func(svc, path string) bool { // a monorepo service owns only its root's files
		r := roots[svc]
		return r == "" || strings.HasPrefix(path, r+"/")
	}
	calls := map[string]map[extract.Site]map[string][]extract.Hit{}
	unknown := map[string]map[string]*scip.UnknownClient{}
	for _, db := range allKBs {
		c, u, err := scip.ClientCalls(db, cloneDir, owns, wrappers, answeredClients)
		if err != nil {
			return err
		}
		for repo, lines := range c {
			calls[repo] = lines
		}
		for repo, us := range u {
			unknown[repo] = us
		}
	}
	x := &extract.Extractor{Wrappers: used, Consts: &consts, Symbols: symbols, Calls: calls}

	// 4. service edges per domain, plus each domain's own edges.override.yaml
	raw := map[string][]extract.Edge{}
	for _, d := range domains {
		for _, m := range manifests[d] {
			e := x.ExtractService(m.Dir(l.Sources(d)), m.ServiceName, m.Root)
			raw[d] = append(raw[d], e...)
			all = append(all, e...)
		}
	}
	// each domain's edges.override.yaml, validated before anything is written
	overrides := map[string]*edgemap.Override{}
	extracted := all // the service map takes extracted rows only; overrides merge in by its own rules
	for _, d := range domains {
		o, err := edgemap.LoadOverride(l.Overrides(d))
		if err != nil {
			return err // nothing written yet: the previous knowledge stays
		}
		overrides[d] = o
		for _, sk := range o.Skipped {
			fmt.Fprintf(log, "  !! override row skipped: %s\n", sk)
		}
		for _, e := range o.Rows(domainOf) { // kb.db service_edges and versions still read rows
			if domainOf[e.Repo] == d {
				raw[d] = append(raw[d], e)
			}
			all = append(all, e) // hops resolve over every domain
		}
	}
	// code search per domain, before the map: weave-ai reads this build's code
	// text while it reviews the map. A release's name is its build-file version.
	relNames := map[string]map[string]string{}
	for _, d := range domains {
		relNames[d] = map[string]string{}
		for _, m := range manifests[d] {
			name := edgemap.ReleaseVersion(m.Code(l.Sources(d)))
			if name == "" {
				name = m.SHA[:min(12, len(m.SHA))]
			}
			relNames[d][m.ServiceName] = name
		}
		if err := zoektIndex(l, d, manifests[d], relNames[d], log); err != nil {
			return err
		}
	}

	// the service map (docs/edges-spec.md), while every kb is still open; each
	// domain's outputs go into a new generation, switched to at the very end
	gens := map[string]int{}
	for _, d := range domains {
		runID := manifests[d][0].Run
		qf, err := edgemap.LoadQuestions(l.Questions(d))
		if err != nil {
			return err
		}
		// a new generation, starting from the current one's cumulative parts
		build := l.BuildID(d) + 1
		gdir := l.Gen(d, build)
		if err := newGeneration(l, d, gdir); err != nil {
			return err
		}
		gens[d] = build

		// the analyzers' output, per service, as facts (docs/analyzer-spec.md):
		// written into the generation and read back, so the edge mapper takes
		// only what the facts say
		cg := callGraphs(l, d, manifests[d], log)
		gapsOf := map[string][]edgemap.Question{}
		probeQs, probeEntries := springProbe(l, d, manifests[d], qf, log)
		for _, q := range append(probeQs, clientQuestions(manifests[d], unknown)...) {
			svc := q.Subject["service"]
			if svc == "" && len(q.Sites) > 0 {
				svc, _, _ = strings.Cut(q.Sites[0], ":")
			}
			gapsOf[svc] = append(gapsOf[svc], q)
		}
		mine := map[string]bool{}
		rowsOf := map[string][]extract.Edge{}
		for _, m := range manifests[d] {
			mine[m.ServiceName] = true
		}
		var rows []extract.Edge // other domains' rows: read-only context, not facts of this build
		for _, e := range extracted {
			if mine[e.Repo] {
				rowsOf[e.Repo] = append(rowsOf[e.Repo], e)
			} else {
				rows = append(rows, e)
			}
		}
		fdir := filepath.Join(gdir, "facts")
		cfgOf := map[string]map[string]string{}
		var qs []edgemap.Question
		callGraph := map[string][]edgemap.CGEdge{}
		for _, m := range manifests[d] {
			cfg := extract.ServiceConfig(cloneDir(m.ServiceName), roots[m.ServiceName])
			f := toFacts(m, analyzerName(cloneDir(m.ServiceName), m.Root), rowsOf[m.ServiceName], cg[m.ServiceName], cfg, gapsOf[m.ServiceName])
			f.Entrypoints = addEntries(f.Entrypoints, probeEntries[m.ServiceName])
			if err := facts.Write(fdir, f); err != nil {
				return err
			}
			back, err := facts.Read(fdir, m.ServiceName)
			if err != nil {
				return err
			}
			r, c, g := fromFacts(back)
			rows = append(rows, r...)
			if len(c) > 0 {
				callGraph[m.ServiceName] = c
			}
			cfgOf[m.ServiceName] = back.ConfigMap()
			qs = append(qs, g...)
		}
		config := func(svc string) map[string]string {
			if c, ok := cfgOf[svc]; ok {
				return c
			}
			return extract.ServiceConfig(cloneDir(svc), roots[svc]) // another domain's service
		}
		m, mqs, warns, err := edgemap.Build(edgemap.Input{Domain: d, DomainOf: domainOf, Raw: rows, KBs: allKBs, Override: overrides[d], Answers: qf.Answers(),
			CallGraph: callGraph, Config: config})
		if err != nil {
			return fmt.Errorf("domain %s: %w", d, err)
		}
		qs = append(mqs, qs...)
		for _, w := range warns {
			fmt.Fprintf(log, "  !! %s\n", w)
		}
		m.Run = runID
		// weave-ai reviews every edge against the code before it is written
		if c, on := weaveai.FromEnv(); on && o.Tools != nil {
			r := &weaveai.Reviewer{C: c, Tools: o.Tools(d, l.KB(d)+".tmp", l.Zoekt(d), relNames[d]),
				Cache: filepath.Join(l.Domain(d), "weave-ai", "verdicts.yaml")}
			qs = append(qs, reviewMap(&m, r, d, qf, log)...)
		}
		if err := edgemap.Write(filepath.Join(gdir, "edges.yaml"), m); err != nil {
			return err
		}
		// releases and per-service edge history, describing the edges.yaml just written
		notes, err := edgemap.Record(filepath.Join(gdir, "releases.yaml"), filepath.Join(gdir, "edges.history"), manifests[d],
			codeDir, m, time.Now())
		if err != nil {
			return err
		}
		if len(notes) > 0 {
			fmt.Fprintf(log, "  .. %s releases: %d note(s), e.g. %s\n", d, len(notes), notes[0])
		}
		if o.Graph { // the page draws edges.yaml, nothing else
			if err := ui.Write(filepath.Join(gdir, "graph.html"), d, m); err != nil {
				return err
			}
			fmt.Fprintf(log, ">> %s: service map -> %s\n", d, filepath.Join(gdir, "graph.html"))
		}
		run := qf.Record(runID, build, time.Now(), len(manifests[d]), qs)
		if err := qf.Write(l.Questions(d)); err != nil {
			return err
		}
		fmt.Fprintf(log, ">> %s: build %d: %d nodes, %d edges -> %s\n", d, build, len(m.Nodes), len(m.Edges), filepath.Join(gdir, "edges.yaml"))
		for _, svc := range sortedKeys(m.EntryStats) {
			st := m.EntryStats[svc]
			fmt.Fprintf(log, "   entries %-32s found %4d  none %4d  unknown %4d\n", svc, st[0], st[1], st[2])
		}
		fmt.Fprintf(log, ">> %s: %s raised %d questions (%d new, %d resolved, %d obsolete), %d call sites waiting on an answer -> %s\n",
			d, runID, run.Raised, run.New, run.Resolved, run.Obsolete, run.Pending, l.Questions(d))
	}
	for _, d := range domains {
		if err := extract.WriteEdges(kbs[d], raw[d]); err != nil {
			return err
		}
		if err := LoadDomains(kbs[d], manifests[d]); err != nil {
			return err
		}
		// swap in: a crash before this leaves the previous kb.db + manifest intact
		if err := kbs[d].Close(); err != nil {
			return err
		}
		delete(kbs, d)
		if err := os.Rename(l.KB(d)+".tmp", l.KB(d)); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(manifests[d], "", "  ")
		if err := os.WriteFile(filepath.Join(l.Domain(d), "manifest.json"), b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(log, ">> %s: %d services, %d service edges -> %s\n", d, len(manifests[d]), len(raw[d]), l.KB(d))
	}

	// 5. graph version per domain, hops resolved across every domain
	hops := graph.ResolveWithProvenance(all)
	for _, d := range domains {
		s := snapshot.ForDomain(d, manifests[d], l.Sources(d), hops)
		s.Run = manifests[d][0].Run
		if err := snapshot.Append(l.Versions(d), s); err != nil {
			return err
		}
		fmt.Fprintf(log, ">> %s: version %d, %d edges -> %s\n", d, s.Version, len(s.Edges), l.Versions(d))
	}

	// 7. switch every domain to its new generation, all outputs being written
	for _, d := range domains {
		if n, ok := gens[d]; ok {
			if err := switchGeneration(l, d, n, manifests[d][0].Run); err != nil {
				return err
			}
			fmt.Fprintf(log, ">> %s: build %d is current -> %s\n", d, n, l.Current(d))
		}
	}
	fmt.Fprintf(log, ">> %d wrapper methods found, used by %d services\n", len(wrappers), len(used))
	return nil
}

// newGeneration makes the folder of a new build: the current generation's
// cumulative parts (edge history, releases.yaml) carried forward, and the
// override as it is now, which this build's map was merged with.
func newGeneration(l Layout, d, gdir string) error {
	if err := os.RemoveAll(gdir); err != nil { // a failed build's leftovers
		return err
	}
	if err := os.MkdirAll(gdir, 0o755); err != nil {
		return err
	}
	if err := copyTree(l.History(d), filepath.Join(gdir, "edges.history")); err != nil {
		return err
	}
	for _, f := range [][2]string{{l.Releases(d), "releases.yaml"}, {l.Overrides(d), "edges.override.yaml"}} {
		if b, err := os.ReadFile(f[0]); err == nil {
			if err := os.WriteFile(filepath.Join(gdir, f[1]), b, 0o644); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// switchGeneration records the build id and points current at it in one
// rename, so a reader sees the old generation or the new one, never a mix.
// Every generation is kept.
func switchGeneration(l Layout, d string, n int, run string) error {
	gdir := l.Gen(d, n)
	meta := fmt.Sprintf("build: %d\nrun: %s\nbuiltAt: %q\n", n, run, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(gdir, "build.yaml"), []byte(meta), 0o644); err != nil {
		return err
	}
	tmp := l.Current(d) + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("generations", strconv.Itoa(n)), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, l.Current(d))
}

// copyTree copies a folder of files (edge history: <svc>/<version>.yaml).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == src {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if e.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
}

// lock takes the knowledge folder for this build: two builds would each delete
// and recreate the same kb.db. The OS drops the lock if the process dies.
func lock(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, ".build.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another weaver build is running on %s", root)
	}
	return func() { f.Close() }, nil
}

// existingKnowledge: a built domain's raw service edges and SCIP wrappers, and
// its kb opened read-only (the caller closes it).
func existingKnowledge(kbPath string) ([]extract.Edge, map[string]extract.Wrapper, *sql.DB, error) {
	if _, err := os.Stat(kbPath); err != nil {
		return nil, nil, nil, err
	}
	db, err := sql.Open("sqlite", "file:"+kbPath+"?mode=ro")
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) ([]extract.Edge, map[string]extract.Wrapper, *sql.DB, error) {
		db.Close()
		return nil, nil, nil, err
	}
	w, err := scip.WrapperSymbols(db)
	if err != nil {
		return fail(err)
	}
	rows, err := db.Query("SELECT src, dst, kind, channel, role, coalesce(repo, '') FROM service_edges")
	if err != nil {
		return fail(err)
	}
	defer rows.Close()
	var out []extract.Edge
	for rows.Next() {
		var e extract.Edge
		if err := rows.Scan(&e.Src, &e.Dst, &e.Kind, &e.Channel, &e.Role, &e.Repo); err != nil {
			return fail(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	return out, w, db, nil
}

// scipKB SCIP-indexes each service's build roots (a repo that fails to index
// still gets its edges extracted later) and ingests the indexes into a fresh
// kb.db.tmp.
// cacheVersion: bump when what an index means to weaver changes (roots, ingest),
// so every cached index is rebuilt once.
const cacheVersion = 1

// scipCache records, per service, what its cached indexes were built from.
type scipCache struct {
	Version int         `json:"version"`
	SHA     string      `json:"sha"`
	Roots   []scip.Root `json:"roots"`
	OK      []bool      `json:"ok"` // per root: indexed, or failed at this commit
}

func (l Layout) scipCacheFile(d, svc string) string {
	return filepath.Join(l.Scip(d), svc+".cache.json")
}

// reusable: the previous indexes still describe this checkout exactly — same
// commit, nothing uncommitted, same build roots. A plain copy (no commit) never is.
func reusable(c scipCache, m snapshot.Manifest, repo string, roots []scip.Root) bool {
	if c.Version != cacheVersion || m.SHA == "" || m.SHA == "nogit" || c.SHA != m.SHA ||
		len(c.Roots) != len(roots) || len(c.OK) != len(roots) {
		return false
	}
	for i := range roots {
		if c.Roots[i] != roots[i] {
			return false
		}
	}
	out, err := exec.Command("git", "-C", repo, "status", "--porcelain", "--untracked-files=no").Output()
	return err == nil && strings.TrimSpace(string(out)) == ""
}

// ps0: the service a shared index was built for.
func ps0(ps []scip.Part) string {
	if len(ps) == 0 {
		return "its clone (none built)"
	}
	return ps[0].Repo
}

func scipKB(l Layout, d string, manifest []snapshot.Manifest, o Options, log io.Writer) (*sql.DB, error) {
	if err := os.MkdirAll(l.Scip(d), 0o755); err != nil {
		return nil, err
	}
	tmpKB := l.KB(d) + ".tmp"
	_ = os.Remove(tmpKB)
	var parts []scip.Part
	indexed, reused := 0, 0
	shared := map[string][]scip.Part{} // clone dir -> its indexes, for the other services of a monorepo
	for _, m := range manifest {
		name, repo := m.ServiceName, m.Dir(l.Sources(d))
		if ps, ok := shared[repo]; ok {
			for _, p := range ps {
				parts = append(parts, scip.Part{Scip: p.Scip, Repo: name, Prefix: p.Prefix, Release: m.SHA})
			}
			if len(ps) > 0 {
				indexed++
			}
			fmt.Fprintf(log, "  %s: shares the SCIP index of %s\n", name, ps0(ps))
			continue
		}
		first := len(parts)
		roots := scip.Roots(repo)
		if len(roots) == 0 {
			fmt.Fprintf(log, "  %s: SCIP skipped: no Maven/Gradle project or go.mod\n", name)
			continue
		}
		var prev scipCache
		if b, err := os.ReadFile(l.scipCacheFile(d, name)); err == nil {
			_ = json.Unmarshal(b, &prev)
		}
		reuse := !o.Reindex && reusable(prev, m, repo, roots)
		cache := scipCache{Version: cacheVersion, SHA: m.SHA, Roots: roots, OK: make([]bool, len(roots))}
		ok := 0
		for i, r := range roots {
			sc := filepath.Join(l.Scip(d), fmt.Sprintf("%s.%d.scip", name, i))
			where := name
			if r.Dir != "" {
				where = name + "/" + r.Dir
			}
			if reuse {
				if _, err := os.Stat(sc); prev.OK[i] && err == nil {
					cache.OK[i] = true
					parts = append(parts, scip.Part{Scip: sc, Repo: name, Prefix: r.Dir, Release: m.SHA})
					ok++
					continue
				}
				if !prev.OK[i] {
					fmt.Fprintf(log, "  %s: SCIP skipped: failed at this commit last build (--reindex to retry)\n", where)
					continue
				}
				// cached index file missing: fall through and index
			}
			fmt.Fprintf(log, "  %s: %s…\n", where, r.Tool)
			start := time.Now()
			if err := scip.Index(repo, r, sc, o.ScipTimeout); err != nil {
				fmt.Fprintf(log, "  %s: SCIP skipped: %v\n", where, err)
				continue
			}
			fmt.Fprintf(log, "  %s: indexed in %s\n", where, time.Since(start).Round(time.Second))
			cache.OK[i] = true
			parts = append(parts, scip.Part{Scip: sc, Repo: name, Prefix: r.Dir, Release: m.SHA})
			ok++
		}
		if reuse {
			reused++
			fmt.Fprintf(log, "  %s: unchanged at %.10s, reusing its SCIP index\n", name, m.SHA)
		}
		if b, err := json.Marshal(cache); err == nil {
			if err := os.WriteFile(l.scipCacheFile(d, name), b, 0o644); err != nil {
				return nil, err
			}
		}
		if ok > 0 {
			indexed++
		}
		if m.Clone != "" {
			shared[repo] = append([]scip.Part(nil), parts[first:]...)
		}
		if len(roots) > 1 {
			fmt.Fprintf(log, "  %s: %d/%d build roots SCIP-indexed\n", name, ok, len(roots))
		}
	}
	if reused > 0 {
		fmt.Fprintf(log, ">> %s: %d services unchanged, SCIP index reused\n", d, reused)
	}
	kb, err := sql.Open("sqlite", tmpKB)
	if err != nil {
		return nil, err
	}
	// every older release of the previous kb.db comes along, not current: old
	// releases are never indexed again (build-spec §4)
	built := map[string]string{}
	for _, m := range manifest {
		built[m.ServiceName] = m.SHA
	}
	if err := scip.CarryForward(kb, l.KB(d), built); err != nil {
		kb.Close()
		return nil, err
	}
	skipped, err := scip.Ingest(kb, parts)
	if err != nil {
		kb.Close()
		return nil, err
	}
	if skipped > 0 {
		fmt.Fprintf(log, "  %s: %d SCIP occurrences with invalid ranges skipped\n", d, skipped)
	}
	fmt.Fprintf(log, ">> %s: %d/%d services SCIP-indexed\n", d, indexed, len(manifest))
	return kb, nil
}

// zoektIndex writes the domain's code-search shards: one repo per service with
// its release branch, commit and domain; shards of dropped services go.
func zoektIndex(l Layout, d string, manifest []snapshot.Manifest, rels map[string]string, log io.Writer) error {
	dir := l.Zoekt(d)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	n := 0
	for _, m := range manifest {
		r := search.Repo{Name: m.ServiceName, Run: m.Run, Dir: m.Dir(l.Sources(d)), Root: m.Root,
			URL: m.GitURL, Domain: d, Branch: m.Branch, Commit: m.SHA, Release: rels[m.ServiceName]}
		if done, _ := filepath.Glob(filepath.Join(dir, url.QueryEscape(search.ShardPrefix(r))+"_v*.zoekt")); len(done) > 0 && m.SHA != "nogit" {
			continue // this release is indexed already, and a release's code doesn't change
		}
		if err := search.IndexRepo(dir, r); err != nil {
			return fmt.Errorf("zoekt index %s: %w", m.ServiceName, err)
		}
		n++
	}
	// shards from before releases had their own (named <service>_v16…, no
	// release branch) can't be searched by release: they go
	if old, _ := filepath.Glob(filepath.Join(dir, "*.zoekt")); len(old) > 0 {
		for _, f := range old {
			if !strings.Contains(filepath.Base(f), "%40") {
				_ = os.Remove(f)
			}
		}
	}
	// every release's shard is kept, retired services' included (build-spec §4)
	fmt.Fprintf(log, ">> %s: zoekt index, %d release(s) indexed, %d services -> %s\n", d, n, len(manifest), dir)
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// clientQuestions: one `client` question per external type the domain's
// services call that looks like a client but isn't classified, with the
// services and call sites that use it.
func clientQuestions(ms []snapshot.Manifest, unknown map[string]map[string]*scip.UnknownClient) []edgemap.Question {
	type agg struct {
		calls         int
		svcs, methods []string
		sites         []string
	}
	by := map[string]*agg{}
	var types []string
	for _, m := range ms {
		for typ, u := range unknown[m.ServiceName] {
			a := by[typ]
			if a == nil {
				a = &agg{}
				by[typ] = a
				types = append(types, typ)
			}
			a.calls += u.Calls
			a.svcs = append(a.svcs, m.ServiceName)
			for _, x := range u.Methods {
				if !slices.Contains(a.methods, x) {
					a.methods = append(a.methods, x)
				}
			}
			for _, st := range u.Sites {
				a.sites = append(a.sites, fmt.Sprintf("%s:%s:%d", m.ServiceName, st.Path, st.Line+1))
			}
		}
	}
	sort.Strings(types)
	var out []edgemap.Question
	for _, typ := range types {
		a := by[typ]
		sort.Strings(a.methods)
		out = append(out, edgemap.ClientQuestion(typ, a.calls, a.svcs, a.methods, a.sites))
	}
	return out
}

// ReleaseNames: each service's current release name from a releases.yaml (its
// version, else its short commit): the branch its Zoekt shard carries.
func ReleaseNames(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var f edgemap.ReleaseFile
	if yaml.Unmarshal(b, &f) != nil {
		return out
	}
	for _, s := range f.Services {
		if len(s.Releases) == 0 {
			continue
		}
		r := s.Releases[0]
		name := r.Version
		if name == "" {
			name = r.CommitHash[:min(12, len(r.CommitHash))]
		}
		out[s.ServiceName] = name
	}
	return out
}

// reviewMap has weave-ai check every edge (docs/analyzer-spec.md §5), those
// with the most entry points behind them first, up to the cap. An edge it
// agrees with is marked reviewedBy: llm; one it disputes stays in the map with
// call: unknown and a review question. A review answered keep is the human's.
func reviewMap(m *edgemap.Map, r *weaveai.Reviewer, d string, qf *edgemap.Questions, log io.Writer) []edgemap.Question {
	answered := qf.ReviewAnswers()
	order := make([]int, len(m.Edges))
	weight := func(e edgemap.Edge) int {
		n := 0
		for _, c := range e.Callers {
			if l, ok := c.Entries.([]edgemap.Entry); ok {
				n += len(l)
			}
		}
		return n
	}
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ea, eb := m.Edges[order[a]], m.Edges[order[b]]
		if wa, wb := weight(ea), weight(eb); wa != wb {
			return wa > wb
		}
		return edgemap.EdgeRef(ea) < edgemap.EdgeRef(eb)
	})
	var qs []edgemap.Question
	agreed, disputed, reviewed := 0, 0, 0
	for _, i := range order {
		e := &m.Edges[i]
		ref := edgemap.EdgeRef(*e)
		if answered[ref] == "keep" {
			e.ReviewedBy = "human"
			continue
		}
		if e.Question != "" || reviewed >= r.C.MaxReviews {
			continue // waiting on a question already, or over the cap: unreviewed
		}
		reviewed++
		c := weaveai.Claim{Ref: ref, Domain: d}
		for _, cl := range e.Callers {
			c.Sites = append(c.Sites, e.From+":"+cl.At)
		}
		if e.Handler != nil {
			c.Handler = e.Handler.At
		}
		v, err := r.Review(c)
		if err != nil {
			fmt.Fprintf(log, "  !! weave-ai: %v; the rest of the map is written unreviewed\n", err)
			break
		}
		if v.Agree {
			e.ReviewedBy = "llm"
			agreed++
			continue
		}
		q := edgemap.ReviewQuestion(*e, v.Reason, v.Evidence)
		e.Call, e.Question = "unknown", q.ID
		qs = append(qs, q)
		disputed++
	}
	fmt.Fprintf(log, ">> %s: weave-ai reviewed %d edges: %d agreed, %d disputed, %d unreviewed\n", d, reviewed, agreed, disputed, len(m.Edges)-reviewed)
	return qs
}
