package dev.weaver.springprobe;

import java.lang.reflect.Method;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import org.springframework.aot.hint.RuntimeHints;
import org.springframework.beans.factory.annotation.AnnotatedBeanDefinition;
import org.springframework.beans.factory.config.BeanDefinition;
import org.springframework.beans.factory.support.DefaultListableBeanFactory;
import org.springframework.beans.factory.support.RootBeanDefinition;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.SpringApplicationRunListener;
import org.springframework.context.ConfigurableApplicationContext;
import org.springframework.context.annotation.ClassPathScanningCandidateComponentProvider;
import org.springframework.context.support.GenericApplicationContext;
import org.springframework.core.ResolvableType;
import org.springframework.core.env.ConfigurableEnvironment;
import org.springframework.core.env.EnumerablePropertySource;
import org.springframework.core.env.PropertySource;
import org.springframework.core.type.filter.AnnotationTypeFilter;
import org.springframework.core.type.filter.AssignableTypeFilter;
import org.springframework.core.type.filter.TypeFilter;
import org.springframework.scheduling.config.ScheduledTaskHolder;
import org.springframework.util.ClassUtils;

/**
 * Runs inside a child JVM on the target app's classpath and writes one level's facts as JSON.
 * Usage: Agent aot|runtime|static mainClass rules.json out.json [profiles]
 */
public final class Agent {
    private final Map<String, Object> rules;
    private final String source;
    final List<Map<String, Object>> beans = new ArrayList<>();
    final List<Map<String, Object>> injection = new ArrayList<>();
    final List<Map<String, Object>> entrypoints = new ArrayList<>();
    final List<Map<String, Object>> proxies = new ArrayList<>();
    final List<Map<String, Object>> properties = new ArrayList<>();
    final List<Map<String, Object>> gaps = new ArrayList<>();
    final Map<String, Map<String, Object>> questions = new LinkedHashMap<>();
    /** Production values the user answered (spring-probe answer); applied like command-line properties. */
    private Map<String, Object> answers = Map.of();

    private Agent(Map<String, Object> rules, String source) {
        this.rules = rules;
        this.source = source;
    }

    @SuppressWarnings("unchecked")
    // SpringAsmInfo.ASM_VERSION read at run time: the constant would be compiled in, and Spring 5's ASM
    // rejects the newer API level of the Spring this jar was built against
    private static final int ASM = asm();
    private static int asm() {
        try {
            return (int) org.springframework.asm.SpringAsmInfo.class.getField("ASM_VERSION").get(null);
        } catch (ReflectiveOperationException e) {
            throw new IllegalStateException(e);
        }
    }

    public static void main(String[] a) throws Exception {
        Class<?> main = Class.forName(a[1]);
        Map<String, Object> rules = (Map<String, Object>) Json.parse(Files.readString(Path.of(a[2])));
        String profiles = a.length > 4 ? a[4] : "";
        Agent ag = new Agent(rules, a[0]);
        if (a.length > 5) ag.answers = (Map<String, Object>) Json.parse(Files.readString(Path.of(a[5])));
        switch (a[0]) {
            case "aot" -> Aot.run(ag, main, profiles);
            case "runtime" -> ag.runtime(main, profiles);
            case "static" -> ag.staticModel(main);
            default -> throw new IllegalArgumentException("level " + a[0]);
        }
        Map<String, Object> out = new LinkedHashMap<>();
        out.put("beans", ag.beans);
        out.put("injection", ag.injection);
        out.put("entrypoints", ag.entrypoints);
        out.put("proxies", ag.proxies);
        out.put("properties", ag.properties);
        out.put("gaps", ag.gaps);
        out.put("questions", new ArrayList<>(ag.questions.values()));
        Files.writeString(Path.of(a[3]), Json.write(out));
        System.exit(0); // non-daemon threads of the app (pools, clients) must not keep the JVM alive
    }

    // ---- level 1: AOT processing (bean definitions, conditions evaluated, nothing instantiated) ----

    // Boot 3 only (AbandonedRunException, refreshForAotProcessing): kept in a class of its own so that
    // loading Agent on a Boot 2 classpath, for the static model, doesn't need them.
    private static final class Aot {
    static void run(Agent ag, Class<?> main, String profiles) {
        System.setProperty("spring.aot.processing", "true");
        SpringApplication app = new SpringApplication(main);
        ConfigurableApplicationContext[] holder = new ConfigurableApplicationContext[1];
        try {
            SpringApplication.withHook(application -> new SpringApplicationRunListener() {
                @Override
                public void contextLoaded(ConfigurableApplicationContext ctx) {
                    holder[0] = ctx;
                    throw new SpringApplication.AbandonedRunException(ctx);
                }
            }, () -> app.run(ag.args(profiles, Map.of())));
        } catch (SpringApplication.AbandonedRunException expected) {
            // stopped right after bean definitions were loaded, before refresh
        }
        GenericApplicationContext ctx = (GenericApplicationContext) holder[0];
        ctx.refreshForAotProcessing(new RuntimeHints());
        ag.collect((DefaultListableBeanFactory) ctx.getBeanFactory(), ctx.getEnvironment(), false);
    }
    }

    // ---- level 2: full start with infrastructure stubbed, then read what was really wired ----

    @SuppressWarnings("unchecked")
    private void runtime(Class<?> main, String profiles) throws Exception {
        Map<String, Object> stubs = new LinkedHashMap<>((Map<String, Object>) rules.get("stubs"));
        stubs.putAll(listenerSwitches(main));
        if (Model.ann(main, "org.springframework.cloud.netflix.eureka.server.EnableEurekaServer") != null) {
            // a Eureka server needs the client's beans: keep the client, but alone (no peers, no registry fetch)
            stubs.remove("eureka.client.enabled");
            stubs.remove("spring.cloud.discovery.enabled");
            stubs.put("eureka.client.register-with-eureka", "false");
            stubs.put("eureka.client.fetch-registry", "false");
        }
        SpringApplication app = new SpringApplication(main);
        ConfigurableApplicationContext ctx = app.run(args(profiles, stubs));
        DefaultListableBeanFactory bf = (DefaultListableBeanFactory) ctx.getBeanFactory();
        collect(bf, ctx.getEnvironment(), true);
        Set<String> seen = new HashSet<>();
        entrypoints.forEach(e -> seen.add(e.get("kind") + " " + e.get("resource")));
        for (ScheduledTaskHolder h : ctx.getBeansOfType(ScheduledTaskHolder.class).values())
            for (var t : h.getScheduledTasks()) {
                String r = t.getTask().toString();
                String res = t.getTask() instanceof org.springframework.scheduling.config.CronTask ct ? ct.getExpression() : r;
                if (seen.add("schedule " + res)) entrypoints.add(Json.map("kind", "schedule", "resource", res, "symbol", r, "source", source));
            }
        if (org.springframework.util.ClassUtils.isPresent("org.springframework.web.servlet.DispatcherServlet", Agent.class.getClassLoader()))
            Mvc.mappings(bf, seen, entrypoints, source);
        ctx.close();
    }

    // Spring MVC only: a class of its own, so Agent loads on a WebFlux or non-web classpath. The
    // annotations already give those apps' REST entry points; this adds what MVC really mapped.
    private static final class Mvc {
        static void mappings(DefaultListableBeanFactory bf, Set<String> seen, List<Map<String, Object>> entrypoints, String source) {
            for (String n : bf.getBeanNamesForType(org.springframework.web.servlet.mvc.method.annotation.RequestMappingHandlerMapping.class)) {
                var hm = (org.springframework.web.servlet.mvc.method.annotation.RequestMappingHandlerMapping) bf.getBean(n);
                hm.getHandlerMethods().forEach((info, h) -> {
                    String verb = info.getMethodsCondition().getMethods().stream().findFirst().map(Enum::name).orElse("ANY");
                    for (String p : info.getPatternValues()) {
                        String res = verb + " " + p;
                        if (Model.appClass(h.getBeanType()) && seen.add("rest " + res)) // not Boot's own /error
                            entrypoints.add(Json.map("kind", "rest", "resource", res, "symbol", h.getBeanType().getName() + "#" + h.getMethod().getName(), "source", source));
                    }
                });
            }
        }
    }

    /** Properties that stop listeners which have no global auto-startup switch, e.g. RocketMQ (rules: listenerSwitches). */
    @SuppressWarnings("unchecked")
    private Map<String, Object> listenerSwitches(Class<?> main) throws Exception {
        Map<String, Object> out = new LinkedHashMap<>();
        for (Map<String, Object> sw : (List<Map<String, Object>>) rules.getOrDefault("listenerSwitches", List.of())) {
            String ann = (String) sw.get("annotation");
            if (!ClassUtils.isPresent(ann, null)) continue;
            ClassPathScanningCandidateComponentProvider scan = new ClassPathScanningCandidateComponentProvider(false);
            scan.addIncludeFilter(new AnnotationTypeFilter(load(ann)));
            for (var bd : scan.findCandidateComponents(main.getPackageName())) {
                var a = Model.ann(Class.forName(bd.getBeanClassName()), ann);
                Matcher m = Pattern.compile("\\{(\\w+)}").matcher((String) sw.get("property"));
                StringBuilder key = new StringBuilder();
                while (m.find()) m.appendReplacement(key, Matcher.quoteReplacement(String.valueOf(Model.str(a, m.group(1)))));
                m.appendTail(key);
                out.put(key.toString(), sw.get("value"));
            }
        }
        return out;
    }

    /** Command-line properties: answered production values, then stubs (stubs win: the run never connects anywhere). */
    private String[] args(String profiles, Map<String, Object> stubs) {
        Map<String, Object> props = new LinkedHashMap<>(answers);
        props.putAll(stubs);
        List<String> a = new ArrayList<>();
        if (!profiles.isEmpty()) a.add("--spring.profiles.active=" + profiles);
        props.forEach((k, v) -> a.add("--" + k + "=" + v));
        return a.toArray(String[]::new);
    }

    /** Facts from a bean factory: AOT-processed (live=false) or fully started (live=true). */
    private void collect(DefaultListableBeanFactory bf, ConfigurableEnvironment env, boolean live) {
        Set<String> proxied = new HashSet<>();
        for (String name : bf.getBeanDefinitionNames()) {
            BeanDefinition bd = bf.getMergedBeanDefinition(name);
            Class<?> type = type(bf, name);
            if (type == null) continue;
            Class<?> factory = factoryClass(bf, bd);
            boolean app = Model.appClass(type) || Model.appClass(factory);
            if (!app) continue;
            beans.add(Json.map("name", name, "type", type.getName(), "primary", bd.isPrimary() ? true : null,
                    "via", factory != null ? factory.getName() + "#" + bd.getFactoryMethodName() : null, "source", source));
            if (type.isInterface()) {
                Map<String, Object> p = Model.proxy(type, env, source);
                if (p != null && proxied.add(type.getName())) proxies.add(p);
                continue;
            }
            List<Model.Point> pts = factory != null ? factoryPoints(factory, bd) : Model.points(type);
            for (Model.Point p : pts) injection(bf, name, p, live);
            if (factory == null) {
                entrypoints.addAll(Model.entrypoints(type, rules, env, source));
                proxies.addAll(Model.aop(type, source));
            }
        }
        properties(env);
    }

    private static Class<?> type(DefaultListableBeanFactory bf, String name) {
        try {
            Class<?> t = bf.getType(name, false);
            return t == null ? null : ClassUtils.getUserClass(t);
        } catch (RuntimeException | LinkageError e) {
            return null;
        }
    }

    private static Class<?> factoryClass(DefaultListableBeanFactory bf, BeanDefinition bd) {
        if (bd.getFactoryMethodName() == null) return null;
        if (bd.getFactoryBeanName() != null) return type(bf, bd.getFactoryBeanName());
        return bd instanceof RootBeanDefinition r && r.hasBeanClass() ? r.getBeanClass() : null;
    }

    private static List<Model.Point> factoryPoints(Class<?> factory, BeanDefinition bd) {
        if (bd instanceof RootBeanDefinition r && r.getResolvedFactoryMethod() != null)
            return Model.params(factory.getName() + "#" + bd.getFactoryMethodName(), r.getResolvedFactoryMethod());
        for (Method m : factory.getDeclaredMethods())
            if (m.getName().equals(bd.getFactoryMethodName())) return Model.params(factory.getName() + "#" + m.getName(), m);
        return List.of();
    }

    private void injection(DefaultListableBeanFactory bf, String owner, Model.Point p, boolean live) {
        ResolvableType el = p.element();
        List<String> cands;
        if (live) {
            // what Spring actually injected: the owner's registered dependencies of the asked type
            cands = new ArrayList<>();
            for (String d : bf.getDependenciesForBean(owner)) {
                if (!bf.containsBean(d)) continue;
                Class<?> t = type(bf, d);
                if (t != null && el.isAssignableFrom(t)) cands.add(d);
            }
            if (!p.multi() && cands.size() > 1) cands = narrow(bf, p, cands);
        } else {
            cands = candidates(bf, owner, p, el);
        }
        String at = p.at();
        if (cands.isEmpty()) {
            if (!Model.appClass(el.resolve())) return; // framework-provided (ApplicationContext...), not a bean
            gaps.add(Json.map("kind", "implementation", "at", at, "type", Model.typeName(el), "candidates", List.of(), "reason", "no bean of this type", "source", source));
            return;
        }
        if (!p.multi() && cands.size() > 1) {
            gaps.add(Json.map("kind", "implementation", "at", at, "type", Model.typeName(el), "candidates", impls(bf, cands), "source", source));
            return;
        }
        // skip plain framework infrastructure (DataSource, ObjectMapper...) unless it's an app type
        if (!Model.appClass(el.resolve()) && cands.stream().noneMatch(c -> Model.appClass(type(bf, c)))) return;
        Map<String, Object> m = Json.map("at", at, "type", Model.typeName(el));
        if (p.multi()) {
            m.put("beans", Model.sorted(cands));
            m.put("impls", impls(bf, cands));
        } else {
            m.put("bean", cands.get(0));
            m.put("impl", impls(bf, cands).get(0));
        }
        m.put("source", source);
        injection.add(m);
    }

    /** Spring's own choice without creating beans: candidates by type and generics, qualifiers, @Resource name, @Primary, then name. */
    private static List<String> candidates(DefaultListableBeanFactory bf, String owner, Model.Point p, ResolvableType el) {
        if (p.resourceName() != null && bf.containsBean(p.resourceName())) return List.of(p.resourceName());
        List<String> out = new ArrayList<>();
        for (String n : bf.getBeanNamesForType(el, true, false)) {
            if (n.equals(owner)) continue;
            boolean ok;
            try {
                ok = p.multi() || bf.isAutowireCandidate(n, p.dd());
            } catch (RuntimeException e) {
                ok = true;
            }
            if (!ok) continue;
            if (p.multi()) {
                String q = p.paramQualifier() != null ? p.paramQualifier() : p.methodQualifier();
                if (q != null && !qualifies(bf, n, q)) continue;
            }
            out.add(n);
        }
        return p.multi() || out.size() < 2 ? out : narrow(bf, p, out);
    }

    private static List<String> narrow(DefaultListableBeanFactory bf, Model.Point p, List<String> cands) {
        List<String> prim = cands.stream().filter(n -> bf.containsBeanDefinition(n) && bf.getMergedBeanDefinition(n).isPrimary()).toList();
        if (prim.size() == 1) return prim;
        String dn = p.dd().getDependencyName();
        if (dn != null && cands.contains(dn)) return List.of(dn);
        return cands;
    }

    private static boolean qualifies(DefaultListableBeanFactory bf, String n, String q) {
        if (n.equals(q)) return true;
        Class<?> t = type(bf, n);
        return t != null && q.equals(Model.str(Model.ann(t, Model.QUALIFIER), "value"));
    }

    private static List<String> impls(DefaultListableBeanFactory bf, List<String> names) {
        List<String> out = new ArrayList<>();
        for (String n : names) {
            Class<?> t = type(bf, n);
            out.add(t == null ? n : t.getName());
        }
        return out;
    }

    @SuppressWarnings("unchecked")
    private void properties(ConfigurableEnvironment env) {
        List<String> secrets = (List<String>) rules.get("secretPatterns");
        Map<String, String> renamed = (Map<String, String>) rules.get("renamedProperties");
        Set<String> keys = new TreeSet<>();
        List<PropertySource<?>> own = new ArrayList<>();
        for (PropertySource<?> ps : env.getPropertySources()) {
            String n = ps.getName();
            // only the app's own config: not the machine's env or JVM properties, not our stubs
            if (!(ps instanceof EnumerablePropertySource<?> e) || !n.startsWith("Config resource")) continue;
            own.add(ps);
            keys.addAll(Arrays.asList(e.getPropertyNames()));
        }
        Pattern secret = Pattern.compile(".*(" + String.join("|", secrets) + ").*", Pattern.CASE_INSENSITIVE);
        for (String k : keys) {
            for (Map.Entry<String, String> r : renamed.entrySet())
                if (k.startsWith(r.getKey()))
                    gaps.add(Json.map("kind", "property", "at", k, "reason", "not read by Spring Boot 3: use "
                            + (r.getValue().endsWith(".") ? r.getValue() + k.substring(r.getKey().length()) : r.getValue()), "source", source));
            String last = k.substring(k.lastIndexOf('.') + 1);
            if (secret.matcher(last).matches()) continue; // never written anywhere
            String v;
            try {
                Object raw = own.stream().map(ps -> ps.getProperty(k)).filter(x -> x != null).findFirst().orElse(null);
                v = raw == null ? null : env.resolvePlaceholders(raw.toString());
            } catch (RuntimeException e) {
                v = null;
            }
            if (v == null) continue;
            properties.add(Json.map("key", k, "value", v, "source", source));
        }
    }

    // ---- level 3: static Spring model, no context: component scan + annotation rules ----

    private void staticModel(Class<?> main) throws Exception {
        String base = main.getPackageName();
        Scanner comps = new Scanner(false, List.of(new AnnotationTypeFilter(org.springframework.stereotype.Component.class)));
        List<TypeFilter> ifaceFilters = new ArrayList<>();
        for (String f : List.of("org.springframework.cloud.openfeign.FeignClient", "org.apache.ibatis.annotations.Mapper"))
            ifaceFilters.addAll(filter(f, n -> new AnnotationTypeFilter(load(n))));
        for (String f : List.of("org.springframework.data.repository.Repository", "com.baomidou.mybatisplus.core.mapper.BaseMapper"))
            ifaceFilters.addAll(filter(f, n -> new AssignableTypeFilter(load(n))));
        Scanner ifaces = new Scanner(true, ifaceFilters);

        record SBean(String name, Class<?> type, boolean primary, String via) {}
        List<SBean> sbeans = new ArrayList<>();
        Map<Class<?>, List<Model.Point>> pts = new LinkedHashMap<>();
        Map<String, List<Model.Point>> factoryPts = new LinkedHashMap<>();
        for (var bd : comps.findCandidateComponents(base)) {
            try {
            Class<?> c = Class.forName(bd.getBeanClassName());
            String name = Model.str(Model.ann(c, "org.springframework.stereotype.Component"), "value");
            if (name == null) name = Model.str(Model.ann(c, "org.springframework.stereotype.Service"), "value");
            if (name == null) name = java.beans.Introspector.decapitalize(c.getSimpleName());
            sbeans.add(new SBean(name, c, Model.ann(c, "org.springframework.context.annotation.Primary") != null, null));
            pts.put(c, Model.points(c));
            for (Method m : c.getDeclaredMethods()) {
                var bean = Model.ann(m, Model.BEAN);
                if (bean == null) continue;
                String bn = Model.str(bean, "name") != null ? Model.str(bean, "name").split(",")[0] : m.getName();
                sbeans.add(new SBean(bn, m.getReturnType(), Model.ann(m, "org.springframework.context.annotation.Primary") != null, c.getName() + "#" + m.getName()));
                factoryPts.put(bn, Model.params(c.getName() + "#" + m.getName(), m));
            }
            } catch (LinkageError | ClassNotFoundException e) {
                unloadable(bd.getBeanClassName(), e);
            }
        }
        for (var bd : ifaces.findCandidateComponents(base)) {
            try {
                Class<?> c = Class.forName(bd.getBeanClassName());
                Map<String, Object> p = Model.proxy(c, null, source);
                sbeans.add(new SBean(java.beans.Introspector.decapitalize(c.getSimpleName()), c, false, null));
                if (p != null) proxies.add(p);
            } catch (LinkageError | ClassNotFoundException e) {
                unloadable(bd.getBeanClassName(), e);
            }
        }
        for (SBean b : sbeans)
            beans.add(Json.map("name", b.name(), "type", b.type().getName(), "primary", b.primary() ? true : null, "via", b.via(), "source", source));

        List<Model.Point> all = new ArrayList<>();
        pts.values().forEach(all::addAll);
        factoryPts.values().forEach(all::addAll);
        for (Model.Point p : all) {
            ResolvableType el = p.element();
            List<SBean> c = sbeans.stream().filter(b -> el.isAssignableFrom(ResolvableType.forClass(b.type()))).toList();
            // CodeQL SpringAutowire precedence: param @Qualifier > method @Qualifier > @Resource(name) > by type
            String q = p.paramQualifier() != null ? p.paramQualifier() : p.methodQualifier();
            if (q != null) c = c.stream().filter(b -> b.name().equals(q) || q.equals(Model.str(Model.ann(b.type(), Model.QUALIFIER), "value"))).toList();
            else if (p.resourceName() != null && c.stream().anyMatch(b -> b.name().equals(p.resourceName())))
                c = c.stream().filter(b -> b.name().equals(p.resourceName())).toList();
            if (!p.multi() && c.size() > 1) {
                List<SBean> prim = c.stream().filter(SBean::primary).toList();
                if (prim.size() == 1) c = prim;
            }
            if (c.isEmpty()) {
                if (Model.appClass(el.resolve()))
                    gaps.add(Json.map("kind", "implementation", "at", p.at(), "type", Model.typeName(el), "candidates", List.of(), "reason", "no bean of this type in the scanned packages", "source", source));
                continue;
            }
            if (!p.multi() && c.size() > 1) {
                boolean cond = c.stream().anyMatch(b -> conditional(b.type()));
                gaps.add(Json.map("kind", "implementation", "at", p.at(), "type", Model.typeName(el), "candidates", c.stream().map(b -> b.type().getName()).toList(),
                        "reason", cond ? "candidates have @Profile/@Conditional; static reading doesn't evaluate them" : null, "source", source));
                continue;
            }
            Map<String, Object> m = Json.map("at", p.at(), "type", Model.typeName(el));
            if (p.multi()) {
                m.put("beans", Model.sorted(c.stream().map(SBean::name).toList()));
                m.put("impls", c.stream().map(b -> b.type().getName()).toList());
            } else {
                m.put("bean", c.get(0).name());
                m.put("impl", c.get(0).type().getName());
            }
            m.put("source", source);
            injection.add(m);
        }
        for (Class<?> c : pts.keySet()) {
            try {
                entrypoints.addAll(Model.entrypoints(c, rules, null, source));
                proxies.addAll(Model.aop(c, source));
                dynamicLookups(c);
            } catch (LinkageError e) {
                unloadable(c.getName(), e);
            }
        }
        List<Class<?>> conditioned = new ArrayList<>(pts.keySet());
        conditioned.addAll(appAutoConfigs());
        for (Class<?> c : conditioned) {
            try {
                branchPoints(c);
            } catch (LinkageError e) {
                unloadable(c.getName(), e);
            }
        }
        profileFiles();
        // every ${key} that ends up in a fact (topic, cron, Feign url, REST prefix): production may differ
        Pattern ph = Pattern.compile("\\$\\{([^}:]+)");
        List<Map<String, Object>> facts = new ArrayList<>(entrypoints);
        facts.addAll(proxies);
        for (Map<String, Object> f : facts) {
            Matcher m = ph.matcher(Json.write(f));
            String what = f.containsKey("symbol") ? f.get("kind") + " " + f.get("symbol") : f.get("means") + " " + f.get("type");
            while (m.find()) question(m.group(1), "value", what, null, null);
        }
        @SuppressWarnings("unchecked")
        List<String> addrs = (List<String>) rules.getOrDefault("addressProperties", List.of());
        for (String k : addrs) question(k, "address", "infrastructure address", null, null);
    }

    /** A class the static model can't load, usually for an optional dependency left off the runtime
     *  classpath: skipped, its beans and entry points unknown. */
    private void unloadable(String cls, Throwable e) {
        gaps.add(Json.map("kind", "class", "at", cls, "reason", "can't load: " + e, "source", source));
    }

    /** Config that picks between code paths: @Profile, @ConditionalOnProperty, @ConditionalOnExpression. */
    private void branchPoints(Class<?> c) {
        List<java.lang.reflect.AnnotatedElement> els = new ArrayList<>(List.of(c));
        for (Method m : c.getDeclaredMethods()) if (Model.ann(m, Model.BEAN) != null) els.add(m);
        for (var el : els) {
            String what = el instanceof Method m ? c.getName() + "#" + m.getName() : c.getName();
            var prof = Model.ann(el, "org.springframework.context.annotation.Profile");
            if (prof != null)
                for (String p : (String[]) Model.attr(prof, "value"))
                    question("spring.profiles.active", "profile", what, p.replaceAll("[!()&| ]", ""), null);
            var cop = Model.ann(el, "org.springframework.boot.autoconfigure.condition.ConditionalOnProperty");
            if (cop != null) {
                String prefix = Model.str(cop, "prefix");
                String[] names = (String[]) Model.attr(cop, "name");
                if (names.length == 0) names = (String[]) Model.attr(cop, "value");
                String having = Model.str(cop, "havingValue");
                boolean ifMissing = Boolean.TRUE.equals(Model.attr(cop, "matchIfMissing"));
                for (String n : names)
                    question(prefix == null ? n : prefix.replaceAll("\\.$", "") + "." + n, "condition", what,
                            having == null ? "(any value but false)" : having, ifMissing ? "(unset)" : null);
            }
            var coe = Model.ann(el, "org.springframework.boot.autoconfigure.condition.ConditionalOnExpression");
            if (coe != null) {
                Matcher m = Pattern.compile("\\$\\{([^}:]+)").matcher(Model.str(coe, "value"));
                while (m.find()) question(m.group(1), "condition", what, null, null);
            }
        }
    }

    @SuppressWarnings("unchecked")
    private void question(String key, String kind, String usedBy, String option, String option2) {
        List<String> secrets = (List<String>) rules.get("secretPatterns");
        String last = key.substring(key.lastIndexOf('.') + 1).toLowerCase();
        if (secrets.stream().anyMatch(last::contains)) return; // never asked, never stored
        Map<String, Object> q = questions.computeIfAbsent(key, k -> Json.map("key", k, "kind", kind, "options", new ArrayList<String>(), "usedBy", new ArrayList<String>()));
        List<String> opts = (List<String>) q.get("options");
        for (String o : new String[] {option, option2}) if (o != null && !opts.contains(o)) opts.add(o);
        List<String> used = (List<String>) q.get("usedBy");
        if (!used.contains(usedBy)) used.add(usedBy);
    }

    /** Profiles that have their own config file: application-<profile>.yml|yaml|properties. */
    private void profileFiles() throws java.io.IOException {
        var r = new org.springframework.core.io.support.PathMatchingResourcePatternResolver();
        for (var res : r.getResources("classpath*:application-*.*")) {
            if (res.getURL().getPath().contains(".jar!")) continue;
            String n = res.getFilename();
            if (n == null || !n.matches("application-.+\\.(yml|yaml|properties)")) continue;
            question("spring.profiles.active", "profile", n, n.substring(12, n.lastIndexOf('.')), null);
        }
    }

    /** The app's own auto-configurations (AutoConfiguration.imports in its classes, not in jars): outside the component scan. */
    private List<Class<?>> appAutoConfigs() throws Exception {
        List<Class<?>> out = new ArrayList<>();
        var urls = Agent.class.getClassLoader().getResources("META-INF/spring/org.springframework.boot.autoconfigure.AutoConfiguration.imports");
        while (urls.hasMoreElements()) {
            var u = urls.nextElement();
            if (u.getPath().contains(".jar!")) continue;
            try (var in = u.openStream()) {
                for (String l : new String(in.readAllBytes(), java.nio.charset.StandardCharsets.UTF_8).split("\\R"))
                    if (!l.isBlank() && !l.startsWith("#")) {
                        try {
                            out.add(Class.forName(l.trim()));
                        } catch (LinkageError | ClassNotFoundException e) {
                            unloadable(l.trim(), e);
                        }
                    }
            }
        }
        return out;
    }

    /** Calls that pick a bean or class by a runtime string: the entries walk can't follow them. */
    private void dynamicLookups(Class<?> c) throws java.io.IOException {
        String res = c.getName().replace('.', '/') + ".class";
        try (var in = c.getClassLoader().getResourceAsStream(res)) {
            if (in == null) return;
            new org.springframework.asm.ClassReader(in).accept(new org.springframework.asm.ClassVisitor(ASM) {
                @Override
                public org.springframework.asm.MethodVisitor visitMethod(int acc, String name, String desc, String sig, String[] ex) {
                    return new org.springframework.asm.MethodVisitor(ASM) {
                        int line;

                        @Override
                        public void visitLineNumber(int l, org.springframework.asm.Label start) { line = l; }

                        @Override
                        public void visitMethodInsn(int op, String owner, String m, String d, boolean itf) {
                            String what = null;
                            if (m.equals("getBean") && d.startsWith("(Ljava/lang/String;")) what = "bean looked up by a runtime name (getBean(String))";
                            else if (owner.equals("java/lang/Class") && m.equals("forName")) what = "class loaded by a runtime name (Class.forName)";
                            else if (owner.equals("java/lang/reflect/Method") && m.equals("invoke")) what = "reflective call (Method.invoke)";
                            if (what != null)
                                gaps.add(Json.map("kind", "entries", "at", c.getName() + "#" + name, "line", line, "reason", what, "source", source));
                        }
                    };
                }
            }, 0);
        }
    }

    private static boolean conditional(Class<?> c) {
        for (var a : c.getAnnotations()) {
            String n = a.annotationType().getName();
            if (n.equals("org.springframework.context.annotation.Profile") || a.annotationType().isAnnotationPresent(org.springframework.context.annotation.Conditional.class)) return true;
        }
        return false;
    }

    /** Component scan by include filters only: Spring's own scanner evaluates @Profile/@Conditional, which a static reading must not. */
    private static final class Scanner extends ClassPathScanningCandidateComponentProvider {
        private final boolean interfaces;
        private final List<TypeFilter> include;

        Scanner(boolean interfaces, List<TypeFilter> include) {
            super(false);
            this.interfaces = interfaces;
            this.include = include;
        }

        @Override
        protected boolean isCandidateComponent(org.springframework.core.type.classreading.MetadataReader r) throws java.io.IOException {
            for (TypeFilter f : include) if (f.match(r, getMetadataReaderFactory())) return true;
            return false;
        }

        @Override
        protected boolean isCandidateComponent(AnnotatedBeanDefinition bd) {
            var m = bd.getMetadata();
            return m.isIndependent() && (interfaces ? m.isInterface() : m.isConcrete());
        }
    }

    private static List<TypeFilter> filter(String cls, java.util.function.Function<String, TypeFilter> f) {
        return ClassUtils.isPresent(cls, null) ? List.of(f.apply(cls)) : List.of();
    }

    @SuppressWarnings("unchecked")
    private static <T> Class<T> load(String n) {
        try {
            return (Class<T>) Class.forName(n);
        } catch (ClassNotFoundException e) {
            throw new IllegalStateException(e);
        }
    }
}
