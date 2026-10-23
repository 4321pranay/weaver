# weaver

AI code knowledge base which considers code as the source of truth.

Answers "how does X work" and "how does a request flow across services" - grounded in compiler-accurate code intelligence.

weaver builds all the **static** knowledge into one **knowledge folder** (`KNOWLEDGE_DIR`, default `./knowledge`), one folder
per domain, from the service clones in **sources** (`SOURCES_DIR`, default `./sources`):

```
sources/<domain>/sources.yaml  input: the domain's services (name, git url, branch)
sources/<domain>/<service>/   input: clone of each service at its branch

knowledge/<domain>/           output:
  kb.db                SCIP symbols, service edges, `domains` table
  versions.jsonl       graph versions, to diff against live traffic (drift)
  zoekt/               code search, scoped by the graph:
                       repo:^(checkout|payment)$ retry · meta.domain:^payments$ timeout
  manifest.json        service -> git url, branch, commit
  edges.override.yaml  runtime-only edges you add by hand (template on first build)
  graph.html           service map: open in a browser; compare any two versions
  scip/                per-repo SCIP indexes (intermediate)
```

Domains are stored apart but resolved together: a call from a `payments` service
to an `ordering` service appears in both domains' `versions.jsonl`, and queries
(`def`, `flow`, `blast`) read every domain's `kb.db` at once.

## Why is this built ?

Two layers, because no single OSS tool covers both:

1. **Cross-repo code graph** — SCIP indexers (`scip-java`, `scip-go`, …) produce
   compiler-accurate, globally-unique symbol IDs. weaver reads each `index.scip`
   with the official SCIP bindings into SQLite — every occurrence with its exact
   line and column. Cross-repo go-to-definition / find-references is then a SQL
   lookup on the symbol string, and constant references resolve to the exact
   constant SCIP says they point at — this is
   Sourcegraph's "cross-repo stitching",
2. **Service-edge index** — SCIP only sees *compiled* symbol links. Your
   REST/gRPC/Kafka/Redis/Database calls are strings (URLs, topics, channels) with no
   symbol, so SCIP is blind to them. The tree-sitter extractor
   (`internal/extract`) pulls them into a `service_edges` table. AI Model matches a producer edge to a consumer edge on the
   same channel to trace a hop.

## Setup

**1. Build weaver and install the SCIP tools on the build agent** (all OSS):

```bash
go build -o ./bin/weaver ./cmd/weaver
GOBIN=/usr/local/bin go install github.com/scip-code/scip-go/cmd/scip-go@latest   # Go indexer
# scip-java (needs JDK 17+): cs install --contrib scip-java   (see scip-code/scip-java)
```

**2. Build the knowledge.** Each domain lists its services in
`sources/<domain>/sources.yaml`; the folder names the domain:

```yaml
# sources/payments/sources.yaml
services:
  - serviceName: payment-gateway
    gitUrl: https://bitbucket.internal/scm/pay/payment-gateway.git
    branch: release/2.4
```

```bash
export SOURCES_DIR=/srv/weaver/sources KNOWLEDGE_DIR=/srv/weaver/knowledge
weaver build                  # every domain with a sources.yaml (prompts for git user/password)
weaver build payments         # just one domain; links to other domains come from their existing knowledge
weaver build --no-fetch       # rebuild from the existing clones, no git
weaver build --reindex        # SCIP-index every service again (default: reuse unchanged ones)
```

Each service is cloned at its `branch` (required) into `sources/<domain>/<service>`;
one whose branch doesn't exist is skipped and reported. SCIP indexing — the slow
step, minutes per repo — is skipped for a service whose commit, working tree and
build roots are unchanged since the last build; a root that failed at the same
commit isn't retried until `--reindex`. Credentials reach git only
through `GIT_ASKPASS` — never in a URL, `.git/config` or the process list; set
`WEAVER_GIT_USER` / `WEAVER_GIT_PASS` to skip the prompt (e.g. from cron). One build
runs at a time per knowledge folder. Runtime-only edges go in
`knowledge/<domain>/edges.override.yaml` (a commented template is created on the
first build; your edits are kept). Falcon reads a domain's search index with the
Zoekt Go library (or `zoekt-webserver -index knowledge/<domain>/zoekt`).

**3. Inspect** the built knowledge (`weaver query help`):

```bash
export KNOWLEDGE_DIR=/srv/weaver/knowledge
weaver use domain payments        # every query below reads only this domain (--all: every domain)
weaver query def validateToken          # where defined, any repo
weaver query refs validateToken         # who uses it, any repo
weaver query flow payment               # payment's raw service edges
weaver query blast payment both 3       # what breaks / what it depends on, by hop
weaver query context "why is checkout slow?"   # system prompt grounded in the graph
weaver query graph --infra        # service map JSON
weaver query versions            # graph versions, one per build; then: weaver query diff <v1> <v2>
weaver query search 'repo:^payment$ retry'   # the Zoekt index, falcon's view
```

## Test

```bash
go test ./...    # unit + end-to-end; the benchmark and SCIP probe tests need
                 # benchmarks/ (they skip otherwise)
go test -v ./internal/bench           # benchmark scores; a miss prints the full report
```

## Limits

- **Service edges are heuristic.** The extractor sees literal and statically
  resolvable topics/URLs/channels.
  Runtime-computed ones (`send(topicVar)`, config-driven routes) are invisible —
  list them in the domain's `edges.override.yaml`. This is the calibration knob; expect to
  tune it. Ground truth would need distributed tracing (OTel), not static
  analysis — add that only if the override file becomes unmanageable.
- **SCIP needs a build.** `scip-java` compiles; `scip-go` needs a buildable
  module (`go.mod` at the repo root). A repo that doesn't build gets no code
  graph (edges still extracted).
- **Constants resolve exactly where SCIP indexed.** A constant reference is
  matched to the SCIP symbol at its exact line/column and read at that symbol's
  definition. Only for repos SCIP couldn't index (e.g. .NET, a build that won't
  run) does weaver fall back to names: real constants only, test code skipped,
  the service's own first, and a name with different values left unresolved.

## Pending

Known gaps, roughly in priority order.

**Accuracy**
- **Measure on real code.** All scores so far come from synthetic benchmarks
  (`benchmarks/`). The next step is hand-labelled ground truth for a few real
  services, to get real precision/recall.
- **Unresolved `Class.CONSTANT` references are still guessed.** When neither
  SCIP nor the name-based fallback resolves one, a naming-convention rule turns
  it into `constant` (lowercased, `_` → `.`), which can be wrong. It should be
  marked unresolved instead.
- **Repos SCIP can't index.** No indexer for .NET (`scip-dotnet` could be added);
  projects whose build needs an older JDK than `scip-java` runs on need a
  per-repo JDK setting.
- **Non-ASCII columns.** SCIP and tree-sitter may count columns differently on
  lines with non-ASCII text, so exact constant matching can miss there.

**Correctness**
- **Version ids can collide.** Graph versions are numbered by build time in
  seconds; two builds of a domain in the same second share one. Number them
  previous + 1 instead.
- **`query flow <service>` over-matches.** It also returns edges whose channel
  merely contains the service name.
- **A build passes even when most repos fail SCIP.** It should fail above a
  threshold of failed repos.

**Scale and operations**
- **Resolved hops aren't stored.** Every query re-reads and re-matches all
  edges; storing hops at build time keeps queries fast at thousands of repos.
- **Air-gapped builds need dependency mirrors.** The SCIP indexers build each
  project and download its dependencies, so an offline build host needs a Go
  module proxy and a Maven/Gradle mirror.

**Ideas, not started**
- An interactive `weaver` prompt with built-in completion (works on Windows,
  needs no shell setup) instead of the zsh script.
- Per-repo summaries (purpose, entry points) generated at build time, to help a
  model pick which repo to look at.

## Credits

Ideas borrowed (re-implemented from their docs; no code copied, repos not vendored):

- [software-architecture-reconstruction-tool](https://github.com/katyadam/software-architecture-reconstruction-tool)
  — the static-resolution passes in `internal/extract`: cross-file constant
  resolution, format-string / URI templating (`Sprintf`, `String.format`), and
  config binding (Spring properties → channel values).
- [SAVAT](https://github.com/katyadam/SAVAT) — change-impact ideas: per-service
  content hashing in `internal/snapshot` (SAVAT hashes bytecode per method; we hash
  source per service) so a diff flags services that changed internally while
  their edges stayed the same, and the literal-variable data-flow pre-pass in
  `internal/extract`.
