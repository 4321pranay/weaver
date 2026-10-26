# RCA Spec

How falcon writes up an incident analysis. Every analysis produces:
- **`rca/<incident-id>.yaml`**, the record. Tools, tickets and later
  analyses read it.
- **`rca/<incident-id>.md`** and **`rca/<incident-id>.pdf`**, the same content
  rendered as a postmortem for people. `falcon rca pdf <incident-id>`
  regenerates the PDF, e.g. after a review.

The YAML is the source; the Markdown and PDF are always generated from it and
never edited.

---

## 1. Principles

1. **Every claim has evidence.** A finding without at least one evidence item
   is not written.
2. **Facts and inferences are kept apart.** Evidence is what a tool returned.
   A finding is falcon's reading of that evidence, with a confidence.
3. **What was ruled out is recorded.** Hypotheses that were checked and
   rejected are part of the result, so a reader can see what was considered.
4. **What couldn't be seen is recorded.** Missing metrics, unmapped services,
   open weaver questions and `not_evaluated` drift checks are listed, never
   left out.
5. **Trust markers travel.** A fact from weaver marked `match: name`,
   `resolvedBy: llm`, `call: unknown` or `question: <id>`, or one that ends at
   an `unknown/` node, keeps that marker in the evidence. A finding never rests
   on a placeholder alone.
6. **No action is taken.** Actions are recommendations for people. Falcon
   doesn't roll back, restart or change anything.
7. **Checked, not trusted.** Before a record is written, falcon checks it the
   way weaver checks weave-ai (analyzer-spec §5): every evidence item must have
   been returned by a tool in that analysis, with its `at`, `query` or `ref`,
   and every quoted value must appear in what the tool returned. A finding
   whose evidence fails is dropped. `confidence` is computed from the
   evidence by the rules of §3, never chosen by the LLM.
8. **Bounded.** An analysis has a budget of tool calls, tokens and time. When
   one runs out, or the LLM is unreachable, falcon writes what it has with
   status `partial`, listing what it didn't get to under `unseen`.

---

## 2. Record: `rca/<incident-id>.yaml`

```yaml
version: falcon/rca/v1.0
incident:
  id: INC-2026-1024-01
  opened: 2026-10-24T10:04:00Z
  trigger: {source: alert, name: "OrdersErrorRateHigh", workload: shop/Deployment/checkout-api}
  status: analysed                      # analysing | partial | analysed | reviewed
analysis:
  at: 2026-10-24T10:19:30Z
  window: {from: 2026-10-24T09:30:00Z, to: 2026-10-24T10:19:00Z}
  domain: payments
  weaverBuild: 42                       # the weaver build id (generation) the analysis read
  servers: [weaver, wiki]               # knowledge MCP servers consulted

summary: >
  POST /orders fails because checkout turns payment's 4xx answers into 500s.
  The call became required in checkout 1.4.0.

impact:
  entries:                              # the entry points that degraded
    - {service: checkout, kind: rest, resource: POST /orders, errorRate: 0.31, baseline: 0.002}
  services: [checkout]
  window: {from: 2026-10-24T10:02:00Z, to: null}   # null: still ongoing

findings:
  - id: F1
    kind: root_cause                    # root_cause | contributing | symptom
    statement: "checkout → payment POST /pay changed from optional to required in 1.4.0; a 4xx from payment is rethrown as 500"
    confidence: high                    # high | medium | low (§3)
    evidence: [E1, E2, E3]
  - id: F2
    kind: contributing
    statement: "No exception handler maps HttpClientErrorException, so the 500 reaches the caller"
    confidence: medium
    evidence: [E4]

evidence:
  - id: E1
    source: weaver
    tool: diff
    ref: "checkout → payment rest POST /pay"
    detail: "call: optional → required between 1.3.2 and 1.4.0"
  - id: E2
    source: weaver
    tool: read
    at: src/main/java/shop/CheckoutService.java:42
    release: 1.4.0
    detail: "catch rethrows the 4xx as RuntimeException"
  - id: E3
    source: metrics
    query: 'sum(rate(istio_requests_total{destination_workload="payment",response_code=~"4.."}[5m]))'
    detail: "payment 4xx: 0.1/s before 10:02, 3.4/s after"
  - id: E4
    source: weaver
    tool: search
    query: 'repo:^checkout$ ControllerAdvice|HttpClientErrorException'
    detail: "no handler found"
    marker: match:name                  # a trust marker carried from weaver (principle 5)
  - id: E5
    source: metrics
    query: 'kube_horizontalpodautoscaler_status_current_replicas{horizontalpodautoscaler="payment-hpa"}'
    detail: "3 of 10 replicas throughout the window"

ruledOut:
  - hypothesis: "payment is out of capacity"
    evidence: [E5]
    reason: "HPA at 3 of 10 replicas; payment latency unchanged"

unseen:                                 # what the analysis could not see (principle 4)
  - "payment → database/ledger: no metric carries the database host"
  - "open weaver question q-f42ffdad: courier's Kafka instance unknown"

timeline:
  - {at: 2026-10-24T09:58:00Z, event: "checkout 1.4.0 deployed"}
  - {at: 2026-10-24T10:02:00Z, event: "payment 4xx rise", evidence: [E3]}
  - {at: 2026-10-24T10:04:00Z, event: "alert OrdersErrorRateHigh"}

actions:                                # recommendations only (principle 6)
  - {type: mitigate, text: "roll back checkout to 1.3.2"}
  - {type: fix, text: "pass payment's 4xx through as 4xx in CheckoutService.placeOrder", owner: null}
  - {type: knowledge, text: "answer weaver question q-f42ffdad"}
```

| Field | Meaning |
|---|---|
| `analysis` | When it ran, the metrics window, the domain, and the exact weaver build (`weaverBuild`, the build id) and knowledge servers it used |
| `impact` | The entry points that degraded, with their error rate against a baseline; the affected services; how long. `to: null` means still ongoing |
| `evidence` | Every fact used, with its source: the tool and its `ref`, `at`, `release` or `query`, so it can be checked again |
| `unseen` | Gaps in what the analysis could see |
| `actions` | Recommendations: `mitigate`, `fix`, `knowledge` (improve weaver's or falcon's data), `monitor` |

---

## 3. Confidence

| Confidence | When |
|---|---|
| `high` | At least two independent kinds of evidence agree (e.g. a code change in weaver and a matching metric change), and no evidence contradicts the finding |
| `medium` | One kind of evidence supports it, or any of its evidence carries a trust marker (`match: name`, `resolvedBy: llm`, `call: unknown`, `question: <id>`, an `unknown/` node) |
| `low` | It is an inference with no direct evidence of the mechanism, e.g. timing only |

A `root_cause` finding at `low` confidence is written as `contributing` and
listed in `unseen` as an open question.

---

## 4. Rendered page: `.md` and `.pdf`

Generated from the record in postmortem order: title and status, summary,
impact, findings with their confidence, ruled out, what couldn't be seen,
timeline, recommended actions, provenance. Each evidence item appears once, in
an appendix, and findings link to it by id. The PDF has the same content and
order as the Markdown, with evidence links that work offline.

---

## 5. Lifecycle

| Status | Meaning |
|---|---|
| `analysing` | The analysis is running |
| `partial` | The analysis stopped early (budget used up, LLM unreachable): its findings are checked, but incomplete, and `unseen` says what is missing |
| `analysed` | Falcon finished. The record is complete and its evidence fixed |
| `reviewed` | An engineer confirmed or corrected it. Corrections are new findings with `by: <name>`; falcon's findings are kept and marked `rejectedBy: <name>` instead of being deleted |

A new analysis of the same incident (for example after it keeps going) writes
a new record with a suffix (`INC-2026-1024-01.2`) and links back to the
previous one.

