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
	"strings"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	zsearch "github.com/sourcegraph/zoekt/search"
)

// Repo describes one service's checkout.
type Repo struct {
	Name, Dir, URL, Domain, Branch, Commit string
}

var skip = map[string]bool{".git": true, "vendor": true, "node_modules": true}

// IndexRepo (re)writes the shards for one service; Zoekt replaces that repo's
// older shards on Finish. Binary and oversized (>2MB) files are skipped by Zoekt.
// ponytail: ctags off — SCIP already gives symbols, and no ctags binary on the
// air-gapped agent. Turn it on if falcon needs `sym:` queries.
func IndexRepo(indexDir string, r Repo) error {
	desc := zoekt.Repository{Name: r.Name, URL: r.URL, Source: r.Dir,
		Branches: []zoekt.RepositoryBranch{{Name: r.Branch, Version: r.Commit}}}
	if r.Domain != "" {
		desc.Metadata = map[string]string{"domain": r.Domain}
	}
	opts := index.Options{IndexDir: indexDir, RepositoryDescription: desc, DisableCTags: true}
	opts.SetDefaults()
	b, err := index.NewBuilder(opts)
	if err != nil {
		return err
	}
	err = filepath.WalkDir(r.Dir, func(p string, d os.DirEntry, err error) error {
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
		return b.AddFile(filepath.ToSlash(rel), data)
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
