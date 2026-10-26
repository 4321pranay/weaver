# Drift Spec

**Drift** is a difference between what weaver knows from the code and what
is actually running. `falcon drift-detect domain <name>` identifies drift for one
domain, reports it, and never corrects it.

Falcon identifies drift **for incident analysis**, not as an analysis of its
own:
- **Evidence for an incident:** a workload the mapping doesn't know, a call
  weaver doesn't know about, a broker address that changed.
- **A guard on the analysis:** where the code map and reality differ, the
  investigation knows not to trust that part of weaver's knowledge.
- **A list of fixes** to weaver's knowledge (§6), so the next incident starts
  from a correct map.

---

## 1. Principles

1. **Deterministic detection.** Every check is a rule over data. No LLM
   decides whether drift exists. An LLM may later *explain* a finding (§8),
   but it never creates or closes one.
2. **Report, never correct.** Each finding names the file a human should
   change (§6). Falcon writes only its own report.
3. **Not evaluated is not OK.** A check whose live data is unavailable reports
   `not_evaluated` with the reason. It never reports "no drift".
4. **Stable findings.** The same drift on the next run is the same finding,
   with the same `id`. A finding is not repeated as new each time.
5. **Same identities as weaver.** Services, nodes and edges are matched with
   the normalisation in edges-spec §2.3 and §4.1.

---

## 2. Inputs

| Input | Source | Used by |
|---|---|---|
| Service map | `weaver.nodes`, `weaver.edges` (MCP) | §4.2, §4.3 |
| Workloads of each service | falcon's `service-mappings.yaml` (falcon-architecture §1) | all checks |
| Workloads, Services, HPAs, pod images and labels | kube-state-metrics series (`kube_deployment_*`, `kube_statefulset_*`, `kube_service_info`, `kube_horizontalpodautoscaler_info`, `kube_pod_container_info`, `kube_pod_labels`) | §4.1, §4.1 |
| Observed calls | service-mesh and client metrics: whatever is exported (§4.2) | §4.2 |

Each run reads one consistent weaver build. Falcon records its build id
(`weaverBuild`, weaver-architecture §2) in the report.

---

## 3. Run

```
falcon drift-detect domain <name> [--check mapping,dependencies,infrastructure] [--window 7d]
```

- **Default:** all checks run. `--window` is the observation window for §4.2,
  and defaults to 7 days.
- **When it runs:** on demand, on a schedule, and after every weaver build of
  the domain.
- **Output:** `drift/<domain>.yaml` (§5), replaced atomically on every run.
  Also a summary on stdout.
- **Exit codes:**
  - `0`: no open findings at `high` severity or above.
  - `1`: at least one such finding.
  - `2`: the run failed, for example weaver or the metrics backend was unreachable.

---

## 4. Checks

### 4.1 `mapping`: service-mappings.yaml vs cluster

Evaluated from kube-state-metrics series: an object exists when its series does.

| Result | Condition | Severity |
|---|---|---|
| `missing_workload` | a listed `namespace/kind/name` does not exist | high |
| `missing_service` / `missing_hpa` | a listed Service or HPA does not exist | medium |
| `hpa_mismatch` | the HPA's `scaleTargetRef` is not the listed workload | medium |
| `unclaimed_workload` | a workload in a namespace that the mapping uses belongs to no `serviceName` | low |
| `unknown_service` | a `serviceName` is not a service in weaver's map | high |

### 4.2 `dependencies`: code map vs observed calls

Observed calls are collected over `--window` from **metrics only**; falcon
reads no traces or logs. Each metric source maps to an edge as follows:

| Source | Gives `from → to kind resource` from |
|---|---|
| Istio `istio_requests_total` | `source_workload` / `source_workload_namespace` → `destination_workload` / `destination_workload_namespace`; `request_protocol` gives `rest` or `grpc`. Istio doesn't label the request path by default, so these edges match at `from → to kind` level |
| Istio `istio_tcp_sent_bytes_total` | `source_workload` → `destination_service`: TCP connections to brokers and databases, matched to infrastructure nodes through their `aliases`. No topic or table, so `from → to kind` level |
| Kafka client metrics, if exported | producer and consumer client ids per topic: the only source that gives a topic |

**Mapping to weaver's terms:**
- Workloads become services via `service-mappings.yaml`.
- Hosts become nodes via node `aliases` (edges-spec §2.3).
- Resources are normalised as in edges-spec §3.1.
- A source that can't supply the resource matches at `from → to kind` level
  only. The finding says so.

| Result | Condition | Severity |
|---|---|---|
| `unexpected` | observed, but not in `edges.yaml`: either weaver missed it, or the code calls something weaver can't see | high if `to` is outside the domain, medium otherwise |
| `dormant` | in `edges.yaml`, but not observed in the window | info. Low if its caller's `entries` were observed with traffic: the code path runs, yet the call was never seen |
| `criticality_conflict` | a `call: optional`/`async` edge whose failures, observed in the same window, coincide with errors on its caller's entry point | medium |
| `not_evaluated` | no source covers this edge's kind | info |
| `probable_answer` | an observed call matches no edge, but the same `from` has an edge of the same `kind` to `unknown/<question-id>` (edges-spec §2.1): the observation probably answers that question. Reported once, with the question id, instead of an `unexpected` plus a `dormant` | info |

A `dormant` edge is never removed as dead. It may be a rarely used path, which
is exactly what the static map is for.

### 4.3 `infrastructure`: node properties vs observed addresses

Only workloads with `env: production` in `service-mappings.yaml`
(falcon-architecture §1.2) are compared: weaver's map describes production,
so a staging namespace would always differ. Other workloads are
`not_evaluated` here.

Falcon reads no configuration, pod specs or secrets; only metrics. Addresses
come from client metrics that carry them as labels (for example Kafka client
metrics with broker hosts, or connection-pool metrics with the database host),
mapped to services by workload.

| Result | Condition | Severity |
|---|---|---|
| `address_mismatch` | an observed address is not the node's address nor one of its `aliases`. Kafka brokers are compared as sets: one shared broker is enough | high |
| `extra_instance` | the workloads talk to an instance of this type that the map has no node for | medium |
| `not_evaluated` | no metric carries the address | info |

Addresses are normalised as in edges-spec §2.3: lower-cased, scheme removed,
default port added, broker lists sorted.

---

## 5. Report: `drift/<domain>.yaml`

```yaml
version: falcon/drift/v1.0
domain: payments
run: {at: 2026-10-24T11:00:00Z, window: 7d, weaverBuild: 42}
summary: {open: 1, high: 1, medium: 0, low: 0, info: 0, not_evaluated: [dependencies/redis]}
findings:
  - id: 2b44f0d1
    check: dependencies
    result: unexpected
    severity: high
    subject: {edge: "checkout → external/api.fraudcheck.io rest POST /v2/score"}
    live: {source: istio_requests_total, calls: 18230, firstObserved: 2026-10-22T07:41:00Z}
    fix: {file: edges.override.yaml, action: "add the edge, or find why weaver missed it"}
    firstSeen: 2026-10-24T11:00:00Z
    lastSeen:  2026-10-24T11:00:00Z
    status: open
```

| Field | Meaning |
|---|---|
| `id` | Stable: a hash of `check`, `result` and `subject`. The same drift keeps its `id`, `firstSeen` and `status` across runs |
| `fix` | The file to change and the action, never applied automatically (§6) |
| `status` | `open` · `accepted` (§7) · `resolved`. A finding no longer detected becomes `resolved` and is kept for 30 days |

---

## 6. Where fixes go

| Finding | File | Owner |
|---|---|---|
| `mapping` results | `service-mappings.yaml` | falcon operator |
| `unexpected`, `dormant`, `criticality_conflict` | `edges.override.yaml`, or a weaver extraction bug | service team |
| `address_mismatch`, `extra_instance` | `edges.override.yaml` (node `props`, `aliases`), or the deployment config | service team |

---

## 7. Accepting drift

Known, intended drift is recorded in `drift.accept.yaml`, kept next to
`service-mappings.yaml`:

```yaml
version: falcon/drift.accept/v1.0
accepted:
  - id: 2b44f0d1
    reason: "fraudcheck called through a sidecar; not in code by design"
    by: payments-team
    until: 2027-01-31
```

- **Effect:** an accepted finding gets `status: accepted` and leaves the
  summary counts. It still appears in the report.
- **Expiry:** `until` is required. An expired entry turns the finding back
  into `open`.
- **Unknown ids:** an id that no longer matches a finding is reported as a
  warning.

