package main

import (
	"fmt"
	"os"
	"sort"

	"weaver/internal/build"
	"weaver/internal/kb"
	"weaver/internal/snapshot"
)

// zsh completion; dynamic values come from `weaver __complete`, run with the
// same binary the user typed, so it sees the same KNOWLEDGE_DIR / domain.
const zshCompletion = `#compdef weaver
_weaver() {
  local w=${words[1]}
  if (( CURRENT == 2 )); then compadd build use query completion help; return; fi
  case ${words[2]} in
    build) compadd -- --no-fetch --reindex --graph ${(f)"$($w __complete configured 2>/dev/null)"} ;;
    use)
      if (( CURRENT == 3 )); then compadd domain
      else compadd -- --all ${(f)"$($w __complete domains 2>/dev/null)"}; fi ;;
    query)
      if (( CURRENT == 3 )); then compadd def refs flow blast context search graph versions diff help; return; fi
      case ${words[3]} in
        def|refs) (( CURRENT == 4 )) && compadd -- ${(f)"$($w __complete symbols ${words[CURRENT]} 2>/dev/null)"} ;;
        flow)     (( CURRENT == 4 )) && compadd -- ${(f)"$($w __complete services 2>/dev/null)"} ;;
        context)  compadd -- ${(f)"$($w __complete services 2>/dev/null)"} ;;
        blast)
          case $CURRENT in
            4) compadd -- ${(f)"$($w __complete services 2>/dev/null)"} ;;
            5) compadd downstream upstream both ;;
          esac ;;
        graph) compadd -- --infra ;;
        diff) (( CURRENT <= 5 )) && compadd -- ${(f)"$($w __complete versions 2>/dev/null)"} ;;
      esac ;;
  esac
}
compdef _weaver weaver
`

// cmdComplete prints one candidate per line for the completion script.
func cmdComplete(layout build.Layout, args []string) {
	var out []string
	switch arg(args, 0, "") {
	case "domains":
		out = layout.Domains()
	case "configured":
		out = layout.Configured()
	case "services":
		if mine := domainServices(layout); mine != nil {
			for s := range mine {
				out = append(out, s)
			}
		} else {
			for _, d := range layout.Domains() {
				m, _ := snapshot.ReadManifest(layout.Domain(d))
				for _, s := range m {
					out = append(out, s.ServiceName)
				}
			}
		}
	case "symbols":
		var k *kb.KB
		var err error
		if d := currentDomain(layout); d != "" {
			k, err = kb.Open(layout.KB(d))
		} else {
			k, err = kb.OpenKnowledge(layout.Root)
		}
		if err != nil {
			return
		}
		out, _ = k.SymbolNames(arg(args, 1, ""), 200)
		k.Close()
	case "versions":
		if d := currentDomain(layout); d != "" {
			all, _ := snapshot.LoadAll(layout.Versions(d))
			for _, s := range all {
				out = append(out, fmt.Sprint(s.Version))
			}
		}
	}
	sort.Strings(out)
	for _, s := range out {
		fmt.Fprintln(os.Stdout, s)
	}
}
