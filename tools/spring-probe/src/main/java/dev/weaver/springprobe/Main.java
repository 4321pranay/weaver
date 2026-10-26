package dev.weaver.springprobe;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.Comparator;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.TimeUnit;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import java.util.stream.Stream;

/** spring-probe CLI and MCP server (stdio). Both call the same functions and return the same JSON. */
public final class Main {
    static final String VERSION = "spring-probe/facts/v1.0";
    static final Path CACHE = Path.of(System.getProperty("user.home"), ".cache", "spring-probe");
    static final List<String> LEVELS = List.of("aot", "runtime", "static");

    @SuppressWarnings("unchecked")
    public static void main(String[] a) throws Exception {
        if (a.length == 0) {
            usage();
            return;
        }
        Map<String, String> o = opts(a);
        if ("-".equals(o.get("answers"))) answersIn = (Map<String, Map<String, Object>>) Yaml.parse(new String(System.in.readAllBytes(), StandardCharsets.UTF_8));
        Object out = switch (a[0]) {
            case "capture" -> capture(o);
            case "beans", "injection", "entrypoints", "proxies", "properties", "questions" -> read(a[0], o);
            case "answer" -> answer(o);
            case "mcp" -> { Mcp.serve(); yield null; }
            case "selftest" -> selftest();
            default -> { usage(); yield null; }
        };
        if (out != null) System.out.print(Yaml.write(out)); // like weaver's own files; MCP replies stay JSON (the protocol)
    }

    private static void usage() {
        System.err.println("""
                usage:
                  spring-probe capture <repo> [--service N] [--commit SHA] [--profiles a,b] [--levels aot,runtime,static] [--run RUN] [--timeout SEC] [--answers -]
                  spring-probe beans <service> [--type T]
                  spring-probe injection <service> [<symbol>]
                  spring-probe entrypoints <service>
                  spring-probe proxies <service>
                  spring-probe properties <service> [--key K]
                  spring-probe questions <service>
                  spring-probe answer <service> [<key> <value>] [--by NAME]
                  spring-probe mcp""");
    }

    /** Positional args go to "arg0", "arg1"...; "--k v" to "k". */
    static Map<String, String> opts(String[] a) {
        Map<String, String> o = new LinkedHashMap<>();
        int pos = 0;
        for (int i = 1; i < a.length; i++) {
            if (a[i].startsWith("--") && i + 1 < a.length) o.put(a[i].substring(2), a[++i]);
            else o.put("arg" + pos++, a[i]);
        }
        return o;
    }

    // ---- capture ----

    static Map<String, Object> capture(Map<String, String> o) throws Exception {
        Path repo = Path.of(req(o, "arg0", "repo")).toAbsolutePath().normalize();
        String service = o.getOrDefault("service", repo.getFileName().toString());
        String profiles = o.getOrDefault("profiles", "");
        List<String> levels = List.of(o.getOrDefault("levels", String.join(",", LEVELS)).split(","));
        int timeout = Integer.parseInt(o.getOrDefault("timeout", "120"));
        String commit = git(repo, "rev-parse", "HEAD");
        if (commit == null) commit = "nogit";
        if (o.containsKey("commit") && !commit.startsWith(o.get("commit")))
            throw new IllegalArgumentException("repo is at " + commit + ", not " + o.get("commit") + "; check out the commit first");
        String gitUrl = git(repo, "config", "remote.origin.url");

        boolean stateless = answersIn != null;
        Path work = stateless ? Files.createTempDirectory("spring-probe-") : Files.createDirectories(CACHE.resolve(service).resolve("work"));
        try {
        // one build at the root; every module whose classes hold a @SpringBootApplication is an app of its own
        String buildError = null;
        boolean gradle = !Files.exists(repo.resolve("pom.xml"));
        try {
            if (gradle) {
                // an init script adds a task to every Java project that writes its runtime classpath
                Path init = work.resolve("spring-probe.gradle");
                Files.writeString(init, GRADLE_INIT);
                String gw = Files.exists(repo.resolve("gradlew")) ? repo.resolve("gradlew").toString() : "gradle";
                exec(List.of(gw, "-q", "--continue", "--no-daemon", "-I", init.toString(), "springProbeClasspath"), repo, 1800, work.resolve("gradle.log"),
                        System.getenv("SPRING_PROBE_GRADLE_JAVA_HOME"));
            } else {
                exec(List.of("mvn", "-q", "-B", "-fae", "-f", repo.resolve("pom.xml").toString(), "-DskipTests", "compile",
                        "dependency:build-classpath", "-Dmdep.includeScope=runtime", "-Dmdep.outputFile=target/" + CP_FILE), repo, 1200, work.resolve("mvn.log"), null);
            }
        } catch (Exception e) {
            buildError = (gradle ? "gradle: " : "maven: ") + e.getMessage(); // --continue / -fae: modules that did build are still captured
        }
        String rulesJson = rules();
        Path rulesFile = work.resolve("rules.json");
        Files.writeString(rulesFile, rulesJson);
        Map<String, Map<String, Object>> allAnswers = answers(repo);
        List<Map<String, Object>> apps = apps(repo);

        String key = sha(gitUrl + "|" + commit + "|" + sha(rulesJson) + "|" + profiles + "|" + levels + "|" + sha(Json.write(allAnswers))).substring(0, 16);
        Path cached = CACHE.resolve(service).resolve(key + ".json");
        if (!stateless && Files.exists(cached) && !"nogit".equals(commit) && !o.containsKey("force")) {
            Files.copy(cached, CACHE.resolve(service).resolve("latest.json"), java.nio.file.StandardCopyOption.REPLACE_EXISTING);
            return parse(cached);
        }

        List<Map<String, Object>> status = new ArrayList<>(), gaps = new ArrayList<>(), questions = new ArrayList<>();
        Map<String, List<Map<String, Object>>> facts = new LinkedHashMap<>();
        if (buildError != null) gaps.add(Json.map("kind", "build", "app", ".", "reason", buildError));
        if (apps.isEmpty()) gaps.add(Json.map("kind", "build", "app", ".", "reason", "no module has a @SpringBootApplication class in its compiled classes"));
        for (Map<String, Object> app : apps) {
            String name = s(app.get("app"));
            Path mod = name.equals(".") ? repo : repo.resolve(name);
            Path classes = repo.resolve(s(app.get("classes"))), cpFile = mod.resolve((gradle ? "build/" : "target/") + CP_FILE);
            String deps = Files.exists(cpFile) ? Files.readString(cpFile).trim() : "";
            String boot = bootVersion(deps), mainClass = s(app.get("mainClass"));
            app.put("boot", boot);
            Path appWork = Files.createDirectories(work.resolve(name.replace('/', '_')));
            // answered production values: the profile becomes the active profile, the rest command-line properties
            Map<String, Map<String, Object>> ans = appAnswers(allAnswers, name);
            Map<String, Object> values = new LinkedHashMap<>();
            ans.forEach((k, v) -> values.put(k, v.get("value")));
            String appProfiles = !o.containsKey("profiles") && values.containsKey(PROFILES) ? s(values.get(PROFILES)) : profiles;
            values.remove(PROFILES);
            Path answersFile = appWork.resolve("answers.json");
            Files.writeString(answersFile, Json.write(values));

            List<Map<String, Object>> results = new ArrayList<>();
            boolean boot3 = boot != null && Integer.parseInt(boot.split("\\.")[0]) >= 3;
            for (String level : LEVELS) {
                if (!levels.contains(level)) continue;
                Map<String, Object> st = Json.map("app", name, "level", level);
                status.add(st);
                if (!boot3 && !level.equals("static")) {
                    st.put("status", "skipped");
                    st.put("reason", boot == null ? "no classpath from maven" : "Spring Boot " + boot + ": only the static model runs below 3.0");
                    continue;
                }
                String cp = jar() + ":" + classes + ":" + deps;
                if (level.equals("runtime") && !deps.contains("/h2-")) cp += ":" + stubJar("com.h2database:h2:2.2.224");
                Path out = appWork.resolve(level + ".json");
                Files.deleteIfExists(out);
                long t0 = System.nanoTime();
                String fail = sandboxed(List.of(javaBin(), "-Dspring.devtools.restart.enabled=false", "-cp", cp, "dev.weaver.springprobe.Agent", level, mainClass, rulesFile.toString(), out.toString(), appProfiles, answersFile.toString()),
                        mod, timeout, appWork.resolve(level + ".log"));
                st.put("seconds", Math.round((System.nanoTime() - t0) / 1e8) / 10.0);
                if (fail == null && Files.exists(out)) {
                    st.put("status", "ok");
                    results.add(tag(parse(out), level));
                } else {
                    st.put("status", "failed");
                    st.put("reason", fail != null ? fail : "no output");
                    if (!stateless) st.put("log", appWork.resolve(level + ".log").toString());
                }
            }
            if (results.isEmpty()) {
                gaps.add(Json.map("kind", "build", "app", name, "reason", "no level captured this app"));
                continue;
            }
            Map<String, Object> f = merge(results);
            for (Map<String, Object> q : questions(results, f, ans)) questions.add(appTag(q, name));
            f.forEach((section, l) -> {
                for (Map<String, Object> x : (List<Map<String, Object>>) l) (section.equals("gaps") ? gaps : facts.computeIfAbsent(section, k -> new ArrayList<>())).add(appTag(x, name));
            });
        }

        Map<String, Object> res = new LinkedHashMap<>();
        res.put("version", VERSION);
        res.put("service", service);
        res.put("repo", repo.toString());
        res.put("gitUrl", gitUrl);
        res.put("commit", commit);
        res.put("run", o.get("run"));
        res.put("at", Instant.now().toString());
        res.put("profiles", profiles);
        res.put("apps", apps);
        res.put("levels", status);
        for (String section : List.of("beans", "injection", "entrypoints", "proxies", "properties")) res.put(section, facts.getOrDefault(section, List.of()));
        res.put("gaps", gaps);
        res.put("questions", groupQuestions(questions));
        if (stateless) return res;
        String json = Json.write(res);
        Files.writeString(cached, json);
        Files.writeString(CACHE.resolve(service).resolve("latest.json"), json);
        return res;
        } finally {
            if (stateless) try (var w = Files.walk(work)) { w.sorted(Comparator.reverseOrder()).forEach(p -> p.toFile().delete()); }
        }
    }

    /** spring-probe selftest: YAML written and read back gives the same data. */
    static Object selftest() {
        Map<String, Object> v = Json.map("cache.type", Json.map("value", "redis", "by", "pranay", "at", "2026-10-25T06:30:59Z"),
                "jobs.cron", Json.map("value", "0 0 * * * *", "by", "a: b # c"), "flag", Json.map("value", "no"),
                "list", List.of("x", Json.map("k", "v", "n", List.of()), List.of("y")), "empty", Map.of());
        Object back = Yaml.parse(Yaml.write(v));
        if (!Json.write(v).equals(Json.write(back))) throw new IllegalStateException("yaml round trip:\n" + Yaml.write(v) + "->\n" + Json.write(back));
        Object hand = Yaml.parse("# by hand\ncache.type:\n  value: redis   # plain\n  by: 'o''neil'\nlist:\n- a\n- k: v\n  j: w\n");
        if (!Json.write(hand).equals(Json.write(Json.map("cache.type", Json.map("value", "redis", "by", "o'neil"),
                "list", List.of("a", Json.map("k", "v", "j", "w")))))) throw new IllegalStateException("yaml by hand: " + Json.write(hand));
        if (!(Yaml.parse("{}\n") instanceof Map<?, ?> m0 && m0.isEmpty())) throw new IllegalStateException("yaml {}");
        return Json.map("selftest", "ok");
    }

    static final String CP_FILE = "spring-probe-cp.txt";

    /** The repo's apps: each module (the root included) whose compiled classes hold a @SpringBootApplication:
     *  Maven's target/classes, Gradle's build/classes/{java,kotlin}/main. */
    static List<Map<String, Object>> apps(Path repo) throws IOException {
        List<Map<String, Object>> out = new ArrayList<>();
        try (Stream<Path> w = Files.walk(repo)) {
            for (Path c : w.filter(p -> Files.isDirectory(p) && !repo.relativize(p).toString().contains(".git") && module(p) != null).sorted().toList()) {
                String main = mainClass(c);
                if (main == null) continue;
                String rel = repo.relativize(module(c)).toString();
                out.add(Json.map("app", rel.isEmpty() ? "." : rel, "mainClass", main, "classes", repo.relativize(c).toString()));
            }
        }
        return out;
    }

    /** The module a classes folder belongs to, or null when it isn't one. */
    static Path module(Path c) {
        if (c.endsWith("target/classes") && Files.exists(c.getParent().getParent().resolve("pom.xml"))) return c.getParent().getParent();
        if ((c.endsWith("build/classes/java/main") || c.endsWith("build/classes/kotlin/main"))) {
            Path m = c.getParent().getParent().getParent().getParent();
            if (Files.exists(m.resolve("build.gradle")) || Files.exists(m.resolve("build.gradle.kts"))) return m;
        }
        return null;
    }

    static final String GRADLE_INIT = """
            allprojects {
                afterEvaluate { p ->
                    if (p.plugins.hasPlugin('java')) {
                        p.tasks.create('springProbeClasspath') {
                            dependsOn 'classes'
                            doLast {
                                def f = p.file('build/%s')
                                f.parentFile.mkdirs()
                                f.text = (p.sourceSets.main.runtimeClasspath.files - p.sourceSets.main.output.classesDirs.files).join(':')
                            }
                        }
                    }
                }
            }
            """.formatted(CP_FILE);

    /** An app's answers: "<app>:<key>" answers it alone, a plain "<key>" every app of the repo. */
    static Map<String, Map<String, Object>> appAnswers(Map<String, Map<String, Object>> all, String app) {
        Map<String, Map<String, Object>> out = new LinkedHashMap<>();
        all.forEach((k, v) -> { if (!k.contains(":")) out.put(k, v); });
        all.forEach((k, v) -> { if (k.startsWith(app + ":")) out.put(k.substring(app.length() + 1), v); });
        return out;
    }

    /** A key asked in several apps of the repo is asked once, for the repo (app "."): its plain-key answer
     *  applies to every app (appAnswers). apps lists where it came from. Asked in one app, it stays that app's. */
    @SuppressWarnings("unchecked")
    static List<Map<String, Object>> groupQuestions(List<Map<String, Object>> qs) {
        Map<String, List<Map<String, Object>>> by = new LinkedHashMap<>();
        for (Map<String, Object> q : qs) by.computeIfAbsent(q.get("kind") + "|" + q.get("key"), k -> new ArrayList<>()).add(q);
        List<Map<String, Object>> out = new ArrayList<>();
        for (List<Map<String, Object>> g : by.values()) {
            if (g.size() == 1) {
                out.add(g.get(0));
                continue;
            }
            Map<String, Object> q = new LinkedHashMap<>(g.get(0));
            Set<String> opts = new java.util.LinkedHashSet<>(), used = new java.util.LinkedHashSet<>();
            List<String> apps = new ArrayList<>();
            boolean answered = true;
            for (Map<String, Object> x : g) {
                opts.addAll((List<String>) x.get("options"));
                used.addAll((List<String>) x.get("usedBy"));
                apps.add(s(x.get("app")));
                answered &= "answered".equals(x.get("status"));
            }
            q.put("app", ".");
            q.put("apps", apps);
            q.put("options", new ArrayList<>(opts));
            q.put("usedBy", new ArrayList<>(used));
            q.put("status", answered ? "answered" : "open");
            if (!answered) q.remove("answer");
            out.add(q);
        }
        return out;
    }

    static String answerKey(Map<String, Object> q) {
        return ".".equals(q.get("app")) || q.get("app") == null ? s(q.get("key")) : q.get("app") + ":" + q.get("key");
    }

    private static Map<String, Object> appTag(Map<String, Object> m, String app) {
        m.put("app", app);
        return m;
    }

    // ---- production config questions ----

    static final String PROFILES = "spring.profiles.active";
    /** Answers live in the service's repo, reviewed and versioned with its code. With --answers - they come
     *  on stdin instead and capture keeps no state (no cache, no files): weaver's build calls it this way,
     *  its questions.yaml holding the answers. */
    static Map<String, Map<String, Object>> answersIn;
    static Path answersFile(Path repo) {
        return repo.resolve(".spring-probe").resolve("answers.yaml");
    }

    /**
     * Config keys whose production value can send the code down a different path (profile, condition)
     * or into a fact (topic, URL, address). No production config is available, so a person answers them.
     */
    @SuppressWarnings("unchecked")
    static List<Map<String, Object>> questions(List<Map<String, Object>> results, Map<String, Object> facts, Map<String, Map<String, Object>> ans) {
        Set<String> appKeys = new HashSet<>();
        ((List<Map<String, Object>>) facts.get("properties")).forEach(p -> appKeys.add(s(p.get("key"))));
        List<Map<String, Object>> out = new ArrayList<>();
        for (Map<String, Object> r : results)
            for (Map<String, Object> q : (List<Map<String, Object>>) r.getOrDefault("questions", List.of())) {
                if ("address".equals(q.get("kind")) && !appKeys.contains(s(q.get("key")))) continue; // app doesn't use it
                Map<String, Object> a = ans.get(s(q.get("key")));
                q.put("status", a == null ? "open" : "answered");
                if (a != null) q.put("answer", a);
                out.add(q);
            }
        // answered values replace the dev config's in the reported properties
        List<Map<String, Object>> props = (List<Map<String, Object>>) facts.get("properties");
        ans.forEach((k, a) -> {
            if (k.equals(PROFILES)) return;
            props.removeIf(p -> k.equals(p.get("key")));
            props.add(Json.map("key", k, "value", a.get("value"), "source", "answer"));
        });
        return out;
    }

    @SuppressWarnings("unchecked")
    static Map<String, Map<String, Object>> answers(Path repo) throws IOException {
        if (answersIn != null) return new LinkedHashMap<>(answersIn);
        Path f = answersFile(repo);
        return Files.exists(f) ? (Map<String, Map<String, Object>>) Yaml.parse(Files.readString(f)) : new LinkedHashMap<>();
    }


    /** spring-probe answer <service> [<key> <value>] [--by name]: interactive without key. No defaults are offered. */
    @SuppressWarnings("unchecked")
    static Object answer(Map<String, String> o) throws Exception {
        String service = req(o, "arg0", "service");
        Path latest = CACHE.resolve(service).resolve("latest.json");
        if (!Files.exists(latest)) throw new IllegalArgumentException("no capture for " + service + "; run spring-probe capture first");
        Map<String, Object> cap = parse(latest);
        List<Map<String, Object>> qs = (List<Map<String, Object>>) cap.get("questions");
        Path repo = Path.of(s(cap.get("repo")));
        Map<String, Map<String, Object>> ans = answers(repo);
        String by = o.getOrDefault("by", git(Path.of("."), "config", "user.name") != null ? git(Path.of("."), "config", "user.name") : System.getenv("USER"));
        int n = 0;
        if (o.containsKey("arg1")) {
            String k = o.get("arg1"); // <key>, or <app>:<key> in a multi-module repo
            if (qs.stream().noneMatch(q -> k.equals(answerKey(q)) || k.equals(q.get("key")))) throw new IllegalArgumentException(k + " is not a question for " + service);
            ans.put(k, Json.map("value", req(o, "arg2", "value"), "by", by, "at", Instant.now().toString()));
            n = 1;
        } else {
            BufferedReader in = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
            List<Map<String, Object>> open = qs.stream().filter(q -> !ans.containsKey(answerKey(q))).toList();
            System.err.println(open.size() + " open question(s) for " + service + ". Type the production value; empty skips, q stops.\n");
            for (Map<String, Object> q : open) {
                System.err.println("[" + q.get("kind") + "] " + answerKey(q));
                List<String> opts = (List<String>) q.get("options");
                if (!opts.isEmpty()) System.err.println("  paths: " + String.join(" | ", opts));
                List<String> used = (List<String>) q.get("usedBy");
                System.err.println("  decides: " + String.join(", ", used.subList(0, Math.min(4, used.size()))) + (used.size() > 4 ? ", …" : ""));
                System.err.print("  production value> ");
                String line = in.readLine();
                if (line == null || line.trim().equals("q")) break;
                if (line.isBlank()) { System.err.println(); continue; }
                ans.put(answerKey(q), Json.map("value", line.trim(), "by", by, "at", Instant.now().toString()));
                n++;
                System.err.println();
            }
        }
        Files.createDirectories(answersFile(repo).getParent());
        Files.writeString(answersFile(repo), "# production values answered with `spring-probe answer`, read by `spring-probe capture`\n" + Yaml.write(ans));
        return Json.map("service", service, "answered", n, "file", answersFile(repo).toString(),
                "next", "spring-probe capture <repo> applies the answers");
    }

    private static Map<String, Object> tag(Map<String, Object> m, String level) {
        m.put("_level", level);
        return m;
    }

    /** Earlier levels win; a later level only adds facts the earlier ones didn't establish. */
    @SuppressWarnings("unchecked")
    static Map<String, Object> merge(List<Map<String, Object>> results) {
        Map<String, Object> out = new LinkedHashMap<>();
        for (String section : List.of("beans", "injection", "entrypoints", "proxies", "properties")) {
            List<Map<String, Object>> merged = new ArrayList<>();
            Set<String> seen = new HashSet<>();
            for (Map<String, Object> r : results)
                for (Map<String, Object> f : (List<Map<String, Object>>) r.get(section)) {
                    List<String> keys = switch (section) {
                        case "beans" -> List.of(s(f.get("type")));
                        case "injection" -> List.of(s(f.get("at")));
                        case "entrypoints" -> List.of(f.get("kind") + "|" + f.get("symbol"), f.get("kind") + "|" + f.get("resource"));
                        case "proxies" -> List.of(s(f.get("type")));
                        default -> List.of(s(f.get("key")));
                    };
                    if (keys.stream().anyMatch(seen::contains)) {
                        // aot only knows a @Bean method's declared type; a later level may know the real class
                        if (section.equals("injection")) merged.stream().filter(m -> keys.contains(s(m.get("at")))
                                && s(m.get("impl")).equals(s(m.get("type"))) && f.get("impl") != null && !f.get("impl").equals(m.get("impl")))
                                .forEach(m -> { m.put("impl", f.get("impl")); m.put("source", m.get("source") + "+" + f.get("source")); });
                        continue;
                    }
                    seen.addAll(keys);
                    merged.add(f);
                }
            out.put(section, merged);
        }
        // a gap stays only if no level established that fact
        Set<String> resolved = new HashSet<>();
        ((List<Map<String, Object>>) out.get("injection")).forEach(f -> resolved.add(s(f.get("at"))));
        List<Map<String, Object>> gaps = new ArrayList<>();
        Set<String> seen = new HashSet<>();
        for (Map<String, Object> r : results)
            for (Map<String, Object> g : (List<Map<String, Object>>) r.get("gaps")) {
                String k = g.get("kind") + "|" + g.get("at");
                if (resolved.contains(s(g.get("at"))) || !seen.add(k)) continue;
                gaps.add(g);
            }
        out.put("gaps", gaps);
        return out;
    }

    private static String s(Object o) {
        return String.valueOf(o);
    }

    // ---- read tools: never run anything, only read the cached result ----

    @SuppressWarnings("unchecked")
    static Object read(String what, Map<String, String> o) throws IOException {
        String service = req(o, "arg0", "service");
        Path latest = CACHE.resolve(service).resolve("latest.json");
        if (!Files.exists(latest)) throw new IllegalArgumentException("no capture for " + service + "; run spring-probe capture first");
        Map<String, Object> r = parse(latest);
        List<Map<String, Object>> l = (List<Map<String, Object>>) r.get(what);
        String filter = switch (what) {
            case "beans" -> o.get("type");
            case "injection" -> o.get("arg1");
            case "properties" -> o.get("key");
            default -> null;
        };
        if (filter != null) {
            String field = switch (what) { case "beans" -> "type"; case "injection" -> "at"; default -> "key"; };
            l = l.stream().filter(f -> s(f.get(field)).contains(filter)).toList();
        }
        Map<String, Object> out = new LinkedHashMap<>();
        out.put("service", service);
        out.put("commit", r.get("commit"));
        out.put(what, l);
        if (what.equals("injection")) {
            List<Map<String, Object>> gaps = ((List<Map<String, Object>>) r.get("gaps")).stream()
                    .filter(g -> "implementation".equals(g.get("kind")) && (filter == null || s(g.get("at")).contains(filter))).toList();
            out.put("gaps", gaps);
        }
        return out;
    }

    // ---- helpers ----

    static String req(Map<String, String> o, String k, String what) {
        String v = o.get(k);
        if (v == null) throw new IllegalArgumentException("missing " + what);
        return v;
    }

    @SuppressWarnings("unchecked")
    static Map<String, Object> parse(Path p) throws IOException {
        return (Map<String, Object>) Json.parse(Files.readString(p));
    }

    static String rules() throws IOException {
        try (InputStream in = Main.class.getResourceAsStream("/rules/boot-3.json")) {
            return new String(in.readAllBytes(), StandardCharsets.UTF_8);
        }
    }

    static String bootVersion(String cp) {
        Matcher m = Pattern.compile("spring-boot-(\\d+\\.\\d+)\\.[^/]*\\.jar").matcher(cp);
        return m.find() ? m.group(1) : null;
    }

    /** The @SpringBootApplication class: its descriptor appears in the class file's constant pool. */
    static String mainClass(Path classes) throws IOException {
        if (!Files.isDirectory(classes)) return null;
        byte[] marker = "Lorg/springframework/boot/autoconfigure/SpringBootApplication;".getBytes(StandardCharsets.UTF_8);
        try (Stream<Path> s = Files.walk(classes)) {
            for (Path p : s.filter(f -> f.toString().endsWith(".class")).toList()) {
                if (indexOf(Files.readAllBytes(p), marker) >= 0) {
                    String rel = classes.relativize(p).toString();
                    return rel.substring(0, rel.length() - 6).replace('/', '.');
                }
            }
        }
        return null;
    }

    private static int indexOf(byte[] h, byte[] n) {
        outer:
        for (int i = 0; i <= h.length - n.length; i++) {
            for (int j = 0; j < n.length; j++) if (h[i + j] != n[j]) continue outer;
            return i;
        }
        return -1;
    }

    static String jar() throws Exception {
        return Path.of(Main.class.getProtectionDomain().getCodeSource().getLocation().toURI()).toString();
    }

    static String javaBin() {
        return Path.of(System.getProperty("java.home"), "bin", "java").toString();
    }

    static String stubJar(String coords) throws Exception {
        String[] c = coords.split(":");
        Path lib = Files.createDirectories(CACHE.resolve("lib"));
        Path jar = lib.resolve(c[1] + "-" + c[2] + ".jar");
        if (!Files.exists(jar))
            exec(List.of("mvn", "-q", "-B", "dependency:copy", "-Dartifact=" + coords, "-DoutputDirectory=" + lib), lib, 300, lib.resolve("mvn.log"), null);
        return jar.toString();
    }

    /**
     * Runs the agent with no network but loopback (a fresh network namespace), an empty environment
     * (no credentials from the caller), and a time limit. Returns null on success, else the reason.
     */
    static String sandboxed(List<String> cmd, Path dir, int timeoutSec, Path log) throws Exception {
        List<String> full = new ArrayList<>(List.of("unshare", "-rn", "sh", "-c", "ip link set lo up && exec \"$@\"", "sh"));
        full.addAll(cmd);
        ProcessBuilder pb = new ProcessBuilder(full).directory(dir.toFile()).redirectErrorStream(true).redirectOutput(log.toFile());
        pb.environment().clear();
        pb.environment().put("PATH", "/usr/bin:/bin");
        Process p;
        try {
            p = pb.start();
        } catch (IOException e) {
            return "no sandbox (unshare): " + e.getMessage();
        }
        if (!p.waitFor(timeoutSec, TimeUnit.SECONDS)) {
            p.descendants().forEach(ProcessHandle::destroyForcibly);
            p.destroyForcibly();
            return "timeout after " + timeoutSec + "s";
        }
        if (p.exitValue() == 0) return null;
        return "exit " + p.exitValue() + ": " + lastError(log);
    }

    private static String lastError(Path log) throws IOException {
        List<String> lines = Files.readAllLines(log);
        for (int i = 0; i < lines.size(); i++)
            if (lines.get(i).matches("^(Exception|Caused by|.*Error:|.*Exception:).*")) {
                String l = lines.get(i);
                for (int j = i + 1; j < lines.size(); j++) if (lines.get(j).startsWith("Caused by")) l = lines.get(j);
                return l.length() > 300 ? l.substring(0, 300) : l;
            }
        return lines.isEmpty() ? "" : lines.get(lines.size() - 1);
    }

    /** javaHome, when set, runs the build tool on that JDK (old Gradle can't run on a new one). */
    static void exec(List<String> cmd, Path dir, int timeoutSec, Path log, String javaHome) throws Exception {
        ProcessBuilder pb = new ProcessBuilder(cmd).directory(dir.toFile()).redirectErrorStream(true).redirectOutput(log.toFile());
        if (javaHome != null && !javaHome.isBlank()) pb.environment().put("JAVA_HOME", javaHome);
        Process p = pb.start();
        if (!p.waitFor(timeoutSec, TimeUnit.SECONDS)) {
            p.destroyForcibly();
            throw new IOException(String.join(" ", cmd) + ": timeout");
        }
        if (p.exitValue() != 0) throw new IOException(String.join(" ", cmd.subList(0, 2)) + " failed, see " + log);
    }

    static String git(Path repo, String... args) {
        try {
            List<String> cmd = new ArrayList<>(List.of("git", "-C", repo.toString()));
            cmd.addAll(List.of(args));
            Process p = new ProcessBuilder(cmd).redirectErrorStream(true).start();
            String out;
            try (BufferedReader r = new BufferedReader(new InputStreamReader(p.getInputStream()))) {
                out = r.readLine();
            }
            return p.waitFor() == 0 ? out : null;
        } catch (Exception e) {
            return null;
        }
    }

    static String sha(String s) throws Exception {
        return HexFormat.of().formatHex(MessageDigest.getInstance("SHA-1").digest(s.getBytes(StandardCharsets.UTF_8)));
    }

    /** MCP over stdio: newline-delimited JSON-RPC 2.0. */
    static final class Mcp {
        private static final String[][] TOOLS = {
            {"spring.capture", "Run the capture levels for a Spring Boot repo and return its facts (beans, injection, entry points, proxies, properties, gaps).", "repo", "service,commit,profiles,levels,run,timeout,force"},
            {"spring.beans", "Beans of a captured service.", "service", "type"},
            {"spring.injection", "Which bean fills each injection point (symbol like com.x.OrderService#<init>(0:gateway)), plus unresolved ones.", "service", "symbol"},
            {"spring.entrypoints", "Framework-called entry points: REST, schedules, Kafka/RocketMQ listeners, events, XXL-JOB.", "service", ""},
            {"spring.proxies", "Runtime proxies and what they mean: Feign = REST call, repository/mapper = table, @Async/@Transactional.", "service", ""},
            {"spring.properties", "Resolved non-secret properties.", "service", "key"},
            {"spring.questions", "Config keys whose production value is unknown and can change code paths or facts; answered only by a person with `spring-probe answer`.", "service", ""},
        };

        static void serve() throws IOException {
            PrintStream out = new PrintStream(new java.io.FileOutputStream(java.io.FileDescriptor.out), true, StandardCharsets.UTF_8);
            System.setOut(System.err); // nothing but JSON-RPC may reach stdout
            BufferedReader in = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
            for (String line; (line = in.readLine()) != null; ) {
                if (line.isBlank()) continue;
                Map<String, Object> resp = handle(line);
                if (resp != null) out.println(Json.write(resp).replace("\n", ""));
            }
        }

        @SuppressWarnings("unchecked")
        static Map<String, Object> handle(String line) {
            Map<String, Object> req;
            try {
                req = (Map<String, Object>) Json.parse(line);
            } catch (RuntimeException e) {
                return Json.map("jsonrpc", "2.0", "id", null, "error", Json.map("code", -32700, "message", "parse error"));
            }
            Object id = req.get("id");
            if (id == null) return null; // notification
            String method = s(req.get("method"));
            Map<String, Object> params = req.get("params") instanceof Map ? (Map<String, Object>) req.get("params") : Map.of();
            Object result = switch (method) {
                case "initialize" -> Json.map("protocolVersion", "2024-11-05", "capabilities", Json.map("tools", Map.of()),
                        "serverInfo", Json.map("name", "spring-probe", "version", "0.1.0"));
                case "ping" -> Map.of();
                case "tools/list" -> Json.map("tools", tools());
                case "tools/call" -> call(s(params.get("name")), params.get("arguments") instanceof Map ? (Map<String, Object>) params.get("arguments") : Map.of());
                default -> null;
            };
            if (result == null) return Json.map("jsonrpc", "2.0", "id", id, "error", Json.map("code", -32601, "message", "unknown method " + method));
            return Json.map("jsonrpc", "2.0", "id", id, "result", result);
        }

        private static List<Object> tools() {
            List<Object> l = new ArrayList<>();
            for (String[] t : TOOLS) {
                Map<String, Object> props = new LinkedHashMap<>();
                props.put(t[2], Json.map("type", "string"));
                for (String opt : t[3].split(",")) if (!opt.isEmpty()) props.put(opt, Json.map("type", "string"));
                l.add(Json.map("name", t[0], "description", t[1],
                        "inputSchema", Json.map("type", "object", "properties", props, "required", List.of(t[2]))));
            }
            return l;
        }

        private static Map<String, Object> call(String name, Map<String, Object> args) {
            Map<String, String> o = new LinkedHashMap<>();
            args.forEach((k, v) -> o.put(k, s(v)));
            try {
                Object r;
                if (name.equals("spring.capture")) {
                    o.put("arg0", o.get("repo"));
                    r = capture(o);
                } else {
                    o.put("arg0", o.get("service"));
                    if (o.containsKey("symbol")) o.put("arg1", o.get("symbol"));
                    r = read(name.substring("spring.".length()), o);
                }
                return Json.map("content", List.of(Json.map("type", "text", "text", Json.write(r))));
            } catch (Exception e) {
                return Json.map("content", List.of(Json.map("type", "text", "text", e.toString())), "isError", true);
            }
        }
    }
}
