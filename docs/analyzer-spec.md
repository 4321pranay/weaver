# Analyzer Spec: Facts

A build has two halves:

- **Analyzers**, one per language, read code and report **facts**: what a
  service serves, calls and reads, and what they couldn't establish. All
  language and framework knowledge lives here.
- **The edge mapper** turns every service's facts into the map (edges-spec)
  and questions (questions-spec). It knows no language.

---

## 1. Analyzers

One analyzer runs per build root: `go.mod` → `weave-go`; Maven or Gradle →
`weave-java`. A monorepo is analysed once; each service gets the facts of its
own root.

The analyzers combine four tools (spring-probe in `weave-java` only):

| Tool | Answers | Can't |
|---|---|---|
| SCIP | what a call resolves to; definitions, references, constants | see past interfaces and injection |
| call graph (gocg, javacg) | which implementation an interface call reaches | understand Spring wiring |
| spring-probe | what Spring actually wires: beans, active conditions, proxies, mapped routes, properties | work outside Spring, or without a buildable app |
| tree-sitter | a call's arguments and the code around it (criticality, route prefixes) | know types |

SCIP decides what is a call, tree-sitter reads it, and the call graph and
spring-probe say what runs behind it.

- An analyzer never fetches code and never asks questions.
- The same input always gives the same facts.
- A failure never fails the build; it becomes an `analyzer` question.

Other languages fall back to method names (`source: name`, edges
`match: name`); the `domains` tool reports each service's share of such edges.

---

## 2. Facts

One YAML file per service and build: `knowledge/<domain>/facts/<service>.yaml`.

```yaml
version: weaver/facts/v1.0
service: mall-portal
root: mall-portal
analyzer: {name: weave-java, version: "1.0.0", rules: "boot-3@a41c09"}
entrypoints:
  - {kind: rest, resource: "POST /sso/login", source: spring-probe:runtime,
     handler: {symbol: "…UmsMemberController#login().", at: "mall-portal/…/UmsMemberController.java:52"}}
calls:
  - {kind: redis, role: produce, call: optional, source: scip,
     resource: {expr: "REDIS_KEY_MEMBER + \":\" + id", value: "mall:ums:member:{}"},
     at: "mall-portal/…/UmsMemberCacheServiceImpl.java:41", via: "…RedisService#set()."}
callgraph:
  - {from: "…UmsMemberServiceImpl#getCurrentMember().", to: ["…UmsMemberCacheServiceImpl#getMember()."], source: "javacg:spring"}
config:
  - {key: spring.data.redis.host, value: "localhost", at: "mall-portal/…/application-dev.yml:12", source: files}
gaps:
  - {type: client, subject: {type: "…amqp/core/AmqpTemplate#"}, at: "…/CancelOrderSender.java:24", source: scip}
```

| Field | Holds |
|---|---|
| `entrypoints` | what the service serves (edges-spec §3.5), with its `handler` |
| `calls` | each call to another system: `resource.expr` as written, `resource.value` resolved (`{}` for runtime parts), `via` its wrapper, `call` (§4) |
| `callgraph` | call targets the source doesn't show: interfaces, injected beans, functions passed as values |
| `config` | every non-secret key the code reads, each value with the profile or file it comes from |

Paths are relative to the repo; symbols are SCIP symbols.

---|---|
| `scip` | resolved by SCIP |
| `gocg`, `javacg:<model>` | a call graph |
| `spring-probe:<level>` | `aot`, `runtime` or `static` |
| `files` | config files or Compose |
| `syntax` | tree-sitter structure only |
| `name` | method names only (no SCIP index) |

When facts disagree, the higher one wins: `spring-probe:runtime`,
`spring-probe:aot`, `scip`, call graphs, `spring-probe:static`, `files`,
`syntax`, `name`.

### 2.2 `gaps`

What an analyzer couldn't establish, typed as questions (`resource`, `client`,
`config`, `implementation`, `analyzer`). The mapper raises each as a question
unless another service's facts settle it.

---

## 3. What a call is

A call is reported only where SCIP resolves its method to:

- a known client API (Spring Data Redis, Redisson, Jedis, Lettuce, go-redis,
  redigo, Kafka, RestTemplate, RestClient, WebClient, net/http, resty); the
  method decides read or write;
- a wrapper around one, called directly or through its interface;
- generated gRPC code: the resource is the full method name it sends
  (`cart.v1.Cart/GetCart`); a server registration serves every method of its
  descriptor;
- a type answered with a `client` question.

The source supplies only the arguments. An external type that looks like a
client but matches no rule is a `client` gap. Client APIs live in the
analyzer's versioned rule file: a new client is a rule, not code.

---

## 4. Rules for `call`

How each analyzer sets a call site's `call` (values: edges-spec §4).

**weave-java**

| Code around the call | `call` |
|---|---|
| no `catch` covers it, or the catch rethrows | `required` |
| the catch returns a value or a default; `@CircuitBreaker`/Feign fallback | `fallback` |
| the catch only logs, or is empty | `optional` |
| `@Async`, or a Kafka/Redis send not awaited | `async` |
| the catch calls a helper | `unknown` |

A `catch` covers the call per a fixed list (`Exception`, `RuntimeException`,
`RestClientException`, `FeignException`, `WebClientResponseException`,
`IOException`). An exception that reaches `@ControllerAdvice` still fails the
request: `required`.

**weave-go**

| Code after the call | `call` |
|---|---|
| `if err != nil` returns the error (wrapped or not), panics or `log.Fatal`s | `required` |
| `if err != nil { return <value>, nil }`; `errors.Is(err, X)` with a fallback (for `X` only) | `fallback` |
| `if err != nil` only logs; error ignored or `_` | `optional` |
| inside `go func(){…}()` | `async` |
| anything else | `unknown` |

---

## 5. weave-ai

The LLM side of a build: on when an endpoint is configured (any
OpenAI-compatible chat endpoint with tool calling), off otherwise. It uses only
weaver's tools (mcp-spec §2), never files. The edge mapper calls it for:

1. **Review.** Every edge, with the facts it rests on, is checked against the
   code.
   - **Agree:** written, `reviewedBy: llm`.
   - **Disagree:** written with `call: unknown` and a `review` question
     carrying the reason and evidence.
2. **Gaps.** It proposes answers to `resource`, `address`, `target`, `call`,
   `implementation`, `entries` and `client` gaps. An accepted answer is
   written to the override with `resolvedBy: llm`; a rejected one becomes a
   human question with the proposal.

**Validation.** A verdict or answer counts only if every evidence location was
returned by a tool in the same task, every literal in a value appears in that
evidence, and the value has the right form. Anything else counts as a
disagreement.

**Cost.** Verdicts are cached by edge, facts, the code read, model and prompt,
so an unchanged rebuild makes no LLM calls. Reviews are capped per build, in a
fixed order (most entry points first); edges over the cap are written
unreviewed.

---

## 6. Testing

1. **Analyzers:** fixture repos with expected facts, kept exact.
2. **Mapper:** hand-written facts in, expected map and questions out.
3. **Corpus:** real repos at pinned commits with expected edges; precision and
   recall per kind must not drop.
