package extract

// Call criticality (edges spec §5.1): does a failure of the call at a line reach
// the caller? Read from the code around the call, up to its nearest handler.
// ponytail: one level of syntax — a helper that rethrows, or a global handler,
// is not followed; those come out `unknown` and go to the review queue.

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// exceptions a catch clause must name to cover a transport call.
var coveringCatch = map[string]bool{
	"Exception": true, "Throwable": true, "RuntimeException": true, "IOException": true,
	"RestClientException": true, "RestClientResponseException": true, "HttpStatusCodeException": true,
	"HttpClientErrorException": true, "HttpServerErrorException": true, "ResourceAccessException": true,
	"FeignException": true, "WebClientException": true, "WebClientResponseException": true,
	"KafkaException": true, "DataAccessException": true, "RedisConnectionFailureException": true,
}

// criticality of the call on 0-based line, spanning columns [from, to).
func (f *file) criticality(line, from, to int, kind string) string {
	n := f.tree.RootNode().NamedDescendantForPointRange(
		sitter.Point{Row: uint(line), Column: uint(from)}, sitter.Point{Row: uint(line), Column: uint(to)})
	if n == nil {
		return "unknown"
	}
	if f.lang == "java" {
		return f.javaCrit(n, kind, line)
	}
	return f.goCrit(n)
}

func same(a, b *sitter.Node) bool { return a != nil && b != nil && a.Id() == b.Id() }

func (f *file) has(n *sitter.Node, kinds ...string) bool {
	found := false
	walk(n, func(c *sitter.Node) bool {
		for _, k := range kinds {
			if c.Kind() == k {
				found = true
			}
		}
		return !found
	})
	return found
}

func (f *file) javaCrit(n *sitter.Node, kind string, line int) string {
	if kind == "kafka" {
		lt := strings.Split(string(f.src), "\n")[line]
		if !strings.Contains(lt, ".get(") && !strings.Contains(lt, ".join(") {
			return "async" // KafkaTemplate.send returns a future nobody waits on
		}
	}
	prev := n
	for p := n.Parent(); p != nil; prev, p = p, p.Parent() {
		switch p.Kind() {
		case "lambda_expression":
			return "unknown" // runs whenever the lambda does
		case "try_statement", "try_with_resources_statement":
			if !same(prev, p.ChildByFieldName("body")) {
				continue // the call is in a catch / finally, not guarded by this try
			}
			for _, c := range children(p) {
				if c.Kind() == "catch_clause" && f.catchCovers(c) {
					return f.catchBody(c.ChildByFieldName("body"))
				}
			}
		case "method_declaration", "constructor_declaration":
			for _, c := range children(p) {
				if c.Kind() != "modifiers" {
					continue
				}
				m := f.text(c)
				switch {
				case strings.Contains(m, "@Async"):
					return "async"
				case strings.Contains(m, "fallbackMethod"): // @CircuitBreaker / @HystrixCommand
					return "fallback"
				}
			}
			return "required"
		}
	}
	return "required"
}

func (f *file) catchCovers(c *sitter.Node) bool {
	for _, p := range children(c) {
		if p.Kind() != "catch_formal_parameter" {
			continue
		}
		for _, t := range children(p) {
			if t.Kind() != "catch_type" {
				continue
			}
			for _, name := range strings.Split(f.text(t), "|") {
				name = strings.TrimSpace(name)
				if coveringCatch[name[strings.LastIndex(name, ".")+1:]] {
					return true
				}
			}
		}
	}
	return false
}

// catchBody: what a covering catch does with the error.
func (f *file) catchBody(b *sitter.Node) string {
	if b == nil {
		return "unknown"
	}
	switch {
	case f.has(b, "throw_statement"):
		return "required"
	case f.has(b, "return_statement", "assignment_expression"):
		return "fallback"
	}
	for _, s := range children(b) {
		if !s.IsNamed() || strings.HasSuffix(s.Kind(), "comment") {
			continue
		}
		if s.Kind() != "expression_statement" || !isLog(f.text(s)) {
			return "unknown" // calls a helper, sets state, ...
		}
	}
	return "optional" // empty, or only logs
}

func isLog(stmt string) bool {
	s := strings.ToLower(stmt)
	for _, p := range []string{"log.", "logger.", "slog.", "e.printstacktrace", "system.out.", "system.err.", "fmt.print", "klog."} {
		if strings.HasPrefix(s, p) || strings.Contains(s, "."+p) {
			return true
		}
	}
	return false
}

func (f *file) goCrit(n *sitter.Node) string {
	// the statement holding the call, and whether it runs in its own goroutine
	var stmt *sitter.Node
	for p := n; p != nil; p = p.Parent() {
		switch p.Kind() {
		case "go_statement":
			return "async"
		case "function_declaration", "method_declaration":
			p = nil
		}
		if p == nil {
			break
		}
		if stmt == nil && p.Parent() != nil {
			if k := p.Parent().Kind(); k == "block" || k == "statement_list" {
				stmt = p
			}
		}
	}
	if stmt == nil {
		return "unknown"
	}
	errName := ""
	var check *sitter.Node // the statement that looks at the error
	switch stmt.Kind() {
	case "return_statement":
		return "required" // `return client.Do(req)`: the caller gets the error
	case "expression_statement":
		return "optional" // result, error included, dropped
	case "short_var_declaration", "assignment_statement":
		errName = lastName(f, stmt.ChildByFieldName("left"))
		check = stmt.NextNamedSibling()
	case "if_statement":
		if init := stmt.ChildByFieldName("initializer"); init != nil {
			errName = lastName(f, init.ChildByFieldName("left"))
		}
		check = stmt
	}
	switch {
	case errName == "_":
		return "optional"
	case errName == "" || check == nil:
		return "unknown"
	}
	if check.Kind() == "return_statement" && strings.Contains(f.text(check), errName) {
		return "required"
	}
	if check.Kind() != "if_statement" || !strings.Contains(f.text(check.ChildByFieldName("condition")), errName+" != nil") {
		return "unknown"
	}
	body := check.ChildByFieldName("consequence")
	t := f.text(body)
	switch {
	case strings.Contains(t, "panic(") || strings.Contains(t, ".Fatal"):
		return "required"
	case f.has(body, "return_statement"):
		if strings.Contains(t, errName) || strings.Contains(t, "Errorf") || strings.Contains(t, "errors.") || strings.Contains(t, "status.Error") {
			return "required"
		}
		return "fallback"
	}
	return "optional" // logs, continues, or ignores
}

func lastName(f *file, list *sitter.Node) string {
	if list == nil {
		return ""
	}
	names := namedArgs(list)
	if len(names) == 0 {
		return strings.TrimSpace(f.text(list))
	}
	return strings.TrimSpace(f.text(names[len(names)-1]))
}
