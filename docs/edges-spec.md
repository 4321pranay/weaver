# Service Map Spec

This spec defines how weaver describes one domain as a graph. Services and the
infrastructure they use are the **nodes**. Each call between them is an
**edge**, and each edge records whether a failure breaks the caller. Weaver
generates the map on every build, and a human can correct or extend it in an
override file.

---

## 1. Files

| File                    | Written by                             | Location                | Edited by hand                                                  |
| ----------------------- | -------------------------------------- | ----------------------- | --------------------------------------------------------------- |
| `sources.yaml`        | human                                  | `sources/<domain>/`   | yes                                                             |
| `edges.yaml`          | `weaver build` (every build)         | `knowledge/<domain>/` | **never**                                                 |
| `edges.override.yaml` | human (weaver creates a template once) | `knowledge/<domain>/` | yes                                                             |
| `kb.db`               | `weaver build`                       | `knowledge/<domain>/` | no. Holds SCIP data only, no edges                              |
| `releases.yaml`   | `weaver build` (every build)         | `knowledge/<domain>/` | **never**. What was built, and the release history (build-spec §2) |
| `questions.yaml`      | `weaver build`, `weaver answer`      | `knowledge/<domain>/` | **never**. Open and answered questions, and the run log (questions-spec) |

The **effective map** is `edges.yaml` merged with `edges.override.yaml`
(see §5). Every consumer, `weaver query` included, reads the effective map. No
consumer reads either file on its own. Each build copies the override it used
into its generation (weaver-architecture §2), and the effective map is
merged from that generation's two files: an override edit takes effect with
the next build, and rolling a generation back rolls its override back too.

---

## 2. Nodes

A node is one **instance**: a service, a database, a Kafka cluster, a Redis
instance, a Ceph bucket or an external system. Things that live *inside* an
instance (tables, topics, keys, folders, endpoints) are not nodes. They are the
`resource` of an edge (§3).

### 2.1 Fields

| Field       | Required | Description                                                                       |
| ----------- | -------- | --------------------------------------------------------------------------------- |
| `id`      | yes      | `<type>/<name>` for infrastructure, the bare serviceName for services (§2.3)   |
| `type`    | yes      | `service` · `external` · `kafka` · `redis` · `ceph` · `database` · `unknown` (an end an open question is about: id `unknown/<question-id>`, props `{kind, question}`) |
| `props`   | no       | Type-specific properties (§2.2). Unknown keys are kept and passed through        |
| `aliases` | no       | Other addresses of the same instance: hosts, IPs, URLs, config keys               |

### 2.2 Properties per type

| type         | props                              | Example                                                             |
| ------------ | ---------------------------------- | ------------------------------------------------------------------- |
| `service`  | `domain`                         | `{domain: payments}`                                              |
| `external` | `host`                           | `{host: api.stripe.com}`                                          |
| `kafka`    | `brokers` (list)                 | `{brokers: [kafka-1.prod:9092, kafka-2.prod:9092]}`               |
| `redis`    | `host`, `db`                   | `{host: cache.prod:6379, db: 0}`                                  |
| `ceph`     | `endpoint`, `bucket`           | `{endpoint: s3.ceph.prod, bucket: invoices}`                      |
| `database` | `engine`, `database`, `host` | `{engine: postgres, database: orders, host: orders-db.prod:5432}` |
| `unknown`  | `kind`, `question`               | `{kind: kafka, question: q-f42ffdad}`: an end an open question is about; replaced by the answer |

**Environments.** The map describes production. The analyzers report every
value a config key has, with the profile or file it comes from
(`application-dev.yml`, `application-prod.yml`, Compose; analyzer-spec §2).
When the values that decide a node or an edge (an instance's address, a
resource read from config) differ between candidates, and neither the
answered profile (a `config` question on `spring.profiles.active`) nor the
override settles it, the mapper doesn't pick one. The edge goes to
`unknown/<question-id>`, and an `address` question offers the candidates as
its options, the way spring-probe offers a condition's paths.

Weaver never guesses a property. When one can't be resolved from code or
config, the build raises a **question** (questions-spec). It is answered with
`weaver answer`, which saves the answer to `edges.override.yaml`. A property
stays empty only while its question is open, or after it is answered `-`.

Property values are production's: the profile answered for the service
(`spring.profiles.active`, a `config` question), whose answer settles every
key that profile sets; with no answer and only one candidate value, that
value. A node is the same node in every environment, but its addresses
differ between environments; the map records production's, and the
Environments rule above covers the rest.

### 2.3 Identity and naming

`id` is a **logical name**. A hostname is never the identity. Services use
their `serviceName` from `sources.yaml`. Infrastructure uses
`<type>/<name>`, where the name comes from:

| type         | name                                                        |
| ------------ | ----------------------------------------------------------- |
| `database` | `database` prop                                           |
| `kafka`    | the cluster's name, asked once (rule 4 below); every broker's `host:port` is an alias |
| `redis`    | `host`, port stripped                                     |
| `ceph`     | `bucket`                                                  |
| `external` | `host`                                                    |

There is no fallback name. If weaver cannot resolve the name from code or
config, it **asks** (questions-spec), and the answer is saved as the node's `id` in
`edges.override.yaml`.

Rules:

1. **Same address, same node.** All addresses are normalised the same way:
   lower-cased, scheme stripped, default port added, broker lists sorted.
   Two references whose normalised address matches a node's `id`, a prop or
   an `aliases` entry resolve to that node.
2. **Same config key, one question.** Weaver asks once per unresolved config
   key. All services that read the same key (`${KAFKA_BOOTSTRAP_SERVERS}`)
   then get the node named by that one answer, and the key is stored as an
   alias of the node.
3. **Name collisions.** If two different instances would get the same name,
   for example database `orders` on two hosts, each id gets its host appended:
   `database/orders@db-a`, `database/orders@db-b`.
4. **Kafka clusters.** A Kafka node's id names the cluster, not its brokers:
   `kafka/orders`. Every broker's `host:port` is an alias. The first time a
   broker set shares no broker with a known node, weaver asks its name once
   (an `address` question); after that, a service configured with some of the
   brokers, or a cluster that gains or loses brokers, still shares a broker
   with the node and resolves to it, so the id never changes.
5. **External.** A REST or gRPC call to a host that is not a known service in
   any domain becomes an `external` node.

---

## 3. Edges

An edge is a dependency of `from` on `to`, through `kind`, on `resource`.

### 3.1 Fields

| Field          | Required             | Description                                                                                                                                |
| -------------- | -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `from`       | yes¹                | node id                                                                                                                                    |
| `to`         | yes¹                | node id                                                                                                                                    |
| `kind`       | yes                  | protocol: `rest` · `grpc` · `kafka` · `redis` · `db` · `s3`                                                               |
| `resource`   | yes                  | what inside `to` is used (§3.2)                                                                                                         |
| `op`         | infrastructure edges | db, s3, Redis keys: `read` · `write` · `readwrite`. Kafka: `produce` · `consume`. Redis pub/sub: `publish` · `subscribe` |
| `call`       | no                   | criticality (§4). Default `required`                                                                                                    |
| `match`      | generated only       | `name`: the calls were recognised by method name only, because the repo has no SCIP index (§3.7). Absent when SCIP resolved them       |
| `resolvedBy` | generated only       | `llm`: a value on this edge came from weave-ai (analyzer-spec §5). Absent when extracted. A human confirms it in the override          |
| `reviewedBy` | generated only       | `llm`: weave-ai checked the edge against the code and agreed (analyzer-spec §5). `human`: weave-ai disagreed and a person kept it with `weaver answer`. Absent when no LLM is configured, or the edge was over the review cap |
| `question`   | generated only       | the id of the open question this edge waits on (questions-spec). Its `call` is `unknown` until it is answered; the resource is `{}` or the target `unknown/<id>` when that is what's asked |
| `handler`    | generated only       | the code in `to` that serves `resource` (§3.4)                                                                                        |
| `callers`    | generated only       | call sites in `from`: `at`, `call`, `method`, `called_by` (§3.4)                                                                |

¹ An edge must have both ends. If weaver finds a produce with no matching
consume, it writes the edge to the infrastructure node, and the
infrastructure-to-consumer edge is simply absent. Nothing is left dangling.

**Direction:**

- **Services:** `from` is the caller, `to` is the callee.
- **Kafka:** the producer edge is `service → kafka/<x>` with `op: produce`,
  and the consumer edge is `kafka/<x> → service` with `op: consume`.
- **Redis pub/sub:** the publish edge is `service → redis/<x>` with
  `op: publish`, and the subscribe edge is `redis/<x> → service` with
  `op: subscribe`.
- **db, s3 and Redis keys:** the edge always goes service → store, with
  `op: read`, `write` or `readwrite`.

Every edge that touches an infrastructure node has an `op`, and the `op` must
match the direction:

- `consume` and `subscribe` are only allowed on `infra → service` edges.
- Every other `op` is only allowed on `service → infra` edges.

A mismatch fails validation (§6). Edges between two services have no `op`.

**Code ends:**

- On a `service → *` edge, `callers` holds the caller's code.
- On a `* → service` edge (a service handler, or a Kafka or Redis consumer),
  `handler` holds the receiver's code.
- An edge from one service to another has both.

**`kind` and `type` are separate.** The protocol does not always match the
node type: Ceph is reached with `kind: s3`, Postgres and MySQL are both
`kind: db` (`engine` tells them apart), and a managed Kafka may be reached over
`rest`.

**Identity:** `(from, to, kind, resource)`.

### 3.2 Resource

`resource` is the one term for what an edge uses inside its `to` node: an
endpoint of a service, a topic of a cluster, a table of a database, a key of a
Redis instance, a prefix of a bucket. Nodes never list their resources. A
node's resources are exactly the `resource` values of the edges pointing at
it.

**Reference:** any resource can be written as `<node id>#<resource>`, for
example `payment#POST /pay`, `kafka/orders#orders.created` or
`database/orders#orders`. Logs, `weaver query` output and falcon all use this
form.

| kind      | resource                        | Example                         |
| --------- | ------------------------------- | ------------------------------- |
| `rest`  | `METHOD /path`                | `POST /orders/{}`             |
| `grpc`  | `package.Service/Method`      | `payments.v1.Payments/Charge` |
| `kafka` | topic                           | `orders.created`              |
| `redis` | key pattern, or pub/sub channel | `cart:{}`, `order.events`   |
| `db`    | table                           | `orders`                      |
| `s3`    | key prefix                      | `invoices/{}`                 |

**Normalisation** happens before any match or merge:

- Scheme and host are stripped from REST URLs. The host only decides `to`.
- Path parameters become `{}`: `/users/{id}`, `/users/:id` and `/users/123`
  are all `/users/{}`.
- The REST method is upper-cased. A missing method matches any method.
- In redis keys and s3 prefixes, segments that are dynamic (variables,
  formatting or concatenation) become `{}`.

### 3.3 Derived service-to-service hops

Views that show services only (`flow`, `context`, `blast`) join edges through
infrastructure on **node and resource**:

    checkout → kafka/orders  (orders.created)
    kafka/orders → shipping  (orders.created)
    ⇒ checkout ⇒ shipping via kafka/orders orders.created

The node takes part in the join, so a topic named `events` on two different
clusters never joins.

### 3.4 Code references (`handler`, `callers`)

These fields are generated only and come from SCIP. They tie each edge to the
exact code on both sides, so falcon can go from an edge to the code without
searching.

A **code ref** is `{symbol, at}`:

- `symbol` is the SCIP symbol with its version removed (§3.6).
- `at` is `<path>:<line>`, relative to the root of the service's repo.

| Field                   | Meaning                                                                                                                                                                                                                                                    |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `handler`             | Edge-level. Where `to` serves the resource: the REST handler or gRPC method implementation for a service target. For a `kafka/<x> → service` edge, the consumer method. Absent when `to` is infrastructure or external, or the handler wasn't found |
| `callers[].at`        | The call expression in `from`                                                                                                                                                                                                                            |
| `callers[].method`    | The method that encloses the call: its symbol and its definition                                                                                                                                                                                           |
| `callers[].called_by` | Every reference to the `method`, one level up, across all repos of the domain. A ref with no enclosing method (field initialiser, lambda) gives only `at`                                                                                              |
| `callers[].entries`   | Every entry point of `from` whose code reaches this call (§3.5). It is `unknown` when the walk was cut off, and an empty list means no entry point reaches the call                                                                                   |

**Limits:**

- `called_by` is one level deep, and `entries` holds only the two ends of the
  chain. Use `weaver query refs <symbol>` for the steps in between.
- Arbitrary symbols that have nothing to do with an edge stay in kb.db only.
- If SCIP data is missing for a service (the indexer failed, or the language
  has no indexer), `symbol`, `method` and `called_by` are left out and only `at` is
  kept, taken from tree-sitter.

### 3.5 Entry points

An **entry point** is code that starts work in a service, and is what metrics
and alerts are reported for:

| Entry `kind` | Detected from                                                                                   | `resource`                    |
| -------------- | ----------------------------------------------------------------------------------------------- | ------------------------------- |
| `rest`       | `@GetMapping`/`@PostMapping`/…, `@RequestMapping`, Go `HandleFunc`/router registration | `METHOD /path` (§3.2)        |
| `grpc`       | Implementation of a generated service method                                                    | `package.Service/Method`      |
| `kafka`      | `@KafkaListener`, a Go consumer loop handler                                                  | topic                           |
| `redis`      | `MessageListener`, a Go `Subscribe` handler                                                 | pub/sub channel                 |
| `schedule`   | `@Scheduled`, a cron registration                                                             | cron expression, or method name |
| `main`       | `main()` / `@SpringBootApplication` startup code                                            | `main`                        |

An entry is written `{kind, resource, symbol, at}`. Its `kind` and `resource`
are the same as on the `* → service` edge that ends at it, so an entry joins
directly to that edge's `handler`.

**Walk.** For each caller, weaver starts from `method` and follows SCIP
references upward through the methods that enclose them. Every entry point
the walk reaches is added to `entries`.

- **Scope:** the walk stays within `from`'s repo and the libraries that repo
  calls. It never crosses an edge into another service, because that hop is
  its own edge.
- **Cycles:** a method already visited is skipped.
- **Shared work:** a method's entry points are computed once per build and
  reused by every call site whose walk reaches it.
- **Depth:** unlimited. The walk follows every caller until it reaches entry
  points or runs out of callers. The visited set guarantees it ends: each
  method is visited at most once.
- **Cut-off:** the caller gets `entries: unknown` if a step can't be
  resolved. Unresolvable steps are a call through an
  interface whose implementation is injected, a lambda or method reference
  passed elsewhere, or reflection. An `unknown` walk is part of the review
  queue (questions-spec).
- **Without SCIP:** `entries` is left out entirely.

**Use for RCA:**

- **From a metric to dependencies.** `POST /orders` is degraded. Find every
  edge whose `callers[].entries` contains `rest POST /orders`, keeping those
  whose call is `required`.
- **From a failure to impact.** Payment `POST /pay` is failing. Its edges'
  `callers[].entries` list the endpoints and jobs in the callers that are
  affected.

### 3.6 Symbols and libraries

A SCIP symbol is `<scheme> <manager> <package> <version> <descriptors>`, for
example:

    scip-java maven maven/com.acme/acme-kafka 1.3.0 com/acme/kafka/AcmeKafka#publish().

How a reference is matched to its definition depends on whose code it is:

| Code | Example | Matching |
|---|---|---|
| A service's own code | `maven/com.shop/checkout` | exact, within the service's index |
| **Internal library**: the organisation's own code, used as a dependency | `maven/com.acme/acme-kafka 1.3.0` | **exact, version included.** The library is cloned at that version and indexed (build-spec §5) |
| **External library**: third-party code | `maven/org.springframework.data/spring-data-redis 2.7.18` | **without the version**: only package and descriptors are compared |

External libraries are only ever matched as client APIs (§3.7) and are
never indexed, so their version does not matter. For an internal library the
version does matter: two services can depend on different versions whose
wrappers build keys differently.

**In `edges.yaml`**, `symbol` is written without scheme, manager and version
(`maven/com.acme/acme-kafka . com/acme/kafka/AcmeKafka#publish().`), so it
reads the same in every release. A code reference to library code also carries
`lib`, the library and the version where that code lives:

```yaml
method: {symbol: "maven/com.acme/acme-kafka . …/AcmeKafka#publish().", lib: acme-kafka@1.3.0, at: src/main/java/com/acme/kafka/AcmeKafka.java:41}
```

`at` is then relative to the library's repo at that version.

### 3.7 Call detection

Which code is a call, and which argument is its resource, is decided by the
language analyzers from SCIP, never by method names alone
([analyzer-spec.md](analyzer-spec.md) §3). A repo without a SCIP index falls
back to method names, and its edges carry `match: name`.

---

## 4. Call criticality

`call` says whether a failure of this call breaks the caller's flow.

| `call`     | Meaning                                                                                | Does the failure reach the caller? |
| ------------ | -------------------------------------------------------------------------------------- | ---------------------------------- |
| `required` | The error is thrown or returned to the caller                                          | yes                                |
| `fallback` | The error is replaced by an alternative result (default value, cache, fallback method) | no, the result is degraded         |
| `optional` | The error is caught and logged, and the flow continues unchanged                       | no                                 |
| `async`    | The caller does not wait for the outcome                                               | no                                 |
| `unknown`  | Weaver could not tell (generated only)                                                 | treated as `required`            |
| `ignore`   | Not a dependency, for example a health check (override only). Removes the edge         | —                                 |

- **Per call site:** each call site gets its own value.
- **Per edge:** derived from its call sites.
  - If any call site is `required` or `unknown`, the edge is `required`.
  - Otherwise the edge takes the first value that appears in this order:
    `fallback`, `optional`, `async`.

### 4.1 Inference

Each call site's `call` is inferred from the code around it, up to the nearest
enclosing handler, by its language's analyzer (rules: [analyzer-spec.md](analyzer-spec.md) §4).
`optional` is never inferred without evidence: a wrong `optional` hides a real
dependency during an incident, so an analyzer that is unsure writes `unknown`.

---

## 5. Override and merge

### 5.1 Override file

`edges.override.yaml` uses the schema of §2–3. Every field is optional except
the identity:

- **Node row:** `id` is required. `type`, `props` and `aliases` are optional.
- **Edge row:** `from`, `to`, `kind` and `resource` are required. `op` and
  `call` are optional. `handler` and `callers` are not allowed.
- **Call-site row:** an edge row with `method` changes only the call sites
  inside that method, not the whole edge. `method` is the end of the
  enclosing method's symbol, e.g. `RefundService#refund()`, so the row keeps
  matching when lines move. For a repo without SCIP, where call sites have no
  `method`, use `at: <path>` (the file) instead. A call-site row may set only
  `call`; `op` belongs to the edge.

```yaml
version: weaver/edges.override/v1.0
nodes:
  - id: kafka/orders                     # answer to "kafka via ${KAFKA_BOOTSTRAP_SERVERS}: which cluster?"
    type: kafka
    aliases: ["${KAFKA_BOOTSTRAP_SERVERS}"]
    props: {brokers: [kafka-1.prod:9092, kafka-2.prod:9092]}
  - id: external/stripe
    type: external
    props: {host: api.stripe.com}

edges:
  - {from: checkout, to: payment,         kind: rest, resource: POST /pay,        call: optional}
  - {from: checkout, to: payment,         kind: rest, resource: POST /pay,        method: "RefundService#refund()", call: optional}
  - {from: checkout, to: external/stripe, kind: rest, resource: POST /v1/charges}
  - {from: checkout, to: legacy,          kind: rest, resource: GET /ping,        call: ignore}
```

### 5.2 Merge rules

Weaver applies these steps in order to the generated map and the override
copied into the same generation.

**Nodes**

1. An override row whose `id` or one of its `aliases` matches a generated
   node's `id` or alias **merges into** it:
   - The resulting node takes the override's `id`.
   - The override's `props` are merged in key by key, and the override wins
     on conflicts.
   - The two `aliases` lists are combined.
   - Every edge pointing to an absorbed id is redirected to the resulting id.
2. An override row that matches nothing **adds** the node.

**Edges** (matched after normalisation, §3.2)

1. **Match found:** the override's `call` and `op` replace the generated
   values on the edge and on every one of its `callers`. `call: ignore` removes
   the edge.
2. **No match:** the edge is **added** without `callers`.
3. **Call-site rows** (`method` or `at`) are applied after the edge rows, and
   only to the `callers` whose `method.symbol` ends with `method` (or whose
   `at` is in that file):
   - `call` replaces the call site's `call`; `call: ignore` removes the call
     site, and the edge too if no call site is left.
   - The edge's `call` is then derived again from its call sites (§4).
   - A row that matches no call site is a warning (§6); unlike an edge row, it
     never adds anything.
   - When both an edge row and a call-site row match, the call-site row wins
     for its call sites.

The override always wins. The effective map does not record which file a value
came from, because that is visible by comparing the two files.

---

## 6. Validation

An invalid row in `edges.override.yaml` never fails the build. The row is
**skipped** and reported with its `file:line` (in the build log and as a
warning in `questions.yaml`'s run), and the rest of the override still
applies. A row is invalid when:

- it sets `call: unknown`, contains `handler` or `callers`, or adds or points
  at a node of type `unknown` (only an answer replaces one);
- its edge's `from` is not a node, neither generated nor added in the
  override;
- it is a call-site row with both `method` and `at`, or one that sets `op`;
- it repeats an identity with conflicting values, or adds an alias that
  already belongs to another node.

The build **fails**, reporting `file:line`, only when:

- a file can't be read as its schema at all (not YAML, an unknown major
  version, an unknown top-level field);
- generated output would be invalid: an edge whose `op` is missing, not valid
  for its `kind`, or against its direction (§3.1); an unknown `type`, `kind`,
  `op` or `call`; a required field missing; one alias resolving to two nodes.

The build **warns** but continues when:

- An override edge that sets only `call` or `op` matches no generated edge.
  The edge is still added, but this usually means a typo or code that was
  removed.
- An override node matches nothing and no edge uses it.
- A call-site row (`method` / `at`) matches no call site of its edge.
- A generated call site is `unknown`. These are listed as the review queue.

