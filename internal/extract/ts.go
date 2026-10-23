package extract

// Tree-sitter passes over one parsed file. Each pass walks the AST with the same
// stack discipline as the original Python (pop last, push children), so results
// come out in the same order.
//
// Scope is deliberately narrow: calls() only re-reads arguments and classifies
// each — string literal, an env lookup (System.getenv / os.Getenv), or dynamic.
// Config placeholders, profiles, concat resolution stay in normalizeChannel.

import (
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjava "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

var (
	langJava = sitter.NewLanguage(tsjava.Language())
	langGo   = sitter.NewLanguage(tsgo.Language())
)

// Arg is one classified call argument: a literal / ${ENV} placeholder
// (Unquoted=false), or the argument's whole source text (Unquoted=true).
type Arg struct {
	Value    string
	Unquoted bool
	Line     int // 0-based source line of the argument (SCIP's numbering)
}

// Call is one call site: receiver text ("" when none), method name, arguments.
type Call struct {
	Obj, Method string
	Args        []Arg
}

// Annotation is a Java annotation with its arguments; the unnamed one is "default".
type Annotation struct {
	Name   string
	Kwargs map[string]Arg
}

type file struct {
	lang string // "java" | "go"
	src  []byte
	tree *sitter.Tree
}

func parse(src []byte, lang string) *file {
	p := sitter.NewParser()
	defer p.Close()
	if lang == "java" {
		_ = p.SetLanguage(langJava)
	} else {
		_ = p.SetLanguage(langGo)
	}
	return &file{lang: lang, src: src, tree: p.Parse(src, nil)}
}

func (f *file) close() { f.tree.Close() }

func (f *file) text(n *sitter.Node) string { return n.Utf8Text(f.src) }

func children(n *sitter.Node) []*sitter.Node {
	out := make([]*sitter.Node, 0, n.ChildCount())
	for i := uint(0); i < n.ChildCount(); i++ {
		out = append(out, n.Child(i))
	}
	return out
}

// walk visits nodes in the Python stack order; visit returns false to not descend.
func walk(root *sitter.Node, visit func(n *sitter.Node) bool) {
	stack := []*sitter.Node{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visit(n) {
			stack = append(stack, children(n)...)
		}
	}
}

// strlit: a string literal's content without quotes, ok=false if n isn't one.
func (f *file) strlit(n *sitter.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	switch n.Kind() {
	case "string_literal", "interpreted_string_literal", "raw_string_literal":
		t := f.text(n)
		if len(t) >= 2 && strings.ContainsRune("\"`", rune(t[0])) && strings.ContainsRune("\"`", rune(t[len(t)-1])) {
			return t[1 : len(t)-1], true
		}
	}
	return "", false
}

// callParts: (name, object, args) of a Java method_invocation or Go call_expression.
func (f *file) callParts(n *sitter.Node) (name, obj, args *sitter.Node) {
	if f.lang == "java" {
		return n.ChildByFieldName("name"), n.ChildByFieldName("object"), n.ChildByFieldName("arguments")
	}
	fn := n.ChildByFieldName("function")
	args = n.ChildByFieldName("arguments")
	if fn == nil {
		return nil, nil, args
	}
	if fn.Kind() == "selector_expression" {
		return fn.ChildByFieldName("field"), fn.ChildByFieldName("operand"), args
	}
	return fn, nil, args // bare call, no receiver
}

func namedArgs(args *sitter.Node) []*sitter.Node {
	if args == nil {
		return nil
	}
	var out []*sitter.Node
	for _, c := range children(args) {
		if c.IsNamed() {
			out = append(out, c)
		}
	}
	return out
}

func firstArg(args *sitter.Node) *sitter.Node {
	if a := namedArgs(args); len(a) > 0 {
		return a[0]
	}
	return nil
}

// envKey: X for System.getenv("X") / os.Getenv("X").
func (f *file) envKey(n *sitter.Node) (string, bool) {
	if n == nil || (n.Kind() != "method_invocation" && n.Kind() != "call_expression") {
		return "", false
	}
	name, obj, args := f.callParts(n)
	nm, ob := "", ""
	if name != nil {
		nm = f.text(name)
	}
	if obj != nil {
		ob = f.text(obj)
	}
	if !(f.lang == "java" && ob == "System" && nm == "getenv") && !(f.lang == "go" && ob == "os" && nm == "Getenv") {
		return "", false
	}
	return f.strlit(firstArg(args))
}

func (f *file) resolveArg(n *sitter.Node) Arg {
	line := int(n.StartPosition().Row)
	if s, ok := f.strlit(n); ok {
		return Arg{s, false, line}
	}
	if k, ok := f.envKey(n); ok {
		return Arg{"${" + k + "}", false, line}
	}
	return Arg{f.text(n), true, line} // var / concat / call -> hand over whole
}

func (f *file) calls() []Call {
	kind := "call_expression"
	if f.lang == "java" {
		kind = "method_invocation"
	}
	var out []Call
	walk(f.tree.RootNode(), func(n *sitter.Node) bool {
		if n.Kind() == kind {
			if name, obj, args := f.callParts(n); name != nil {
				c := Call{Method: f.text(name)}
				if obj != nil {
					c.Obj = f.text(obj)
				}
				for _, a := range namedArgs(args) {
					c.Args = append(c.Args, f.resolveArg(a))
				}
				out = append(out, c)
			}
		}
		return true
	})
	return out
}

func (f *file) stringLiterals() []string {
	var out []string
	walk(f.tree.RootNode(), func(n *sitter.Node) bool {
		if s, ok := f.strlit(n); ok {
			out = append(out, s)
			return false
		}
		return true
	})
	return out
}

var chiVerbs = map[string]bool{"Get": true, "Post": true, "Put": true, "Delete": true, "Patch": true, "Head": true}

// goChiRoutes: chi/gin route paths with every enclosing r.Route(prefix, …) prefix
// prepended, outermost first. Only string-literal paths/prefixes.
func (f *file) goChiRoutes() []string {
	var out []string
	walk(f.tree.RootNode(), func(n *sitter.Node) bool {
		if n.Kind() == "call_expression" {
			name, _, args := f.callParts(n)
			if name != nil && chiVerbs[f.text(name)] {
				if path, ok := f.strlit(firstArg(args)); ok {
					out = append(out, f.routePrefix(n)+path)
				}
			}
		}
		return true
	})
	return out
}

func (f *file) routePrefix(n *sitter.Node) string {
	var prefixes []string
	for cur := n.Parent(); cur != nil; cur = cur.Parent() {
		if cur.Kind() != "call_expression" {
			continue
		}
		if name, _, args := f.callParts(cur); name != nil && f.text(name) == "Route" {
			if p, ok := f.strlit(firstArg(args)); ok {
				prefixes = append(prefixes, p)
			}
		}
	}
	var b strings.Builder
	for i := len(prefixes) - 1; i >= 0; i-- {
		b.WriteString(prefixes[i])
	}
	return b.String()
}

type keyVals struct {
	Key  string
	Vals []string
}

// goStructFields: keyed elements `Key: <...strings...>` with their non-empty literals.
func (f *file) goStructFields() []keyVals {
	var out []keyVals
	walk(f.tree.RootNode(), func(n *sitter.Node) bool {
		if n.Kind() != "keyed_element" {
			return true
		}
		kids := children(n)
		var key *sitter.Node
		for _, c := range kids {
			if c.Kind() == "literal_element" || c.Kind() == "identifier" {
				key = c
				break
			}
		}
		if key != nil && len(kids) > 0 {
			var vals []string
			vs := append([]*sitter.Node(nil), kids[1:]...)
			for len(vs) > 0 {
				vn := vs[len(vs)-1]
				vs = vs[:len(vs)-1]
				if s, _ := f.strlit(vn); s != "" {
					vals = append(vals, s)
				} else {
					vs = append(vs, children(vn)...)
				}
			}
			if len(vals) > 0 {
				out = append(out, keyVals{f.text(key), vals})
			}
		}
		return true
	})
	return out
}

// goTopicVars: `var x = []string{...}` declarations with their literals in order.
func (f *file) goTopicVars() []keyVals {
	var out []keyVals
	walk(f.tree.RootNode(), func(n *sitter.Node) bool {
		if n.Kind() != "var_spec" && n.Kind() != "short_var_declaration" {
			return true
		}
		name := n.ChildByFieldName("name")
		if name == nil {
			for _, c := range children(n) {
				if c.Kind() == "identifier" {
					name = c
					break
				}
			}
		}
		if comp := find(n, "composite_literal"); name != nil && comp != nil {
			if vals := f.childStrlits(comp, nil); len(vals) > 0 {
				out = append(out, keyVals{f.text(name), vals})
			}
		}
		return true
	})
	return out
}

// find: first descendant (not n itself) of the given kind, Python stack order.
func find(n *sitter.Node, kind string) *sitter.Node {
	var hit *sitter.Node
	walk(n, func(m *sitter.Node) bool {
		if hit != nil {
			return false
		}
		if m.Id() != n.Id() && m.Kind() == kind {
			hit = m
			return false
		}
		return true
	})
	return hit
}

func (f *file) childStrlits(n *sitter.Node, acc []string) []string {
	if s, ok := f.strlit(n); ok {
		return append(acc, s)
	}
	for _, c := range children(n) {
		acc = f.childStrlits(c, acc)
	}
	return acc
}

func (f *file) annotations() []Annotation {
	if f.lang != "java" {
		return nil
	}
	var out []Annotation
	walk(f.tree.RootNode(), func(n *sitter.Node) bool {
		if n.Kind() == "annotation" {
			if name := n.ChildByFieldName("name"); name != nil {
				a := Annotation{Name: f.text(name), Kwargs: map[string]Arg{}}
				if args := n.ChildByFieldName("arguments"); args != nil {
					for _, c := range children(args) {
						switch c.Kind() {
						case "element_value_pair":
							k, v := c.ChildByFieldName("key"), c.ChildByFieldName("value")
							if k != nil && v != nil {
								a.Kwargs[f.text(k)] = f.resolveArg(v)
							}
						case "(", ")", ",":
						default:
							a.Kwargs["default"] = f.resolveArg(c)
						}
					}
				}
				out = append(out, a)
			}
		}
		return true
	})
	return out
}

// ConstDef is a string constant a file declares, at its name's 0-based line.
type ConstDef struct {
	Name, Value string
	Line        int
}

// constants: the string constants a file declares — Java
// static final (and interface) String fields, Go package-level const/var with a
// string literal. Locals inside methods/functions never count.
func (f *file) constants() []ConstDef {
	var out []ConstDef
	root := f.tree.RootNode()
	if f.lang == "go" {
		for _, decl := range children(root) {
			if decl.Kind() != "const_declaration" && decl.Kind() != "var_declaration" {
				continue
			}
			walk(decl, func(n *sitter.Node) bool {
				if n.Kind() != "const_spec" && n.Kind() != "var_spec" {
					return true
				}
				name, val := n.ChildByFieldName("name"), n.ChildByFieldName("value")
				if name != nil && val != nil {
					if s, ok := f.strlit(firstArg(val)); ok && s != "" {
						out = append(out, ConstDef{f.text(name), s, int(name.StartPosition().Row)})
					}
				}
				return false
			})
		}
		return out
	}
	walk(root, func(n *sitter.Node) bool {
		switch n.Kind() {
		case "method_declaration", "constructor_declaration", "lambda_expression", "block":
			return false // locals
		case "field_declaration", "constant_declaration":
		default:
			return true
		}
		if n.Kind() == "field_declaration" {
			mods := ""
			for _, c := range children(n) {
				if c.Kind() == "modifiers" {
					mods = f.text(c)
				}
			}
			if !strings.Contains(mods, "static") || !strings.Contains(mods, "final") {
				return false
			}
		}
		if t := n.ChildByFieldName("type"); t == nil || f.text(t) != "String" {
			return false
		}
		for _, c := range children(n) {
			if c.Kind() != "variable_declarator" {
				continue
			}
			name, val := c.ChildByFieldName("name"), c.ChildByFieldName("value")
			if name != nil {
				if s, ok := f.strlit(val); ok && s != "" {
					out = append(out, ConstDef{f.text(name), s, int(name.StartPosition().Row)})
				}
			}
		}
		return false
	})
	return out
}
