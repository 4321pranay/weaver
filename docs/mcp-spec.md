# MCP Spec: Tools and Agents

Weaver's knowledge is exposed as **tools**: deterministic functions over
kb.db (SCIP), Zoekt, config and the map files. The tools are used by
weaver's weave-ai, in-process, and by falcon, over MCP.

---

## 1. Principles

- **The tools contain no LLM.** Every tool is deterministic: the same input,
  on the same build, gives the same output. Each answer carries `at`
  locations so it can be checked.
- **One implementation.** weave-ai calls the tools in-process,
  and falcon calls the same tools over MCP. No consumer re-implements
  weaver's logic, including the override merge, derived hops or the
  `called_by` walk.
- **Each project owns its own agent and prompts.** Weaver owns the
  knowledge-building agent and its prompts. Falcon owns incident analysis
  and its prompts.
- **The files stay the stored output.** `edges.yaml`, `edges.history/`,
  `releases.yaml` and the Zoekt shards remain the record. MCP reads them
  but does not replace them.

---

## 2. `weaver mcp`

`weaver mcp` serves these tools over MCP.

All locations are `<path>:<line>`, 1-based, relative to the service's repo.
Every answer carries the `build` it was read from, and one answer never mixes
two builds.
Symbols follow edges-spec §3.6.

**Domains**

| Tool | Input | Output |
|---|---|---|
| `domains` | — | `[{domain, build, run, builtAt, services: [serviceName]}]`: what falcon can map with `falcon init domain`, and the build id (weaver-architecture §2) each domain's tools currently answer from |

**Code (SCIP; `release` optional, default current)**

| Tool                 | Input                                                             | Output                                                                              |
| -------------------- | ----------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| `symbol_search`    | `domain`, `query`, `limit`                                  | `[{symbol, kind, at}]`                                                            |
| `definition`       | `domain`, `symbol`                                            | `{symbol, at, snippet}`                                                           |
| `references`       | `domain`, `symbol`                                            | `[{at, method}]`                                                                  |
| `implementations`  | `domain`, `symbol` (an interface or a method on one)          | `[{symbol, at}]`                                                                  |
| `enclosing_method` | `domain`, `service`, `at`                                   | `{symbol, at, range}`                                                             |
| `call_chain`       | `domain`, `symbol`, `direction: up\|down`, optional `depth` (default: unlimited) | a tree of `{symbol, at, children}`. Cut-off branches are marked `cut: <reason>` |

**Text (Zoekt)**

| Tool       | Input                                                                              | Output                    |
| ---------- | ---------------------------------------------------------------------------------- | ------------------------- |
| `search` | `domain`, `query` (Zoekt syntax), optional `service`, `release`, `limit` | `[{service, at, line}]` |
| `read`   | `domain`, `service`, `path`, `from`, `to`, optional `release`          | `{lines: [{n, text}]}`  |

**Values**

| Tool               | Input                                                     | Output                                                                                                                                   |
| ------------------ | --------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `constant_value` | `domain`, `symbol`, or `service` + `at` + `col` | `{value, at}`, or an error when the constant can't be resolved exactly                                                                 |
| `expression`     | `domain`, `service`, `at`, `col`                  | the argument expression, traced through local assignments in its method, constants resolved: `{parts: [{text, value?, at?}], pattern}` |
| `config`         | `domain`, `service`, `key` or `pattern`           | `[{key, value, source}]`. `source` is the file and line: `application*.yml`, `.properties`, `.env`, Compose, Helm values       |

**Map**

| Tool        | Input                                                                     | Output                                                            |
| ----------- | ------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| `nodes`   | `domain`, optional `type`                                             | nodes from the effective map                                      |
| `edges`   | `domain`, `node`, `direction: in\|out\|both`, optional `kind`       | edges, without `callers`                                        |
| `edge`    | `domain`, `ref` (`from → to kind resource`)                        | the full edge: `callers`, `handler`, `entries`              |
| `blast`   | `domain`, `node`, `all` (default `false`: `required` hops only) | `[{node, path}]`                                                |
| `entries` | `domain`, `service`                                                   | the service's entry points (edges-spec §3.5) and the edges each one reaches |

**Releases**

| Tool         | Input                                            | Output                                                                |
| ------------ | ------------------------------------------------ | --------------------------------------------------------------------- |
| `releases` | `domain`, `service`                          | `releases[]` from `releases.yaml`                             |
| `diff`     | `domain`, `service`, `a`, `b` (releases) | `{added, removed, changed}` between the two edge snapshots (build-spec §3) |

**weave-ai only (in-process, never over MCP)**

| Tool          | Input                                 | Output                                                               |
| ------------- | ------------------------------------- | -------------------------------------------------------------------- |
| `questions` | `domain`                            | open gaps (questions-spec), each with an `id`, a type and its context       |
| `propose`   | `id`, `value`, `evidence: [at]` | `accepted` or `rejected: <reason>`, validated as in analyzer-spec §5 |
| `review`    | `edge` (`from → to kind resource`), `verdict: agree\|disagree`, `reason`, `evidence: [at]` | `recorded`, or `rejected: <reason>` when the evidence doesn't hold (analyzer-spec §5) |

---

## 3. weave-ai

The build agent of weaver is `weave-ai` ([analyzer-spec.md](analyzer-spec.md)
§5): the edge mapper has it review every edge and work through the gaps,
using the tools of §2 in-process, plus `questions`, `propose` and `review`.
