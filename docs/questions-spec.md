# Questions Spec

Whatever a build can't resolve from code or config, and the override doesn't
answer, becomes a **question**. Weaver never guesses, and never hides a
dependency it is unsure of: the edges a question concerns are written to
`edges.yaml` with `call: unknown` (which counts as `required`) and
`question: <id>`. What the question is about is left open in the edge: an
unknown resource is `{}`, an unknown target or instance is the node
`unknown/<question-id>` (edges-spec §2.1). The answer fills it in on the next
build.

**Question types**

| Type | Raised when | Answer | What the answer writes |
|---|---|---|---|
| `address` | a service's Kafka / Redis / database instance has no config key, or its key doesn't resolve | a node id: `kafka/<name>`, `redis/<name>`, `database/<name>` | an override node with that id and the question's alias: `<type>@<service>`, or the unresolved config key (`${KAFKA_BOOTSTRAP_SERVERS}`) |
| `target` | a REST / gRPC call that no service serves, with no host to name an external node | a service name, or `external/<host>` | an override edge `from: <service>`, `to: <answer>` (plus the external node) |
| `resource` | a topic, key, URL or table with no part known (`<DYNAMIC_DISPATCH …>`) | the value, `{}` for runtime parts | nothing in the override: an override row can't set one call site's resource. The next build applies it from `questions.yaml` |
| `config` | spring-probe ([spring-probe-spec.md](spring-probe-spec.md)) can't settle a Spring service's production profile, a key a bean condition reads, or a value that ends up in a fact (topic, path, URL, cron). Address and secret keys are never asked: `address` covers instances | the value, or one of the listed options | nothing in the override. Each build passes the service's answered keys to spring-probe with its capture; spring-probe keeps no answers of its own |
| `client` | an external type the domain's services call that looks like a client of another system, but no rule classifies ([analyzer-spec.md](analyzer-spec.md) §3) | its kind: `rest`, `grpc`, `kafka`, `redis`, `db` or `s3`; `-` if it isn't a client | nothing in the override. The next build classifies the type's calls by it |
| `review` | weave-ai disagreed with an edge the facts give, or its evidence didn't hold ([analyzer-spec.md](analyzer-spec.md) §5). Until answered the edge is in the map with `call: unknown`; the question carries weave-ai's reason and evidence | `keep` (the edge is right) or `drop` (it isn't) | `keep`: the edge gets its inferred `call` back, marked `reviewedBy: human`. `drop`: an override row with `call: ignore` (edges-spec §4) removes it |
| `version` | a service has no version in its build file (build-spec §2.1) | the release version | nothing in the override; the next build records the release under it |
| `call` | a call site's criticality is `unknown` (edges-spec §4.1), e.g. the catch body calls a helper | a criticality of edges-spec §4 | a call-site row with that `call` (edges-spec §5.1) |
| `implementation` | an injected interface call has several implementations and the call graph can't pick one | an implementation symbol | nothing in the override; the next build's entries walk follows it |
| `entries` | the entries walk was cut off (edges-spec §3.5) | the entry points that reach the call | nothing in the override; the next build adds them to the call site's `entries` |
| `analyzer` | an analyzer step didn't run, e.g. a Maven build failed (analyzer-spec §1) | `-` once fixed, or a reason it can't be | nothing; the question closes when a build runs the step |

**`knowledge/<domain>/questions.yaml`** holds every question any build of the
domain raised. It is kept across builds, never replaced, and written only by
weaver:

```yaml
version: weaver/questions/v1.0
runs:                                   # one entry per build of the domain
  - {id: run-a82557c687e1, at: "2026-10-24T17:16:24Z", services: 50,
     build: 42, raised: 34, new: 34, pending: 52, resolved: 0, obsolete: 0}
questions:
  - id: q-f42ffdad                      # stable: hash of type + subject
    type: address
    ask: "courier: which kafka instance does it use? No config key found"
    subject: {nodeType: kafka, service: courier}
    expect: "a node id, e.g. kafka/<name>; saved as a node with alias kafka@courier"
    pending: 1                          # call sites waiting on it, written with call: unknown
    sites: [courier:producer_4.go:3]    # up to 10
    status: answered
    firstRun: run-a82557c687e1
    lastRun: run-a82557c687e1
    answer: {value: kafka/food, by: pranay, at: "2026-10-24T18:02:11Z"}
```

| Field | Meaning |
|---|---|
| `id` | Stable across runs: the same gap is the same question |
| `status` | `open` · `answered` (waiting for the next build) · `resolved` (answered; a later build no longer raised it) · `obsolete` (the gap went away unanswered: code changed) · `never` (answered `-`) |
| `previousAnswers` | Answers after which a later build still raised the question; it is then `open` again |

**Answering.** The only way to answer is `weaver answer`:

```bash
weaver answer <domain>                         # asks each open question in turn
weaver answer <domain> <id> <value>            # answers one
         [--by <name>]                         # default: git user.name, else $USER
```

- **Interactive:** each open question is shown with its call sites and the
  expected form, and the typed line is the answer.
  - Empty skips the question.
  - `-` means "can't be known": the question becomes `never`.
  - `q` stops.
- **Suggestions and completion:** for `address` and `target` questions, the
  existing nodes of the asked type (from `edges.yaml` and the override) are
  listed, and node ids complete with Tab. Picking an existing node adds the
  question's alias to it instead of creating a second node for the same
  instance.
- **New nodes:** answering with a node id that doesn't exist yet asks for its
  edges-spec §2.2 properties (brokers, host, engine). The id must be a logical name
  (`kafka/orders`), not a host or IP; hosts go in `props` and `aliases`.
- **Refused answers:** an answer of the wrong form, or one that would make the
  override invalid (edges-spec §6), is refused with the reason, and nothing is written.
- **Accepted answers:** the answer is recorded in `questions.yaml` with who and
  when, and its meaning is written to `edges.override.yaml`. The next build
  applies it, and the question becomes `resolved`.
- **No prompting during builds:** a build never prompts. It writes
  `questions.yaml` and prints the counts, so CI, cron and falcon builds work the
  same as builds on a laptop.
