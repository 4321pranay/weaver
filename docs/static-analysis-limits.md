# The Limits of Static Analysis

Java and Go are Turing-complete, general-purpose languages. By **Rice's
theorem**, no program can decide a non-trivial property of what *every*
program in such a language does at runtime. These languages also bind late by
design, so much of what a service talks to is decided after the code is
written. No static tool, weaver included, can be complete and exact for every
service. The table lists each limit and what weaver does about it.

| Limit | Example | What weaver does |
|---|---|---|
| Computed values | `base + "/" + tenant.region() + path` | Resolves what is known (constants via SCIP, config, concatenation); runtime parts become `{}`; nothing known → a question |
| Values built inside a wrapper or library | `noRedisDAO.generate(PREFIX)` builds the key internally | Resolve the sink's argument inside the wrapper with the call-site values; otherwise the wrapper's own pattern |
| Dependency injection | `@Autowired RedisTemplate`, beans chosen by type | SCIP resolves the field's real type, so injected clients are recognised |
| Interface with an injected implementation | `CacheService` → `RedisCacheService` | A call graph resolves the call to its real implementations: Go with `golang.org/x/tools/go/callgraph` (VTA), Java with SootUp (points-to analysis) plus a Spring bean model (`@Primary`, `@Qualifier`, component scan). Without one: SCIP `is_implementation`; otherwise `entries: unknown` |
| Config decided at deploy time | broker address in env, Helm, Compose or a secret | Reads `application*.yml`, `.properties`, `.env`, and the `environment:` of the Compose service built from the service's folder (Spring relaxed binding); anything else becomes an `address` question |
| Profiles and feature flags | `@Profile("prod")`, `if (flags.newPayments)` | The map describes production: values come from the production profile, and where profiles disagree and nothing settles it the mapper asks (edges-spec §2.2); flag-guarded calls stay in the map |
| Reflection, dynamic proxies, string-built class names | `Class.forName(name).newInstance()` | Not resolved; the call chain ends there and is marked `unknown` |
| Lambdas and callbacks passed elsewhere | `executor.submit(() -> client.call())` | The call is found. Who reaches it comes from the same call graph (VTA for Go, SootUp for Java), which follows functions passed as values; otherwise `entries: unknown` |
| Service discovery and runtime routing | URL from a registry, gateway routes, mesh rules | Matches by path to the service that serves it; a host nobody serves becomes `external/<host>`; no host → a `target` question |
| Code paths that never run | dead code, unused clients | Kept in the map: weaver can't tell; it shows what the code *can* do |
| Calls outside the code | sidecars, cron scripts, DB triggers, mesh retries | Not visible to weaver; added by hand in the override |
| Languages or builds without an indexer | .NET, a failed Gradle build | Falls back to method names; edges marked `match: name` |
| Libraries without source | a third-party client | Matched as a known client API by type; internal libraries are indexed at their exact version |
| Whether a failure matters | catch-and-log vs rethrow | `call` inferred per call site from the code around it; unsure → `unknown`, which counts as `required` |
| What actually runs, how often, how fast | traffic volume, latency, the deployed version | Out of scope: static analysis can't see it (below) |

Weaver never presents a guess as a fact. Where it isn't exact it says so: a
`{}` part, `unknown`, `match: name`, a question for a human
([questions-spec.md](questions-spec.md)), or an answer from weave-ai
marked `resolvedBy: llm`.

**Declaring instead of inferring.** The only way past these limits is to make
the facts explicit. Services declare their dependencies in machine-readable
form (OpenAPI for what they serve, AsyncAPI for topics, a dependency list with
criticality), and weaver checks the declarations against the code: calls that
aren't declared, and declarations the code never makes. Languages and
frameworks that restrict what code may call, such as IDL-generated clients
(protobuf/gRPC), Ballerina, Dapr or workflow engines, reach the same result by
giving up expressiveness.

**Real-time analysis covers the rest.** What static analysis can't see, tools
that watch running systems can, but only for traffic that actually happens:

| Kind | Examples | Sees | Misses |
|---|---|---|---|
| Distributed tracing | OpenTelemetry with Jaeger, Grafana Tempo, Zipkin | every call a request makes, with timing | calls not instrumented, paths not exercised |
| Service-mesh telemetry | Istio, Linkerd | service-to-service calls and errors, without code changes | calls that bypass the mesh (brokers, databases) |
| eBPF service maps | Grafana Beyla, Cilium Hubble, Pixie | network-level calls from the kernel, no instrumentation | why a call is made, which code makes it |
| APM | Datadog, Dynatrace, New Relic, Elastic APM | traces, dependency maps, errors per endpoint | rare paths, code not yet deployed |
| Metrics | Prometheus, kube-state-metrics, Kafka and DB exporters | rates, errors, saturation, versions, broker and pool state | individual calls and their code |

Static and runtime views complement each other: weaver shows what the code
*can* do, including rare paths and code not yet deployed; runtime tools show
what it *does*. A call seen at runtime but not in the map, or in the map but
never seen at runtime, is worth a look. Comparing the two is a consumer's job,
not weaver's.
