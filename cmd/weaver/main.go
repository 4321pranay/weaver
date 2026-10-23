// weaver: static code knowledge base over many repos — SCIP cross-repo symbols +
// a service-edge graph (REST/gRPC/Kafka/Redis/DB) SCIP can't see.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	_ "modernc.org/sqlite"

	"weaver/internal/build"
	"weaver/internal/extract"
	"weaver/internal/graph"
	"weaver/internal/kb"
	"weaver/internal/search"
	"weaver/internal/snapshot"
)

const usage = `usage: weaver <command> [args]

build [<domain>...] [--no-fetch] [--reindex]
          clone each service listed in sources/<domain>/sources.yaml at its branch and
          build the domain's knowledge; no domain = every domain with a sources.yaml;
          --no-fetch rebuilds from the existing clones; a service whose commit is
          unchanged reuses its SCIP index, --reindex indexes everything again
use domain [<name> | --all]
          pick the domain every query reads (saved in KNOWLEDGE_DIR/.domain);
          no name: show it; --all: every domain
query <what> [args]
          inspect the built knowledge (weaver query help)
completion zsh
          shell completion; add to ~/.zshrc:  source <(weaver completion zsh)

input  SOURCES_DIR   (default ./sources):   <domain>/sources.yaml  <domain>/<service>/ clones
output KNOWLEDGE_DIR (default ./knowledge): <domain>/ kb.db  zoekt/  versions.jsonl  manifest.json  scip/
       and <domain>/edges.override.yaml: your runtime-only edges (template created on first build)
env: SOURCES_DIR  KNOWLEDGE_DIR  WEAVER_GIT_USER / WEAVER_GIT_PASS (skip the credentials prompt)
     SCIP_TIMEOUT (default 15m) per build root; the indexers download all dependencies
`

const queryUsage = `usage: weaver query <what> [args]   (reads the domain set by ` + "`weaver use domain`" + `)

def <symbol>                cross-repo definition
refs <symbol>               cross-repo references
flow [service]              raw service edges (all when no service)
blast <service> [downstream|upstream|both] [max_hops]
                            what breaks if it fails / what it depends on, by hop
context "<question>"        LLM system prompt listing the services named in it
search '<zoekt query>' [max]
                            code search, e.g. 'repo:^payment$ retry'
graph [--infra]             service map JSON for the UI; --infra draws
                            topics/keys/tables as nodes
versions                    the domain's graph versions, one per build
diff <v1> <v2>              what changed between two versions
`

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "weaver:", err)
	os.Exit(1)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // channels hold <DYNAMIC_DISPATCH: …> markers
	if err := enc.Encode(v); err != nil {
		die(err)
	}
}

func arg(args []string, i int, def string) string {
	if len(args) > i {
		return args[i]
	}
	return def
}

func knowledgeDir() string { return env("KNOWLEDGE_DIR", "knowledge") }

// currentDomain: the domain set by `weaver use domain`, "" = every domain.
func currentDomain(layout build.Layout) string {
	b, err := os.ReadFile(filepath.Join(layout.Root, ".domain"))
	if err != nil {
		return ""
	}
	d := strings.TrimSpace(string(b))
	if d != "" && !slices.Contains(layout.Domains(), d) {
		die(fmt.Errorf("domain %q (from `weaver use domain`) is not built in %s; built: %v", d, layout.Root, layout.Domains()))
	}
	return d
}

// needDomain: commands over one domain's history (versions, diff) need one picked.
func needDomain(layout build.Layout) string {
	d := currentDomain(layout)
	if d == "" {
		die(fmt.Errorf("no domain selected: weaver use domain <name> (built: %v)", layout.Domains()))
	}
	return d
}

func openKB(layout build.Layout) *kb.KB {
	var k *kb.KB
	var err error
	if d := currentDomain(layout); d != "" {
		k, err = kb.Open(layout.KB(d))
	} else {
		k, err = kb.OpenKnowledge(layout.Root)
	}
	if err != nil {
		die(err)
	}
	return k
}

// domainServices: the selected domain's services, nil when every domain is in use.
func domainServices(layout build.Layout) map[string]bool {
	d := currentDomain(layout)
	if d == "" {
		return nil
	}
	m, _ := snapshot.ReadManifest(layout.Domain(d))
	out := map[string]bool{}
	for _, s := range m {
		out[s.ServiceName] = true
	}
	return out
}

// openAll: every domain's kb. Hops are resolved over all of them — a producer in
// one domain meets its consumer in another — and THEN narrowed to the selected
// domain, the same rule its versions.jsonl uses.
func openAll(layout build.Layout) *kb.KB {
	k, err := kb.OpenKnowledge(layout.Root)
	if err != nil {
		die(err)
	}
	return k
}

func cmdUse(layout build.Layout, args []string) {
	if len(args) == 0 || args[0] != "domain" {
		die(fmt.Errorf("usage: weaver use domain [<name> | --all]"))
	}
	f := filepath.Join(layout.Root, ".domain")
	switch arg(args, 1, "") {
	case "":
		if d := currentDomain(layout); d != "" {
			fmt.Println(d)
		} else {
			fmt.Printf("all domains: %v\n", layout.Domains())
		}
	case "--all":
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			die(err)
		}
		fmt.Printf("using all domains: %v\n", layout.Domains())
	default:
		d := args[1]
		if !slices.Contains(layout.Domains(), d) {
			die(fmt.Errorf("domain %q is not built in %s; built: %v", d, layout.Root, layout.Domains()))
		}
		if err := os.WriteFile(f, []byte(d+"\n"), 0o644); err != nil {
			die(err)
		}
		fmt.Println("using domain", d)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	layout := build.Layout{Root: knowledgeDir(), SourcesRoot: env("SOURCES_DIR", "sources")}

	switch cmd {
	case "build":
		fetch, reindex := true, false
		var rest []string
		for _, a := range args {
			switch a {
			case "--no-fetch":
				fetch = false
			case "--reindex":
				reindex = true
			default:
				rest = append(rest, a)
			}
		}
		cmdBuild(rest, layout, fetch, reindex)
	case "use":
		cmdUse(layout, args)
	case "query":
		if len(args) == 0 {
			fmt.Fprint(os.Stderr, queryUsage)
			os.Exit(2)
		}
		query(layout, args[0], args[1:])
	case "completion":
		if arg(args, 0, "") != "zsh" {
			die(fmt.Errorf("usage: weaver completion zsh"))
		}
		fmt.Print(zshCompletion)
	case "__complete":
		cmdComplete(layout, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func query(layout build.Layout, what string, args []string) {
	switch what {
	case "def", "refs":
		if len(args) != 1 {
			die(fmt.Errorf("usage: weaver query %s <symbol>", what))
		}
		k := openKB(layout)
		defer k.Close()
		var v any
		var err error
		if what == "def" {
			v, err = k.FindDefinition(args[0], 20)
		} else {
			v, err = k.FindReferences(args[0], 50)
		}
		if err != nil {
			die(err)
		}
		printJSON(v)
	case "flow":
		k := openKB(layout)
		defer k.Close()
		v, err := k.ServiceFlow(arg(args, 0, ""))
		if err != nil {
			die(err)
		}
		printJSON(v)
	case "blast":
		if len(args) < 1 {
			die(fmt.Errorf("usage: weaver query blast <service> [downstream|upstream|both] [max_hops]"))
		}
		hops, err := strconv.Atoi(arg(args, 2, "5"))
		if err != nil {
			die(err)
		}
		k := openAll(layout)
		defer k.Close()
		edges, err := k.ResolvedEdges()
		if err != nil {
			die(err)
		}
		if mine := domainServices(layout); mine != nil {
			if !mine[args[0]] {
				die(fmt.Errorf("service %q is not in domain %s", args[0], currentDomain(layout)))
			}
			var keep []kb.Hop
			for _, e := range edges {
				if mine[e.Src] || mine[e.Dst] {
					keep = append(keep, e)
				}
			}
			edges = keep
		}
		printJSON(kb.BlastRadius(edges, args[0], arg(args, 1, "downstream"), hops))
	case "context":
		if len(args) != 1 {
			die(fmt.Errorf(`usage: weaver query context "<question>"`))
		}
		k := openAll(layout)
		defer k.Close()
		s, err := k.Context(args[0], domainServices(layout))
		if err != nil {
			die(err)
		}
		fmt.Print(s)
	case "search":
		querySearch(layout, args)
	case "graph":
		queryGraph(layout, args)
	case "versions":
		all, err := snapshot.LoadAll(layout.Versions(needDomain(layout)))
		if err != nil {
			die(err)
		}
		for _, s := range all {
			fmt.Printf("%d  %s  edges=%d  repos=%d\n", s.Version, s.TS, len(s.Edges), len(s.Commits))
		}
	case "diff":
		if len(args) != 2 {
			die(fmt.Errorf("usage: weaver query diff <v1> <v2>"))
		}
		all, err := snapshot.LoadAll(layout.Versions(needDomain(layout)))
		if err != nil {
			die(err)
		}
		by := map[string]snapshot.Snapshot{}
		for _, s := range all {
			by[strconv.FormatInt(s.Version, 10)] = s
		}
		v1, ok1 := by[args[0]]
		v2, ok2 := by[args[1]]
		if !ok1 || !ok2 {
			die(fmt.Errorf("unknown version (see `weaver query versions`)"))
		}
		printJSON(snapshot.Compare(v1, v2))
	case "-h", "--help", "help":
		fmt.Print(queryUsage)
	default:
		fmt.Fprint(os.Stderr, queryUsage)
		os.Exit(2)
	}
}

func querySearch(layout build.Layout, args []string) {
	if len(args) < 1 {
		die(fmt.Errorf("usage: weaver query search '<zoekt query>' [max]"))
	}
	max, err := strconv.Atoi(arg(args, 1, "50"))
	if err != nil {
		die(err)
	}
	domains := layout.Domains()
	if d := currentDomain(layout); d != "" {
		domains = []string{d}
	}
	var dirs []string
	for _, d := range domains {
		dirs = append(dirs, layout.Zoekt(d))
	}
	hits, err := search.Search(dirs, args[0], max)
	if err != nil {
		die(err)
	}
	printJSON(hits)
}

func cmdBuild(domains []string, layout build.Layout, fetch, reindex bool) {
	if len(domains) == 0 {
		if domains = layout.Configured(); len(domains) == 0 {
			die(fmt.Errorf("no %s found: add a sources.yaml per domain", layout.SourcesFile("<domain>")))
		}
	}
	var services []build.Service
	for _, d := range domains {
		s, err := build.LoadSources(layout.SourcesFile(d), d)
		if err != nil {
			die(err)
		}
		services = append(services, s...)
	}
	timeout, err := time.ParseDuration(env("SCIP_TIMEOUT", "15m"))
	if err != nil {
		die(fmt.Errorf("SCIP_TIMEOUT: %w", err))
	}
	o := build.Options{Fetch: fetch, Reindex: reindex, Log: os.Stdout, ScipTimeout: timeout}
	if fetch {
		o.User, o.Password = os.Getenv("WEAVER_GIT_USER"), os.Getenv("WEAVER_GIT_PASS")
		if o.User == "" {
			fmt.Fprint(os.Stderr, "git username: ")
			if _, err := fmt.Scanln(&o.User); err != nil {
				die(fmt.Errorf("reading username: %w", err))
			}
		}
		if o.Password == "" {
			fmt.Fprint(os.Stderr, "git password: ")
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				die(fmt.Errorf("reading password: %w", err))
			}
			o.Password = string(b)
		}
	}
	if err := build.Run(services, layout, o); err != nil {
		die(err)
	}
	fmt.Println(">> knowledge built in", layout.Root)
}

func queryGraph(layout build.Layout, args []string) {
	infra := slices.Contains(args, "--infra")
	domainOf := map[string]string{}
	for _, d := range layout.Domains() {
		m, _ := snapshot.ReadManifest(layout.Domain(d))
		for _, s := range m {
			domainOf[s.ServiceName] = d
		}
	}
	k := openAll(layout)
	rows, err := k.ServiceFlow("")
	k.Close()
	if err != nil {
		die(err)
	}
	var raw []extract.Edge
	for _, r := range rows {
		raw = append(raw, extract.Edge{Src: r.Src, Dst: r.Dst, Kind: r.Kind, Channel: r.Channel, Role: r.Role})
	}
	hops := graph.Resolve(raw)
	if mine := domainServices(layout); mine != nil {
		var keep []graph.Hop
		for _, h := range hops {
			if mine[h.Src] || mine[h.Dst] {
				keep = append(keep, h)
			}
		}
		hops = keep
	}
	printJSON(graph.ToGraph(hops, infra, domainOf, nil))
}
