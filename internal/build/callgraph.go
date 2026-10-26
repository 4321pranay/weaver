package build

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"weaver/internal/edgemap"
	"weaver/internal/scip"
	"weaver/internal/snapshot"
)

// callGraphs runs the interface-call analysers on each service, when they are
// configured: WEAVER_GOCG is the tools/gocg binary (go/callgraph VTA, per go.mod
// root), WEAVER_JAVACG the java classpath of tools/javacg (SootUp, over the
// service's compiled classes). Their answers let the entries walk cross
// interfaces whose implementation is injected.
func callGraphs(l Layout, d string, ms []snapshot.Manifest, log io.Writer) map[string][]edgemap.CGEdge {
	gocg, javacg := os.Getenv("WEAVER_GOCG"), os.Getenv("WEAVER_JAVACG")
	out := map[string][]edgemap.CGEdge{}
	if gocg == "" && javacg == "" {
		return out
	}
	done := map[string][]edgemap.CGEdge{} // a monorepo clone is analysed once
	for _, m := range ms {
		dir := m.Dir(l.Sources(d))
		if rows, ok := done[dir]; ok {
			if len(rows) > 0 {
				out[m.ServiceName] = rows
			}
			continue
		}
		var rows []edgemap.CGEdge
		for _, r := range scip.Roots(dir) {
			if r.Tool != "scip-go" || gocg == "" {
				continue
			}
			got, err := runCG(exec.Command(gocg, filepath.Join(dir, r.Dir)))
			if err != nil {
				fmt.Fprintf(log, "  !! %s: gocg %s: %v\n", m.ServiceName, r.Dir, err)
				continue
			}
			for i := range got {
				got[i].File = filepath.ToSlash(filepath.Join(r.Dir, got[i].File))
			}
			rows = append(rows, got...)
		}
		if javacg != "" {
			var classes []string
			_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
				if err == nil && e.IsDir() && strings.HasSuffix(filepath.ToSlash(p), "target/classes") {
					classes = append(classes, p)
				}
				return nil
			})
			if len(classes) > 0 {
				got, err := runCG(exec.Command("java", "-cp", javacg, "weaver.JavaCG", strings.Join(classes, ":")))
				if err != nil {
					fmt.Fprintf(log, "  !! %s: javacg: %v\n", m.ServiceName, err)
				}
				rows = append(rows, got...)
			}
		}
		done[dir] = rows
		if len(rows) > 0 {
			out[m.ServiceName] = rows
			fmt.Fprintf(log, "  %s: %d interface call sites resolved\n", m.ServiceName, len(rows))
		}
	}
	return out
}

func runCG(cmd *exec.Cmd) ([]edgemap.CGEdge, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	var rows []edgemap.CGEdge
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r edgemap.CGEdge
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			rows = append(rows, r)
		}
	}
	if err != nil && len(rows) == 0 {
		return nil, fmt.Errorf("%v: %s", err, lastLine(stderr.String()))
	}
	return rows, nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
