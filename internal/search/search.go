// Package search builds the Zoekt code-search index weaver hands to falcon, one
// index dir per domain (knowledge/<domain>/zoekt): one repository per service,
// named by serviceName (the same name the service graph uses), carrying its
// release branch + commit and `domain` metadata.
//
// Falcon (or zoekt-webserver -index <dir>) scopes queries with the graph:
//
//	repo:^(checkout|payment)$ retry      services blast_radius / a drift names
//	meta.domain:^payments$ timeout       one domain
package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	zsearch "github.com/sourcegraph/zoekt/search"
)

// Repo describes one service's checkout.
type Repo struct {
	Name, Dir, URL, Domain, Branch, Commit, Run string
	Root                                        string // a monorepo service: only this folder of Dir, paths still relative to Dir
	// Release: the release's name (its version, build-spec §2), the branch its
	// shard carries. Every release has its own shard, kept (build-spec §4).
	Release string
}

// ShardPrefix: one release's shard file name, by service and commit.
func ShardPrefix(r Repo) string {
	c := r.Commit
	if len(c) > 12 {
		c = c[:12]
	}
	return strings.NewReplacer("/", "_", " ", "_").Replace(r.Name) + "@" + c
}

var skip = map[string]bool{".git": true, "vendor": true, "node_modules": true}

// IndexRepo writes the shard of one service release; it replaces only that
// release's own shard, so every other release stays searchable. Binary and oversized (>2MB) files are skipped by Zoekt.
// ponytail: ctags off — SCIP already gives symbols, and no ctags binary on the
// air-gapped agent. Turn it on if falcon needs `sym:` queries.
func IndexRepo(indexDir string, r Repo) error {
	branch := r.Release // the branch every file is in, so branch: queries find them
	if branch == "" {
		branch = r.Branch
	}
	desc := zoekt.Repository{Name: r.Name, URL: r.URL, Source: r.Dir,
		Branches: []zoekt.RepositoryBranch{{Name: branch, Version: r.Commit}}}
	if r.Domain != "" {
		desc.Metadata = map[string]string{"domain": r.Domain, "run": r.Run}
	}
	opts := index.Options{IndexDir: indexDir, RepositoryDescription: desc, DisableCTags: true, ShardPrefixOverride: ShardPrefix(r)}
	opts.SetDefaults()
	b, err := index.NewBuilder(opts)
	if err != nil {
		return err
	}
	err = filepath.WalkDir(filepath.Join(r.Dir, r.Root), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, sockets
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(r.Dir, p)
		return b.Add(index.Document{Name: filepath.ToSlash(rel), Content: data, Branches: []string{branch}})
	})
	if err != nil {
		return err
	}
	return b.Finish()
}

// Prune deletes shards whose repositories are all outside keep (services dropped
// from sources.yaml), so falcon never searches code weaver no longer tracks.
func Prune(indexDir string, keep map[string]bool) (int, error) {
	shards, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range shards {
		repos, _, err := index.ReadMetadataPath(s)
		if err != nil {
			continue // not a readable shard; leave it alone
		}
		live := false
		for _, r := range repos {
			live = live || keep[r.Name]
		}
		if !live {
			_ = os.Remove(s + ".meta")
			if err := os.Remove(s); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// Hit is one matching line. A service's release branch and commit live on its
// shard (and in kb.db's domains table / the snapshot), not on each hit.
type Hit struct {
	Repo string `json:"repo"`
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Search runs a Zoekt query over one or more index dirs (one per domain) — what
// falcon does; here so weaver can check its own output (`weaver search`).
func Search(indexDirs []string, q string, max int) ([]Hit, error) {
	return SearchReleases(indexDirs, q, max, nil)
}

// SearchReleases runs q over the releases in current (service -> release name)
// unless q names a branch itself: every release is indexed, and a query that
// doesn't pick one searches the code as it is now.
func SearchReleases(indexDirs []string, q string, max int, current map[string]string) ([]Hit, error) {
	if len(current) > 0 && !strings.Contains(q, "branch:") {
		var or []string
		for svc, rel := range current {
			// repo: is a regex; branch: matches a substring of the branch name
			or = append(or, fmt.Sprintf("(repo:^%s$ branch:%s)", regexp.QuoteMeta(svc), rel))
		}
		sort.Strings(or)
		q = "(" + q + ") (" + strings.Join(or, " or ") + ")"
	}
	parsed, err := query.Parse(q)
	if err != nil {
		return nil, fmt.Errorf("query %q: %w", q, err)
	}
	hits := []Hit{}
	for _, dir := range indexDirs {
		s, err := zsearch.NewDirectorySearcher(dir) // blocks until shards are loaded
		if err != nil {
			return nil, err
		}
		opts := &zoekt.SearchOptions{MaxDocDisplayCount: max}
		opts.SetDefaults()
		res, err := s.Search(context.Background(), parsed, opts)
		s.Close()
		if err != nil {
			return nil, err
		}
		for _, f := range res.Files {
			for _, l := range f.LineMatches {
				hits = append(hits, Hit{Repo: f.Repository, File: f.FileName, Line: l.LineNumber,
					Text: strings.TrimRight(string(l.Line), "\r\n")})
				if len(hits) >= max {
					return hits, nil
				}
			}
		}
	}
	return hits, nil
}

// ReadFile returns one file of one service release from the index: the code
// as that release had it, whether or not a checkout of it still exists.
func ReadFile(indexDirs []string, repo, path, release string) (string, bool, error) {
	q := fmt.Sprintf("repo:^%s$ file:^%s$ branch:%s", regexp.QuoteMeta(repo), regexp.QuoteMeta(path), release)
	parsed, err := query.Parse(q)
	if err != nil {
		return "", false, err
	}
	for _, dir := range indexDirs {
		s, err := zsearch.NewDirectorySearcher(dir)
		if err != nil {
			return "", false, err
		}
		opts := &zoekt.SearchOptions{Whole: true, MaxDocDisplayCount: 1}
		opts.SetDefaults()
		res, err := s.Search(context.Background(), parsed, opts)
		s.Close()
		if err != nil {
			return "", false, err
		}
		for _, f := range res.Files {
			if f.FileName == path {
				return string(f.Content), true, nil
			}
		}
	}
	return "", false, nil
}
