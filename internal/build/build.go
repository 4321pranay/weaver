// Package build clones every service in sources/<domain>/sources.yaml at its branch
// into sources/<domain>/, and builds
// weaver's static knowledge into the knowledge folder, one folder per domain:
// kb.db (SCIP symbols, service edges, domains table), graph versions, and the
// Zoekt code-search index falcon queries.
package build

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"weaver/internal/extract"
	"weaver/internal/graph"
	"weaver/internal/scip"
	"weaver/internal/search"
	"weaver/internal/snapshot"
	"weaver/internal/ui"
)

// Service is one entry under `services:` in a sources.yaml; all fields required.
type Service struct {
	Domain, ServiceName, GitURL, Branch string
}

// LoadSources reads one domain's sources/<domain>/sources.yaml; the folder
// names the domain:
//
//	services:
//	  - serviceName: payment-gateway
//	    gitUrl: https://bitbucket.internal/scm/pay/payment-gateway.git
//	    branch: release/2.4
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
		seen[s.ServiceName] = true
		out = append(out, Service{domain, s.ServiceName, s.GitURL, strings.TrimPrefix(s.Branch, "refs/heads/")})
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
// sourcesDir/<serviceName> and drops clones of services no longer configured. A
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
	for _, s := range services {
		configured[s.ServiceName] = true
		m, err := cloneBranch(g, s, sourcesDir)
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
		if e.IsDir() && !configured[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
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
	dest := filepath.Join(sourcesDir, s.ServiceName)
	tmp := filepath.Join(sourcesDir, ".clone-"+s.ServiceName)
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
	if err := os.RemoveAll(dest); err != nil {
		return snapshot.Manifest{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return snapshot.Manifest{}, err
	}
	return snapshot.Manifest{Domain: s.Domain, ServiceName: s.ServiceName, GitURL: s.GitURL,
		Branch: branch, SHA: strings.TrimSpace(sha)}, nil
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
		dir := filepath.Join(sourcesDir, s.ServiceName)
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			fmt.Fprintf(log, "  !! %s: not in %s, skipped\n", s.ServiceName, sourcesDir)
			continue
		}
		m := snapshot.Manifest{Domain: s.Domain, ServiceName: s.ServiceName, GitURL: s.GitURL, Branch: s.Branch, SHA: "nogit"}
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
		"CREATE TABLE domains (domain TEXT, service TEXT, git_url TEXT, branch TEXT, sha TEXT)"} {
		if _, err := kb.Exec(q); err != nil {
			return err
		}
	}
	for _, m := range manifest {
		if _, err := kb.Exec("INSERT INTO domains VALUES (?,?,?,?,?)", m.Domain, m.ServiceName, m.GitURL, m.Branch, m.SHA); err != nil {
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
//	<root>/<domain>/graph.html           service map page (open in a browser)
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
func (l Layout) Graph(d string) string { return filepath.Join(l.Root, d, "graph.html") }
func (l Layout) Overrides(d string) string {
	return filepath.Join(l.Root, d, "edges.override.yaml")
}

// overrideTemplate is written once per domain, so the file is where people look.
const overrideTemplate = `# Service edges of this domain that static analysis can't see: runtime-computed
# topics, URLs built from config, dynamic routing. Loaded on every weaver build
# and merged with the extracted edges. Each row names one of THIS domain's
# services: src for role: produce, dst for role: consume. Pair a produce row
# with a consume row (in any domain) on the same kind + channel to make a hop.
#
# role: produce | consume        kind: rest | grpc | kafka | redis | db
#
# - src: checkout
#   dst: ""
#   kind: kafka
#   channel: orders.created
#   role: produce
`

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
	Log            io.Writer
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
			if err := os.WriteFile(l.Overrides(d), []byte(overrideTemplate), 0o644); err != nil {
				return err
			}
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
	symbols, err := scip.NewConstValues(allKBs, func(repo string) string {
		return filepath.Join(l.Sources(domainOf[repo]), repo)
	})
	if err != nil {
		return err
	}
	x := &extract.Extractor{Wrappers: used, Consts: &consts, Symbols: symbols}

	// 4. service edges per domain, plus each domain's own edges.override.yaml
	raw := map[string][]extract.Edge{}
	for _, d := range domains {
		for _, m := range manifests[d] {
			e := x.ExtractRepo(filepath.Join(l.Sources(d), m.ServiceName))
			raw[d] = append(raw[d], e...)
			all = append(all, e...)
		}
	}
	for _, d := range domains {
		overrides, err := extract.LoadOverrides(l.Overrides(d))
		if err != nil {
			return err // nothing written yet: the previous knowledge stays
		}
		for _, e := range overrides {
			svc := e.Src
			if e.Role == "consume" {
				svc = e.Dst
			}
			if domainOf[svc] != d {
				fmt.Fprintf(log, "  !! %s override %s %s: service %q is not in domain %s, skipped\n", d, e.Kind, e.Channel, svc, d)
				continue
			}
			e.Repo = svc
			raw[d] = append(raw[d], e)
			all = append(all, e)
		}
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
		if err := snapshot.Append(l.Versions(d), s); err != nil {
			return err
		}
		all, err := snapshot.LoadAll(l.Versions(d))
		if err != nil {
			return err
		}
		if err := ui.Write(l.Graph(d), d, all, domainOf); err != nil {
			return err
		}
		fmt.Fprintf(log, ">> %s: version %d, %d edges -> %s, %s\n", d, s.Version, len(s.Edges), l.Versions(d), l.Graph(d))
	}

	// 6. code search per domain
	for _, d := range domains {
		if err := zoektIndex(l, d, manifests[d], log); err != nil {
			return err
		}
	}
	fmt.Fprintf(log, ">> %d wrapper methods found, used by %d services\n", len(wrappers), len(used))
	return nil
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

func scipKB(l Layout, d string, manifest []snapshot.Manifest, o Options, log io.Writer) (*sql.DB, error) {
	if err := os.MkdirAll(l.Scip(d), 0o755); err != nil {
		return nil, err
	}
	tmpKB := l.KB(d) + ".tmp"
	_ = os.Remove(tmpKB)
	var parts []scip.Part
	indexed, reused := 0, 0
	for _, m := range manifest {
		name, repo := m.ServiceName, filepath.Join(l.Sources(d), m.ServiceName)
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
					parts = append(parts, scip.Part{Scip: sc, Repo: name, Prefix: r.Dir})
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
			parts = append(parts, scip.Part{Scip: sc, Repo: name, Prefix: r.Dir})
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
func zoektIndex(l Layout, d string, manifest []snapshot.Manifest, log io.Writer) error {
	dir := l.Zoekt(d)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, m := range manifest {
		r := search.Repo{Name: m.ServiceName, Dir: filepath.Join(l.Sources(d), m.ServiceName),
			URL: m.GitURL, Domain: d, Branch: m.Branch, Commit: m.SHA}
		if err := search.IndexRepo(dir, r); err != nil {
			return fmt.Errorf("zoekt index %s: %w", m.ServiceName, err)
		}
		keep[m.ServiceName] = true
	}
	n, err := search.Prune(dir, keep)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, ">> %s: zoekt index, %d services (%d stale shards removed) -> %s\n", d, len(keep), n, dir)
	return nil
}
