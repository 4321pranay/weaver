# Weaver Features

What weaver does, with the section that defines each feature. How the parts
fit together is in [weaver-architecture.md](weaver-architecture.md).

## 1. Sources and build

| Feature | Spec |
|---|---|
| One entry per service in `sources.yaml`; a monorepo lists each service with its `root`, cloned and indexed once | weaver-architecture §2 |
| Per-language analyzers (`weave-go`, `weave-java`) report facts; one edge mapper builds the map from them | analyzer-spec §1–2 |
| Calls recognised by what SCIP resolves (client APIs, wrappers, generated gRPC code), never by method names alone | analyzer-spec §3 |
| Spring wiring from spring-probe: injected beans, framework entry points, proxies, resolved config | spring-probe-spec |
| Internal libraries indexed at the exact version each service uses | build-spec §5 |
| Every build is one generation with a build id, switched atomically; all generations kept | weaver-architecture §2 |

## 2. Service map

| Feature | Spec |
|---|---|
| Nodes for services and infrastructure (Kafka, Redis, databases, object stores, external hosts), named logically with aliases | edges-spec §2 |
| Edges with kind, resource and operation; derived service-to-service hops through brokers | edges-spec §3 |
| Criticality per call site (`required`, `optional`, `fallback`, `async`, `unknown`) | edges-spec §4, analyzer-spec §4 |
| Code references on every edge: call sites, their methods, callers and the entry points that reach them | edges-spec §3.4–3.5 |
| The map describes production; where config differs by environment and nothing settles it, weaver asks | edges-spec §2.2 |
| Nothing hidden: an unsure edge is still in the map, `call: unknown`, pointing at its question | questions-spec |

## 3. Corrections and questions

| Feature | Spec |
|---|---|
| `edges.override.yaml` patches, adds or ignores nodes, edges and single call sites; bad rows are skipped, not fatal | edges-spec §5–6 |
| Questions for whatever code and config can't settle, kept across builds with stable ids | questions-spec |
| `weaver answer` asks them, suggests existing nodes, and records who answered and when | questions-spec |
| weave-ai, with an LLM configured, reviews every edge against the code and proposes answers, with evidence | analyzer-spec §5 |

## 4. Releases and code

| Feature | Spec |
|---|---|
| Every release per service, named by its build-file version | build-spec §2 |
| An edge snapshot per release that changed edges, for diffs between releases | build-spec §3 |
| Code text (Zoekt) and SCIP data for every release, stored once per changed file | build-spec §4 |

## 5. Access

| Feature | Spec |
|---|---|
| `weaver mcp`: deterministic, read-only tools over the map, code, releases and questions | mcp-spec §2 |
| Every answer names the build it came from | mcp-spec §2 |
| `graph.html`: the map as an interactive page | weaver-architecture §2 |
