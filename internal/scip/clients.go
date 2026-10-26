package scip

import (
	"bufio"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"weaver/internal/extract"
)

// clientAPI: a client library whose method calls are a call to another system.
// A call counts only when SCIP resolves its method to one of types; verbs decide
// read (consume) or write (produce), and arg is where the resource is (-1: the
// first literal argument, else argument 0). A method in neither verb list is
// no call (e.g. opsForValue(), a builder step).
type clientAPI struct {
	kind        string
	types       []string // SCIP symbol substrings of the client's types
	write, read []string // method names, lower-case; "*" = any other method
	arg, goArg  int      // the resource argument in Java and in Go (Go passes ctx first)
}

// ponytail: verb lists per library, not each method's signature; add a method
// when a client's call is missed or misread.
var clientAPIs = []clientAPI{
	{kind: "redis", types: []string{"/data/redis/core/", "/data/redis/connection/", "org/redisson/api/", "redis/clients/jedis/", "io/lettuce/core/", "go-redis/", "gomodule/redigo/"},
		write: []string{"set", "setex", "setnx", "setifabsent", "setifpresent", "getandset", "put", "putall", "putifabsent", "increment", "decrement", "incr", "incrby", "decr", "decrby",
			"delete", "del", "unlink", "expire", "expireat", "persist", "leftpush", "leftpushall", "rightpush", "rightpushall", "leftpop", "rightpop", "lpush", "rpush", "lpop", "rpop",
			"add", "sadd", "remove", "srem", "zadd", "zrem", "hset", "hmset", "hdel", "hincrby", "publish", "convertandsend", "rename"},
		read: []string{"get", "multiget", "mget", "haskey", "exists", "hget", "hgetall", "hmget", "hexists", "hkeys", "hvals", "entries", "values", "keys",
			"range", "lrange", "index", "lindex", "size", "llen", "scard", "members", "smembers", "ismember", "sismember", "zrange", "zrangebyscore", "zscore", "getexpire", "ttl"},
		arg: 0, goArg: 1},
	{kind: "kafka", types: []string{"/kafka/core/KafkaTemplate#", "/kafka/core/KafkaOperations#", "/kafka/clients/producer/", "confluent-kafka-go/"},
		write: []string{"send", "produce", "sendandreceive"}, arg: 0, goArg: 0},
	{kind: "rest", types: []string{"/web/client/RestTemplate#", "/web/client/RestOperations#"},
		write: []string{"getforobject", "getforentity", "postforobject", "postforentity", "postforlocation", "exchange", "put", "delete", "patchforobject", "headforheaders", "optionsforallow", "execute"},
		arg:   0},
	{kind: "rest", types: []string{"/web/reactive/function/client/WebClient", "/web/client/RestClient#"},
		write: []string{"uri"}, arg: 0},
	{kind: "rest", types: []string{"`net/http`/"},
		write: []string{"get", "post", "head", "postform", "newrequest", "newrequestwithcontext"}, arg: 0, goArg: 0},
	{kind: "rest", types: []string{"go-resty/"},
		write: []string{"get", "post", "put", "patch", "delete", "head", "options", "execute"}, arg: 0, goArg: 0},
}

// hit classifies one method symbol: a client API, a wrapper, generated
// gRPC code, or a client type answered with `weaver answer` (answered).
func (c *classifier) hit(sym string) (extract.Hit, bool) {
	if w, ok := c.wrappers[sym]; ok {
		return extract.Hit{Kind: w.Kind, Role: w.Role, Arg: -1}, true
	}
	if h, ok := c.grpc(sym); ok {
		return h, true
	}
	m := strings.ToLower(MethodName(sym))
	goSym := strings.HasPrefix(sym, "scip-go ")
	for _, a := range clientAPIs {
		if !containsAny(sym, a.types) {
			continue
		}
		arg := a.arg
		if goSym {
			arg = a.goArg
		}
		if m == "newrequestwithcontext" {
			arg = 2 // (ctx, method, url)
		} else if m == "newrequest" {
			arg = 1 // (method, url)
		}
		switch {
		case in(a.write, m):
			return extract.Hit{Kind: a.kind, Role: "produce", Arg: arg}, true
		case in(a.read, m):
			return extract.Hit{Kind: a.kind, Role: "consume", Arg: arg}, true
		}
		return extract.Hit{}, false
	}
	for typ, kind := range c.answered {
		if strings.Contains(sym, typ) {
			role := "produce"
			if readVerb.MatchString(m) {
				role = "consume"
			}
			return extract.Hit{Kind: kind, Role: role, Arg: -1}, true
		}
	}
	return extract.Hit{}, false
}

// readVerb: a method of an answered client type that reads, by its name.
// ponytail: a name prefix, only for types a person confirmed are clients.
var readVerb = regexp.MustCompile(`^(get|find|read|list|query|search|select|count|exists|has|is|load|fetch|receive|poll|consume|scan)`)

type classifier struct {
	kb       *sql.DB
	dir      func(repo string) string
	wrappers map[string]extract.Wrapper
	answered map[string]string // client type (symbol up to '#') -> kind
	defPath  map[string][2]string
	grpcFile map[string]*grpcGen
	external map[string]bool
}

// ClientCalls: for every repo in kb, each call site SCIP resolves to a client
// API, a wrapper or a generated gRPC method — by (path, 0-based line), then by
// method name — with what it is. The extractor reads only its arguments from
// the source. Unknown lists, per repo, the external types called from its code
// that look like clients but aren't classified (UnknownClient), so a person
// (or an agent) can say what they are; only from files the service owns (its
// root in a monorepo; nil = all). answered maps a client type to a kind.
func ClientCalls(kb *sql.DB, dir func(repo string) string, owns func(repo, path string) bool, wrappers map[string]extract.Wrapper, answered map[string]string) (map[string]map[extract.Site]map[string][]extract.Hit, map[string]map[string]*UnknownClient, error) {
	out := map[string]map[extract.Site]map[string][]extract.Hit{}
	unknown := map[string]map[string]*UnknownClient{}
	if ok, err := hasSCIP(kb); err != nil || !ok {
		return out, unknown, err
	}
	c := &classifier{kb: kb, dir: dir, wrappers: wrappers, answered: answered,
		defPath: map[string][2]string{}, grpcFile: map[string]*grpcGen{}, external: map[string]bool{}}
	// a method or function reference: Java `Type#m().`, Go `pkg`/F(). or Type#M. (no parens)
	rows, err := kb.Query(`SELECT repo, path, start_line, symbol FROM occurrences WHERE (role & 1) = 0
		AND (symbol LIKE '%).' OR (symbol LIKE 'scip-go %#%.' AND symbol NOT LIKE '%#'))`)
	if err != nil {
		return nil, nil, err
	}
	type ref struct {
		repo, path, sym string
		line            int
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.repo, &r.path, &r.line, &r.sym); err != nil {
			rows.Close()
			return nil, nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	verdict := map[string]*extract.Hit{} // per symbol: nil = not a client call
	for _, r := range refs {
		h, seen := verdict[r.sym]
		if !seen {
			if v, ok := c.hit(r.sym); ok {
				h = &v
			}
			verdict[r.sym] = h
		}
		if h == nil {
			if owns == nil || owns(r.repo, r.path) {
				c.noteUnknown(unknown, r.repo, r.path, r.line, r.sym)
			}
			continue
		}
		if out[r.repo] == nil {
			out[r.repo] = map[extract.Site]map[string][]extract.Hit{}
		}
		s := extract.Site{Path: r.path, Line: r.line}
		if out[r.repo][s] == nil {
			out[r.repo][s] = map[string][]extract.Hit{}
		}
		name := CallName(r.sym)
		out[r.repo][s][name] = append(out[r.repo][s][name], *h)
	}
	return out, unknown, nil
}

// CallName: the name a call site writes for sym: a method's name, or a
// package-level function's (Go `pkg`/RegisterCartServer().).
func CallName(sym string) string {
	if m := MethodName(sym); m != "" {
		return m
	}
	s := strings.TrimSuffix(strings.TrimSpace(sym), "().")
	return strings.Trim(s[strings.LastIndexAny(s, "/` ")+1:], "`")
}

// UnknownClient: an external type a service calls that looks like a client of
// another system but isn't classified: a `client` question.
type UnknownClient struct {
	Type    string   // its SCIP symbol, up to and including '#'
	Calls   int      // call sites
	Methods []string // the methods called, sorted
	Sites   []extract.Site
}

// clientName: external type names worth asking about. Only to ASK: an edge
// comes from a classified type, never from this.
// ponytail: a name suffix list; an agent reading the dependency list replaces it.
var clientName = regexp.MustCompile(`(Template|Client|Operations|Repository|Producer|Consumer|Publisher|Subscriber|Connection|Session|Channel|Stub|Sender|Receiver)#$`)

func (c *classifier) noteUnknown(unknown map[string]map[string]*UnknownClient, repo, path string, line int, sym string) {
	i := strings.LastIndex(sym, "#")
	if i < 0 {
		return
	}
	typ := sym[:i+1]
	if !clientName.MatchString(typ) || strings.HasPrefix(typ, "scip-java maven jdk ") || strings.Contains(typ, " github.com/golang/go ") ||
		strings.Contains(typ, "/annotation/") || strings.HasSuffix(path, ".pb.go") {
		return // the JDK, Go's std, annotation attributes, calls inside generated code
	}
	for _, a := range clientAPIs { // a known client's other methods (opsForValue()) aren't unknown
		if containsAny(sym, a.types) {
			return
		}
	}
	for t := range c.answered {
		if strings.Contains(sym, t) {
			return
		}
	}
	ext, ok := c.external[typ]
	if !ok {
		var n int
		_ = c.kb.QueryRow(`SELECT count(*) FROM occurrences WHERE symbol = ? AND (role & 1) = 1`, typ).Scan(&n)
		ext = n == 0 // no definition in any indexed code: a library's type
		c.external[typ] = ext
	}
	if !ext {
		return
	}
	if unknown[repo] == nil {
		unknown[repo] = map[string]*UnknownClient{}
	}
	u := unknown[repo][typ]
	if u == nil {
		u = &UnknownClient{Type: typ}
		unknown[repo][typ] = u
	}
	u.Calls++
	if m := MethodName(sym); !in(u.Methods, m) {
		u.Methods = append(u.Methods, m)
		sort.Strings(u.Methods)
	}
	if len(u.Sites) < 10 {
		u.Sites = append(u.Sites, extract.Site{Path: path, Line: line})
	}
}

// ---- generated gRPC code ----

// grpcGen: what one *_grpc.pb.go file generates: client methods with their full
// method names, and Register<X>Server functions with their service's methods.
type grpcGen struct {
	client   map[string]string   // "CartClient#GetCart" and "cartClient#GetCart" -> "cart.service.v1.Cart/GetCart"
	register map[string][]string // "RegisterCartServer" -> its methods' full names
}

// grpc: a call SCIP resolves into protoc-gen-go-grpc's generated code. The
// resource is the method's full name the generated code itself sends
// (edges-spec §3.2): never derived from a function's name.
func (c *classifier) grpc(sym string) (extract.Hit, bool) {
	if !strings.HasPrefix(sym, "scip-go ") {
		return extract.Hit{}, false
	}
	dp, ok := c.defPath[sym]
	if !ok {
		_ = c.kb.QueryRow(`SELECT repo, path FROM occurrences WHERE symbol = ? AND (role & 1) = 1 LIMIT 1`, sym).Scan(&dp[0], &dp[1])
		c.defPath[sym] = dp
	}
	if !strings.HasSuffix(dp[1], ".pb.go") { // *_grpc.pb.go, or one .pb.go of the old plugins=grpc generator
		return extract.Hit{}, false
	}
	file := filepath.Join(c.dir(dp[0]), dp[1])
	g, ok := c.grpcFile[file]
	if !ok {
		g = parseGRPC(file)
		c.grpcFile[file] = g
	}
	s := strings.TrimSuffix(strings.TrimSuffix(sym, "()."), ".") // Go methods: Type#Method. ; functions: F().
	desc := s[strings.LastIndexAny(s, "/` ")+1:]                 // CartClient#GetCart or RegisterCartServer
	if full, ok := g.client[desc]; ok {
		return extract.Hit{Kind: "grpc", Role: "produce", Resources: []string{full}}, true
	}
	if ms, ok := g.register[desc]; ok {
		return extract.Hit{Kind: "grpc", Role: "consume", Resources: ms}, true
	}
	return extract.Hit{}, false
}

var (
	reGoMethod  = regexp.MustCompile(`^func \(\w+ \*?(\w+)\) (\w+)\(`)
	reFullConst = regexp.MustCompile(`^\s*(\w+_FullMethodName)\s*=\s*"/([\w.]+/\w+)"`)
	reInvoke    = regexp.MustCompile(`\.(?:Invoke|NewStream)\(ctx,\s*(?:&\w+\.Streams\[\d+\],\s*)?(\w+_FullMethodName|"/[\w.]+/\w+")`)
	reRegister  = regexp.MustCompile(`^func (Register\w+Server)\(`)
	reDescRef   = regexp.MustCompile(`&(\w+_[sS]erviceDesc)\b`) // new X_ServiceDesc, old _X_serviceDesc
	reDescVar   = regexp.MustCompile(`^var (\w+_[sS]erviceDesc) = grpc\.ServiceDesc\{`)
	reSvcName   = regexp.MustCompile(`ServiceName:\s*"([\w.]+)"`)
	reMethName  = regexp.MustCompile(`(?:MethodName|StreamName):\s*"(\w+)"`)
)

// parseGRPC reads a protoc-gen-go-grpc file. ponytail: line patterns of the
// generator's output (old: literal method names; new: *_FullMethodName consts).
func parseGRPC(path string) *grpcGen {
	g := &grpcGen{client: map[string]string{}, register: map[string][]string{}}
	f, err := os.Open(path)
	if err != nil {
		return g
	}
	defer f.Close()
	consts := map[string]string{}
	descs := map[string][]string{} // X_ServiceDesc -> full method names
	regDesc := map[string]string{} // RegisterXServer -> X_ServiceDesc
	var recv, method, reg, desc, svc string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		l := sc.Text()
		if m := reFullConst.FindStringSubmatch(l); m != nil {
			consts[m[1]] = m[2]
		}
		if m := reGoMethod.FindStringSubmatch(l); m != nil {
			recv, method, reg = m[1], m[2], ""
		} else if m := reRegister.FindStringSubmatch(l); m != nil {
			recv, method, reg = "", "", m[1]
		} else if strings.HasPrefix(l, "func ") {
			recv, method, reg = "", "", ""
		}
		if m := reInvoke.FindStringSubmatch(l); m != nil && recv != "" {
			full := strings.Trim(m[1], `"/`)
			if v, ok := consts[m[1]]; ok {
				full = v
			}
			if strings.HasSuffix(strings.ToLower(recv), "client") {
				g.client[recv+"#"+method] = full
				g.client[strings.ToUpper(recv[:1])+recv[1:]+"#"+method] = full // the interface callers hold
			}
		}
		if m := reDescRef.FindStringSubmatch(l); m != nil && reg != "" {
			regDesc[reg] = m[1]
		}
		if m := reDescVar.FindStringSubmatch(l); m != nil {
			desc, svc = m[1], ""
		}
		if desc != "" {
			if m := reSvcName.FindStringSubmatch(l); m != nil {
				svc = m[1]
			}
			if m := reMethName.FindStringSubmatch(l); m != nil && svc != "" {
				descs[desc] = append(descs[desc], svc+"/"+m[1])
			}
			if l == "}" {
				desc = ""
			}
		}
	}
	for r, d := range regDesc {
		g.register[r] = descs[d]
	}
	return g
}

func containsAny(s string, subs []string) bool {
	for _, t := range subs {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func in(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
