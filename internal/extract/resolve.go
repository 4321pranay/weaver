package extract

import (
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
	"strings"
)

// parseYAMLProperties flattens a Spring application.yml into dotted keys. Not a
// YAML parser: indentation-nested `key: value` lines only, which is all Spring
// config channels need.
func parseYAMLProperties(text string) map[string]string {
	config := map[string]string{}
	type level struct {
		indent int
		key    string
	}
	var path []level
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t\v\f"))
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.Trim(strings.TrimSpace(v), `"`), "'")
		for len(path) > 0 && path[len(path)-1].indent >= indent {
			path = path[:len(path)-1]
		}
		if v != "" {
			parts := make([]string, 0, len(path)+1)
			for _, p := range path {
				parts = append(parts, p.key)
			}
			config[strings.Join(append(parts, k), ".")] = v
		} else {
			path = append(path, level{indent, k})
		}
	}
	return config
}

func parseProperties(text string) map[string]string {
	config := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			config[strings.TrimSpace(k)] = strings.Trim(strings.Trim(strings.TrimSpace(v), `"`), "'")
		}
	}
	return config
}

// skipDir: vendored / VCS trees. Same substring test as before — note "/vendor/"
// needs a trailing segment, so files directly in vendor/ are still read.
func skipDir(dir string) bool {
	return strings.Contains(dir, "/.git") || strings.Contains(dir, "/vendor/") || strings.Contains(dir, "/node_modules/")
}

// walkFiles calls fn(dir, name) for every file under root, lexical order.
func walkFiles(root string, fn func(dir, name string)) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if dir := filepath.Dir(p); !skipDir(dir) {
			fn(dir, d.Name())
		}
		return nil
	})
}

// readText reads a file the way Python's text-mode open(errors="ignore") does:
// invalid UTF-8 dropped, \r\n and lone \r turned into \n.
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.ToValidUTF8(string(b), "")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), nil
}

func loadConfig(repoDir string) map[string]string {
	config := map[string]string{}
	walkFiles(repoDir, func(dir, name string) {
		var parsed map[string]string
		switch name {
		case "application.yml", "application.yaml":
			if t, err := readText(filepath.Join(dir, name)); err == nil {
				parsed = parseYAMLProperties(t)
			}
		case "application.properties", ".env":
			if t, err := readText(filepath.Join(dir, name)); err == nil {
				parsed = parseProperties(t)
			}
		}
		for k, v := range parsed {
			config[k] = v
		}
	})
	return config
}

// ServiceConfig: the config of the service whose code is root in repoDir: its
// application.yml / .properties / .env, then the environment: of the Docker
// Compose service built from that folder, anywhere in the repo (a monorepo
// keeps compose files outside every service's folder). Environment wins, as it
// does at runtime.
func ServiceConfig(repoDir, root string) map[string]string {
	code := filepath.Clean(filepath.Join(repoDir, root))
	config := loadConfig(code)
	walkFiles(repoDir, func(dir, name string) {
		if !reCompose.MatchString(name) {
			return
		}
		t, err := readText(filepath.Join(dir, name))
		if err != nil {
			return
		}
		var c struct {
			Services map[string]struct {
				Build       yaml.Node `yaml:"build"`
				Environment yaml.Node `yaml:"environment"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal([]byte(t), &c) != nil {
			return
		}
		for _, svc := range c.Services {
			ctx := svc.Build.Value // build: <dir>
			if svc.Build.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(svc.Build.Content); i += 2 {
					if svc.Build.Content[i].Value == "context" {
						ctx = svc.Build.Content[i+1].Value
					}
				}
			}
			if ctx == "" || filepath.Clean(filepath.Join(dir, ctx)) != code {
				continue
			}
			for k, v := range composeEnv(svc.Environment) {
				config[k] = v
			}
		}
	})
	return config
}

var reCompose = regexp.MustCompile(`^(docker-)?compose[\w.-]*\.ya?ml$`)

// composeEnv: a compose service's environment, as a map or a KEY=value list.
func composeEnv(n yaml.Node) map[string]string {
	out := map[string]string{}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out[n.Content[i].Value] = n.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if k, v, ok := strings.Cut(c.Value, "="); ok {
				out[k] = v
			}
		}
	}
	return out
}

// EnvKeys: the environment variable names Spring binds to a property key
// (relaxed binding): spring.kafka.bootstrap-servers ->
// SPRING_KAFKA_BOOTSTRAPSERVERS and the common SPRING_KAFKA_BOOTSTRAP_SERVERS.
func EnvKeys(key string) []string {
	up := strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
	return []string{strings.ReplaceAll(up, "-", ""), strings.ReplaceAll(up, "-", "_")}
}

var srcExt = []string{".go", ".java", ".kt"}

// IsSource reports whether a file name is a source file the extractor reads.
func IsSource(name string) bool {
	for _, e := range srcExt {
		if strings.HasSuffix(name, e) {
			return true
		}
	}
	return false
}

type srcFile struct {
	name  string
	rel   string // path inside the repo, slash-separated (SCIP's relative_path)
	text  string
	lines []string // text split on \n, for column lookups
}

// IsTest: test code — its fixtures (topics, URLs) aren't the service's wiring.
func IsTest(dir, name string) bool {
	p := "/" + filepath.ToSlash(dir) + "/"
	for _, seg := range []string{"/src/test/", "/test/", "/tests/", "/testdata/"} {
		if strings.Contains(p, seg) {
			return true
		}
	}
	return strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "Test.java") ||
		strings.HasSuffix(name, "Tests.java") || strings.HasSuffix(name, "IT.java")
}

func iterSrc(repoDir string) []srcFile {
	var out []srcFile
	walkFiles(repoDir, func(dir, name string) {
		if !IsSource(name) || IsTest(dir, name) {
			return
		}
		if t, err := readText(filepath.Join(dir, name)); err == nil {
			rel, _ := filepath.Rel(repoDir, filepath.Join(dir, name))
			out = append(out, srcFile{name, filepath.ToSlash(rel), t, strings.Split(t, "\n")})
		}
	})
	return out
}

var (
	reGetter    = regexp.MustCompile(`^\w+\.(get\w+)\(\)$`)
	reFormat    = regexp.MustCompile(`(?:Sprintf|String\.format|fmt\.Errorf)\s*\(\s*"([^"]+)"`)
	reVerb      = regexp.MustCompile(`%[#+\-0-9.]*[a-zA-Z]`)
	reSpel      = regexp.MustCompile(`^#\{@\w+\.(\w+)\}$`)
	reBinding   = regexp.MustCompile(`-(?:out|in)-\d+$`)
	rePlace     = regexp.MustCompile(`\$\{([^}]+)\}`)
	reSpelConst = regexp.MustCompile(`\.([A-Z_][A-Z0-9_]*)}`)
	reDotted    = regexp.MustCompile(`^[A-Za-z0-9_]+\.[A-Za-z0-9_]+$`)
	reScheme    = regexp.MustCompile(`^https?://[^/]+`)
	reBraces    = regexp.MustCompile(`\{[^}]+\}`)
)

// resolveConcat rebuilds `"lit" + var + ...`: known parts (literals, known vars,
// @ConfigurationProperties getters) join; an unknown later part becomes a `{}`
// hole, so `"session:" + userID` -> "session:{}". ok=false when the FIRST part
// is unknown — no leading value worth keeping.
func resolveConcat(expr string, litMap, getterMap map[string]string) (string, bool) {
	parts := strings.Split(expr, "+")
	if len(parts) < 2 {
		return "", false
	}
	var out strings.Builder
	for i, p := range parts {
		p = strings.TrimSpace(p)
		gm := reGetter.FindStringSubmatch(p)
		switch {
		case len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"':
			out.WriteString(p[1 : len(p)-1])
		case has(litMap, p):
			out.WriteString(litMap[p])
		case gm != nil && has(getterMap, gm[1]):
			out.WriteString(getterMap[gm[1]])
		case i == 0:
			return "", false
		default:
			out.WriteString("{}")
		}
	}
	return out.String(), true
}

// resolveFormat: Sprintf / String.format call -> its format string with every
// %verb as `{}`. ok=false if the format arg isn't a string literal.
func resolveFormat(expr string) (string, bool) {
	m := reFormat.FindStringSubmatch(expr)
	if m == nil {
		return "", false
	}
	return reVerb.ReplaceAllString(m[1], "{}"), true
}

func has(m map[string]string, k string) bool { _, ok := m[k]; return ok }

func lastDot(s string) string { return s[strings.LastIndex(s, ".")+1:] }

// maps bundles the per-repo / per-file lookup tables normalizeChannel consults.
type maps struct {
	config, vars, lits, consts, getters map[string]string
	exact                               func(ref string) (string, bool) // SCIP, at the call site
	host                                *string                         // set to a REST URL's host, if it has one
}

var reRef = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

func normalizeChannel(kind, channel string, unquoted bool, m maps) string {
	channel = strings.TrimSpace(channel)

	// SpEL bean reference `#{@topicConfig.orders}` -> the bean getter's value.
	if s := reSpel.FindStringSubmatch(channel); s != nil && has(m.consts, s[1]) {
		channel, unquoted = m.consts[s[1]], false
	}
	// Format-string call, before the literal/concat paths.
	if (strings.Contains(channel, "Sprintf") || strings.Contains(channel, "String.format")) && strings.Contains(channel, "%") {
		if f, ok := resolveFormat(channel); ok {
			channel, unquoted = f, false
		}
	}

	// a constant reference resolved exactly by SCIP beats every name-based guess
	if unquoted && m.exact != nil && !has(m.vars, channel) && reRef.MatchString(channel) {
		if v, ok := m.exact(channel); ok {
			channel, unquoted = v, false
		}
	}

	switch {
	case unquoted && has(m.vars, channel):
		channel, unquoted = "${"+m.vars[channel]+"}", false
	case unquoted && has(m.lits, channel):
		channel, unquoted = m.lits[channel], false // bare known variable
	case unquoted && has(m.consts, channel):
		channel, unquoted = m.consts[channel], false // repo-wide constant
	case unquoted && strings.Contains(channel, ".") && has(m.consts, lastDot(channel)):
		channel, unquoted = m.consts[lastDot(channel)], false // Constants.Topics.CART_EVENTS
	case unquoted && strings.Contains(channel, "+"):
		if j, ok := resolveConcat(channel, m.lits, m.getters); ok {
			channel, unquoted = j, false
		}
	}

	// Spring Cloud Stream: send() takes a BINDING name; the topic is its yaml destination.
	if kind == "kafka" && reBinding.MatchString(channel) {
		if dest := m.config["spring.cloud.stream.bindings."+channel+".destination"]; dest != "" {
			channel, unquoted = dest, false
		}
	}

	original := channel
	channel = rePlace.ReplaceAllStringFunc(channel, func(s string) string {
		key := rePlace.FindStringSubmatch(s)[1]
		if v := m.config[key]; v != "" {
			return v
		}
		if v := os.Getenv(key); v != "" {
			return v
		}
		return "<RUNTIME_PARAMETER: " + key + ">"
	})
	if channel != original {
		unquoted = false
	}

	// SpEL `#{T(Topics).ORDER_PLACED}`: the constant's real value, if known
	if strings.Contains(channel, "#{T") {
		if c := reSpelConst.FindStringSubmatch(channel); c != nil {
			if v, ok := m.consts[c[1]]; ok {
				return v
			}
			return "<DYNAMIC_DISPATCH: " + channel + ">"
		}
	}
	// an unresolved constant stays unresolved: no value is made up from its name
	// (edges spec: weaver never guesses)
	if unquoted && !strings.Contains(channel, "<RUNTIME_PARAMETER") {
		return "<DYNAMIC_DISPATCH: " + channel + ">"
	}
	switch kind {
	case "rest":
		if h := reScheme.FindString(channel); h != "" && m.host != nil {
			*m.host = h[strings.Index(h, "//")+2:]
		}
		channel = reScheme.ReplaceAllString(channel, "")
		channel = reBraces.ReplaceAllString(channel, "{}")
		if strings.HasSuffix(channel, "/") {
			channel += "{}"
		}
	}
	return channel
}

var (
	reBeanGetter = regexp.MustCompile(`\bget([A-Z]\w*)\s*\([^)]*\)\s*\{[^}]*return\s+"([^"]+)"`)
	reViperDef   = regexp.MustCompile(`SetDefault\(\s*"([^"]+)"\s*,\s*"([^"]+)"`)
	reViperGet   = regexp.MustCompile(`\b(\w+)\s*[:=]+\s*viper\.GetString\(\s*"([^"]+)"\s*\)`)
	reFieldSet   = regexp.MustCompile(`\.(\w+)\s*=\s*(\w+)\b`)
)

func setDefault(m map[string]string, k, v string) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

// Consts is a name -> value table where a name seen with two different values is
// Ambiguous: it resolves to nothing rather than to whichever file came first.
type Consts struct {
	Vals      map[string]string
	Ambiguous map[string]bool
}

func (c *Consts) add(k, v string) {
	if c.Ambiguous[k] {
		return
	}
	if old, ok := c.Vals[k]; ok && old != v {
		delete(c.Vals, k)
		c.Ambiguous[k] = true
		return
	}
	c.Vals[k] = v
}

// Over layers c (a service's own repo) over global (every source tree): a name
// the repo defines wins; a name ambiguous in the repo stays unresolved, never
// filled from another repo.
func (c Consts) Over(global Consts) map[string]string {
	out := make(map[string]string, len(global.Vals)+len(c.Vals))
	for k, v := range global.Vals {
		if !c.Ambiguous[k] {
			out[k] = v
		}
	}
	for k, v := range c.Vals {
		out[k] = v
	}
	return out
}

// CollectConstants: the cross-file constant table over every non-test file under
// dirs, keyed by the names a reference site could use. Only REAL constants count —
// Java static final / interface String fields and Go package-level const/var, found
// with tree-sitter — plus bean getters returning a literal and the viper
// SetDefault -> GetString -> .Field chain. A local variable is never a constant.
// ponytail: name-based; the remaining same-name/different-value cases are marked
// ambiguous. A SCIP symbol lookup would resolve those exactly.
func CollectConstants(dirs ...string) Consts {
	c := Consts{Vals: map[string]string{}, Ambiguous: map[string]bool{}}
	for _, d := range dirs {
		for _, f := range iterSrc(d) {
			for _, kv := range fileConstants(f, c.Vals) {
				c.add(kv[0], kv[1])
			}
		}
	}
	return c
}

// FileConstants: the string constants declared in one source file, with lines —
// what a SCIP definition occurrence is matched against to read its value.
func FileConstants(path string) ([]ConstDef, error) {
	t, err := readText(path)
	if err != nil {
		return nil, err
	}
	lang := "go"
	if strings.HasSuffix(path, ".java") || strings.HasSuffix(path, ".kt") {
		lang = "java"
	}
	f := parse([]byte(t), lang)
	defer f.close()
	return f.constants(), nil
}

// fileConstants: (name, value) pairs one file defines. known resolves the viper
// field chain against constants already collected.
func fileConstants(f srcFile, known map[string]string) [][2]string {
	var out [][2]string
	lang := "go"
	if strings.HasSuffix(f.name, ".java") || strings.HasSuffix(f.name, ".kt") {
		lang = "java"
	}
	pf := parse([]byte(f.text), lang)
	for _, c := range pf.constants() {
		out = append(out, [2]string{c.Name, c.Value})
	}
	pf.close()
	t := f.text
	// bean getter `String getOrders() { return "x"; }` keyed as property `orders`
	for _, m := range reBeanGetter.FindAllStringSubmatch(t, -1) {
		out = append(out, [2]string{strings.ToLower(m[1][:1]) + m[1][1:], m[2]})
	}
	// viper chain: SetDefault(key, v) -> x := viper.GetString(key) -> .Field = x
	defs := map[string]string{}
	for _, m := range reViperDef.FindAllStringSubmatch(t, -1) {
		defs[m[1]] = m[2]
	}
	local := map[string]string{}
	for _, m := range reViperGet.FindAllStringSubmatch(t, -1) {
		if v := defs[m[2]]; v != "" {
			setDefault(local, m[1], v)
			out = append(out, [2]string{m[1], v})
		}
	}
	for _, m := range reFieldSet.FindAllStringSubmatch(t, -1) {
		if v, ok := local[m[2]]; ok {
			out = append(out, [2]string{m[1], v})
		} else if v, ok := known[m[2]]; ok {
			out = append(out, [2]string{m[1], v})
		}
	}
	return out
}

var (
	reConfProps = regexp.MustCompile(`@ConfigurationProperties\(\s*(?:prefix\s*=\s*)?"([^"]+)"`)
	reStrField  = regexp.MustCompile(`\bString\s+(\w+)\s*;`)
	reCamel     = regexp.MustCompile(`([a-z])([A-Z])`)
)

// collectGetterConfigs: @ConfigurationProperties(prefix=P) field getters -> their
// yaml value (relaxed binding: ledgerUrl <-> P.ledger-url). One prefix per file.
func collectGetterConfigs(repoDir string, config map[string]string) map[string]string {
	out := map[string]string{}
	for _, f := range iterSrc(repoDir) {
		pm := reConfProps.FindStringSubmatch(f.text)
		if pm == nil {
			continue
		}
		for _, fm := range reStrField.FindAllStringSubmatch(f.text, -1) {
			field := fm[1]
			kebab := strings.ToLower(reCamel.ReplaceAllString(field, "${1}-${2}"))
			v := config[pm[1]+"."+kebab]
			if v == "" {
				v = config[pm[1]+"."+field]
			}
			if v != "" {
				out["get"+strings.ToUpper(field[:1])+field[1:]] = v
			}
		}
	}
	return out
}

// Config: the repo's merged application.yml / .properties / .env keys.
func Config(repoDir string) map[string]string { return loadConfig(repoDir) }
