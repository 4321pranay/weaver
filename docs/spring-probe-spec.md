# spring-probe Spec

spring-probe tells weaver what a Spring / Spring Boot service wires together
at runtime and source code alone doesn't show: which bean fills an injection
point, which methods the framework calls, what a proxy stands for, and which
values the properties resolve to. It targets Spring Boot 3 and later
(Spring Framework 6+); older services get only the static model (§3). Weaver uses these facts where the entries
walk (edges-spec §3.5) would otherwise end in `unknown`, and for addresses and
targets that would otherwise become questions.

Anything spring-probe can't establish stays a gap and follows the normal order:
build agent, then a human question (mcp-spec §3, questions-spec).

---

## 1. Shape

- **One Java tool, two front ends:** a CLI (`spring-probe capture …`) and an
  MCP server (`spring-probe mcp`, stdio). Both call the same code and return
  the same data: YAML from the CLI, like weaver's files, and JSON from MCP,
  which the protocol requires. Answers live in `<repo>/.spring-probe/answers.yaml`;
  inside weaver they come from `questions.yaml` on stdin instead.
- **Weaver's use:** `weaver build` starts `spring-probe mcp` once per build,
  calls `spring.capture` for each Spring service, reads the facts, and stops
  it. If no spring-probe binary is configured, weaver skips this step.
- **A build step, not live data:** running the service locally (§2, level 2) is like running maven or a SCIP indexer. It never touches a real
  environment.

## 2. Capture levels

spring-probe tries the levels in order and keeps every fact it gets. A later
level only adds facts that earlier levels didn't establish; it never overrides
them.

| # | Level | Needs | Gives |
|---|---|---|---|
| 1 | `aot`: run Spring AOT (`process-aot`), which refreshes the context up to bean definitions without creating beans, and read its output | a build | bean definitions with conditions evaluated, injection wiring, proxies, reflection hints, resolved properties; no connections |
| 2 | `runtime`: full local start with infrastructure stubbed (§4), read Actuator `beans`, `mappings`, `scheduledtasks`, `configprops`, then stop | a build, the stub rules for its Boot version | created beans and their real dependencies, endpoint mappings, schedules |
| 3 | `static`: Spring model over the code (§3) | source or bytecode | everything a static reading can tell; always runs as the baseline |

- Level 2 never uses lazy init: Actuator `beans` only lists the dependencies
  of beans that were actually created.
- A level that fails or times out is recorded with its reason, and the next
  level runs. A spring-probe failure never fails `weaver build`.

## 3. Static model (level 3)

- **Beans:** classes with `@Component`, `@Service`, `@Repository`,
  `@Controller`, `@RestController`, `@Configuration` + `@Bean` methods,
  limited to the packages of `@ComponentScan` / `@SpringBootApplication`.
- **Injection** (`@Autowired`, constructor, `@Resource`, `@Inject`). The
  first rule that gives one bean wins:
  1. `@Qualifier` on the parameter or field
  2. `@Qualifier` on the method
  3. `@Resource(name=…)`
  4. by type: the only candidate, else the `@Primary` one

  If several candidates remain, the injection point is a gap.
- **Proxies:**
  - Feign `@FeignClient` → a REST call (name/url, path, method)
  - Spring Data repository → its entity's table (`@Table`, `@Entity`)
  - MyBatis / MyBatis-Plus mapper → its table (`@TableName`, mapper XML)
- **Entry points:** the annotations listed for the service's Boot version in
  the rules (§5), e.g. `@Scheduled`, `@KafkaListener`, `@EventListener`,
  `@RocketMQMessageListener`, `@XxlJob`, plus the edges-spec §3.5 table.
- **AOP:** `@Async` and `@Transactional` don't break the walk: the caller
  still reaches the target method. `@Async` marks the call as asynchronous.
- **SpEL and reflection:** values from SpEL that reference only properties are
  resolved through §4 properties; anything else is a gap.

## 4. Running the service (levels 1 and 2)

**Sandbox:**

- no network route except loopback; a time limit per level (default 120 s);
- no credentials passed in, and none written to any output: property values
  whose keys match the secret patterns (`password`, `secret`, `token`, `key`,
  `credential`) are dropped before output.

**Stubs for level 2** come from the rules for the service's Boot version:

- datasources → H2 in-memory;
- Kafka, Redis, RocketMQ bootstrap → an unreachable loopback address;
  listeners `auto-startup=false`;
- config server, service discovery, and other remote config → off;
- the active profiles are the ones weaver passes (default: none).

## 5. Rules

Version-specific knowledge lives in rule files keyed by Boot `major.minor` (3.0 and later)
(and by framework for third-party starters). A rule file lists:

- property names (e.g. `spring.redis.*` → `spring.data.redis.*` in 3.0);
- the auto-configurations to stub, and the stub properties for each;
- entry-point annotations and the entry `kind` each maps to;
- proxy types and their meaning (REST, table).

An LLM may draft a rule file for a framework that has none. A human reviews
and commits it. An LLM never decides an individual call site.

## 6. Output

One result per service and commit, cached by `(gitUrl, commit, Boot
version, rules hash)`. Every fact carries its `source` (`aot` ·
`runtime` · `static`) and the weaver run id.

```yaml
version: spring-probe/facts/v1.0
service: checkout
commit: 3f2a9c1
boot: "3.2"
run: run-a82557c687e1
levels:
  - {level: aot, status: ok}
  - {level: runtime, status: failed, reason: "timeout after 120s"}
beans:
  - {name: paymentClient, type: com.x.PaymentClient, source: aot}
injection:
  - {at: "OrderService.java:41", type: com.x.PaymentGateway,
     bean: stripeGateway, impl: com.x.StripeGateway, source: aot}
entrypoints:
  - {kind: schedule, resource: "0 */5 * * * *",
     symbol: com.x.ReconcileJob#run, source: static}
proxies:
  - {type: com.x.PaymentClient, means: rest, target: payment,
     resource: "POST /v1/charges", source: static}
  - {type: com.x.OrderMapper, means: table, resource: t_order, source: static}
properties:
  - {key: spring.kafka.bootstrap-servers, value: "kafka-orders:9092",
     source: aot}
gaps:
  - {kind: implementation, at: "RefundService.java:18",
     candidates: [com.x.A, com.x.B]}
```

**How weaver uses it:**

| Fact | Used for |
|---|---|
| `injection` | the entries walk crosses the interface call to `impl` |
| `entrypoints` | added to the entry points of edges-spec §3.5 |
| `proxies` | REST and table edges with their `resource` |
| `properties` | addresses for edges-spec §2.2 nodes |
| `gaps` | become build-agent gaps (`implementation`, `entries`, `address`) |

## 7. MCP tools and CLI

| MCP tool | CLI | Returns |
|---|---|---|
| `spring.capture` | `spring-probe capture <repo> [--commit] [--profiles] [--levels]` | runs the levels, returns the §6 result |
| `spring.beans` | `spring-probe beans <service> [--type]` | beans |
| `spring.injection` | `spring-probe injection <service> <file:line>` | the bean for one injection point |
| `spring.entrypoints` | `spring-probe entrypoints <service>` | entry points |
| `spring.proxies` | `spring-probe proxies <service>` | proxies and their meaning |
| `spring.properties` | `spring-probe properties <service> [--key]` | resolved non-secret properties |

All tools except `spring.capture` read the cached result and never run the
service.
