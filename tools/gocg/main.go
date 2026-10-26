// gocg: interface call sites and the concrete methods VTA says they reach, for
// weaver's entries walk. Same output as tools/javacg: one JSON object per line,
// {"file", "line", "targets": ["`pkg`/Type#Method("]}.
// usage: gocg <module root>   (run where the module's dependencies are downloaded)
package main

import (
	"encoding/json"
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

func main() {
	root, _ := filepath.Abs(os.Args[1])
	cfg := &packages.Config{Mode: packages.LoadAllSyntax, Dir: root, Tests: false}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	fns := ssautil.AllFunctions(prog)
	cg := vta.CallGraph(fns, cha.CallGraph(prog))
	enc := json.NewEncoder(os.Stdout)
	for fn, node := range cg.Nodes {
		if fn == nil {
			continue
		}
		bySite := map[ssa.CallInstruction][]string{}
		for _, e := range node.Out {
			if e.Site == nil || !e.Site.Common().IsInvoke() || e.Callee.Func == nil {
				continue // static calls: SCIP already sees them
			}
			bySite[e.Site] = append(bySite[e.Site], target(e.Callee.Func))
		}
		for site, ts := range bySite {
			pos := prog.Fset.Position(site.Pos())
			rel, err := filepath.Rel(root, pos.Filename)
			if err != nil || pos.Line == 0 || filepath.IsAbs(rel) || rel[:2] == ".." {
				continue // a dependency's code
			}
			sort.Strings(ts)
			enc.Encode(map[string]any{"file": filepath.ToSlash(rel), "line": pos.Line, "targets": ts})
		}
	}
}

// target: the method in SCIP's descriptor form, `pkg`/Type#Method(
func target(f *ssa.Function) string {
	recv := f.Signature.Recv()
	if recv == nil || f.Pkg == nil {
		return f.String()
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	name := t.String()
	if n, ok := t.(*types.Named); ok {
		name = n.Obj().Name()
	}
	return fmt.Sprintf("`%s`/%s#%s(", f.Pkg.Pkg.Path(), name, f.Name())
}
