package dev.weaver.springprobe;

import java.lang.annotation.Annotation;
import java.lang.reflect.Constructor;
import java.lang.reflect.Executable;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.lang.reflect.Modifier;
import java.security.CodeSource;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import org.springframework.beans.factory.config.DependencyDescriptor;
import org.springframework.core.DefaultParameterNameDiscoverer;
import org.springframework.core.MethodParameter;
import org.springframework.core.ResolvableType;
import org.springframework.core.env.Environment;

/** Reflection over app classes, shared by the aot, runtime and static levels. */
final class Model {
    static final String AUTOWIRED = "org.springframework.beans.factory.annotation.Autowired";
    static final String VALUE = "org.springframework.beans.factory.annotation.Value";
    static final String QUALIFIER = "org.springframework.beans.factory.annotation.Qualifier";
    static final String RESOURCE = "jakarta.annotation.Resource";
    static final String INJECT = "jakarta.inject.Inject";
    static final String BEAN = "org.springframework.context.annotation.Bean";

    private Model() {}

    /** One place where Spring injects a dependency. */
    record Point(String at, DependencyDescriptor dd, String resourceName, String paramQualifier, String methodQualifier) {
        /** The bean type asked for; for Map/Collection/array/ObjectProvider/Optional, the element type. */
        ResolvableType element() {
            ResolvableType t = dd.getResolvableType();
            Class<?> raw = t.resolve(Object.class);
            if (Map.class.isAssignableFrom(raw)) return t.asMap().getGeneric(1);
            if (java.util.Collection.class.isAssignableFrom(raw)) return t.asCollection().getGeneric(0);
            if (raw.isArray()) return t.getComponentType();
            if (raw.getName().equals("org.springframework.beans.factory.ObjectProvider") || raw == java.util.Optional.class) return t.getGeneric(0);
            return t;
        }

        boolean multi() {
            Class<?> raw = dd.getResolvableType().resolve(Object.class);
            return Map.class.isAssignableFrom(raw) || java.util.Collection.class.isAssignableFrom(raw) || raw.isArray();
        }
    }

    /** True for classes compiled into the app itself (a classes directory), not from a jar. */
    static boolean appClass(Class<?> c) {
        if (c == null) return false;
        CodeSource cs = c.getProtectionDomain().getCodeSource();
        return cs != null && cs.getLocation() != null && !cs.getLocation().getPath().endsWith(".jar");
    }

    static Annotation ann(java.lang.reflect.AnnotatedElement e, String fqn) {
        return ann(e.getAnnotations(), fqn);
    }

    /** A jakarta.* name also matches its javax.* original (Boot 2 apps). */
    static Annotation ann(Annotation[] as, String fqn) {
        String javax = fqn.startsWith("jakarta.") ? "javax." + fqn.substring(8) : null;
        for (Annotation a : as) {
            String n = a.annotationType().getName();
            if (n.equals(fqn) || n.equals(javax)) return a;
        }
        return null;
    }

    static Object attr(Annotation a, String name) {
        try {
            return a.annotationType().getMethod(name).invoke(a);
        } catch (ReflectiveOperationException e) {
            return null;
        }
    }

    static String str(Annotation a, String name) {
        Object v = a == null ? null : attr(a, name);
        if (v instanceof String[] arr) return arr.length == 0 ? null : String.join(",", arr);
        if (v instanceof Class<?>[] cs) return cs.length == 0 ? null : String.join(",", Arrays.stream(cs).map(Class::getName).toList());
        return v == null || v.toString().isEmpty() ? null : v.toString();
    }

    /** Injection points of a bean class: fields, the injecting constructor, @Autowired methods. */
    static List<Point> points(Class<?> c) {
        List<Point> out = new ArrayList<>();
        for (Class<?> k = c; k != null && k != Object.class; k = k.getSuperclass()) {
            for (Field f : k.getDeclaredFields()) {
                if (Modifier.isStatic(f.getModifiers()) || ann(f, VALUE) != null) continue;
                Annotation res = ann(f, RESOURCE);
                if (ann(f, AUTOWIRED) == null && ann(f, INJECT) == null && res == null) continue;
                String rn = res == null ? null : (str(res, "name") != null ? str(res, "name") : f.getName());
                out.add(new Point(c.getName() + "#" + f.getName(), new DependencyDescriptor(f, false), rn,
                        str(ann(f, QUALIFIER), "value"), null));
            }
        }
        Constructor<?>[] ctors = c.getDeclaredConstructors();
        Constructor<?> inject = null;
        for (Constructor<?> k : ctors) if (ann(k, AUTOWIRED) != null || ann(k, INJECT) != null) inject = k;
        if (inject == null && ctors.length == 1 && ctors[0].getParameterCount() > 0) inject = ctors[0];
        if (inject != null) out.addAll(params(c.getName() + "#<init>", inject));
        for (Method m : c.getDeclaredMethods())
            if (ann(m, AUTOWIRED) != null && m.getParameterCount() > 0) out.addAll(params(c.getName() + "#" + m.getName(), m));
        return out;
    }

    /** Injection points of a @Bean factory method's parameters. */
    static List<Point> params(String prefix, Executable e) {
        List<Point> out = new ArrayList<>();
        String mq = str(ann(e, QUALIFIER), "value");
        for (int i = 0; i < e.getParameterCount(); i++) {
            MethodParameter mp = MethodParameter.forExecutable(e, i);
            if (ann(mp.getParameterAnnotations(), VALUE) != null) continue;
            mp.initParameterNameDiscovery(new DefaultParameterNameDiscoverer());
            DependencyDescriptor dd = new DependencyDescriptor(mp, false);
            String name = mp.getParameterName() == null ? "arg" + i : mp.getParameterName();
            out.add(new Point(prefix + "(" + i + ":" + name + ")", dd, null,
                    str(ann(mp.getParameterAnnotations(), QUALIFIER), "value"), mq));
        }
        return out;
    }

    /** Entry points declared on a bean class, per the rules. env == null leaves ${...} unresolved. */
    @SuppressWarnings("unchecked")
    static List<Map<String, Object>> entrypoints(Class<?> c, Map<String, Object> rules, Environment env, String source) {
        List<Map<String, Object>> out = new ArrayList<>();
        List<Map<String, Object>> eps = (List<Map<String, Object>>) rules.get("entrypoints");
        String classPath = "";
        Annotation crm = ann(c, "org.springframework.web.bind.annotation.RequestMapping");
        if (crm != null) classPath = first(attr(crm, "path"), attr(crm, "value"));
        for (Map<String, Object> ep : eps) {
            String a = (String) ep.get("annotation");
            List<String> attrs = (List<String>) ep.getOrDefault("attrs", List.of());
            if (Boolean.TRUE.equals(ep.get("onClass"))) {
                Annotation ca = ann(c, a);
                if (ca == null) continue;
                String res = attrs.stream().map(x -> str(ca, x)).filter(x -> x != null).findFirst().orElse(null);
                out.add(entry((String) ep.get("kind"), res, c.getName() + "#" + ep.get("method"), env, source, (String) ep.get("prefix")));
                continue;
            }
            for (Method m : c.getDeclaredMethods()) {
                Annotation ma = ann(m, a);
                if (ma == null) continue;
                String res;
                if ("rest".equals(ep.get("kind"))) {
                    if (m.getName().contains("lambda$")) continue;
                    String verb = (String) ep.get("method");
                    if (verb == null) {
                        Object ms = attr(ma, "method");
                        verb = ms instanceof Object[] arr && arr.length > 0 ? arr[0].toString() : "ANY";
                    }
                    res = verb + " " + join(resolve(classPath, env), resolve(first(attr(ma, "path"), attr(ma, "value")), env));
                } else {
                    res = attrs.stream().map(x -> str(ma, x)).filter(x -> x != null).findFirst().orElse(null);
                    if (res == null && Boolean.TRUE.equals(ep.get("paramType")) && m.getParameterCount() > 0)
                        res = m.getParameterTypes()[0].getName();
                    if (res == null && "schedule".equals(ep.get("kind"))) res = String.valueOf(attr(ma, "fixedRate"));
                }
                out.add(entry((String) ep.get("kind"), res, c.getName() + "#" + m.getName(), env, source, (String) ep.get("prefix")));
            }
        }
        return out;
    }

    private static Map<String, Object> entry(String kind, String res, String symbol, Environment env, String source, String prefix) {
        Map<String, Object> e = new LinkedHashMap<>();
        e.put("kind", kind);
        String r = resolve(res, env);
        e.put("resource", prefix == null || r == null ? r : prefix + r);
        e.put("symbol", symbol);
        if (r != null && r.contains("${")) e.put("unresolved", true);
        e.put("source", source);
        return e;
    }

    static String resolve(String v, Environment env) {
        if (v == null || env == null) return v;
        try {
            return env.resolvePlaceholders(v);
        } catch (RuntimeException e) {
            return v;
        }
    }

    private static String first(Object... vs) {
        for (Object v : vs) if (v instanceof String[] a && a.length > 0) return a[0];
        return "";
    }

    private static String join(String a, String b) {
        String p = ("/" + a + "/" + b).replaceAll("/+", "/");
        return p.length() > 1 && p.endsWith("/") ? p.substring(0, p.length() - 1) : p;
    }

    /** What a framework proxy interface stands for: a REST client or a table. null if it isn't one. */
    static Map<String, Object> proxy(Class<?> iface, Environment env, String source) {
        if (!iface.isInterface()) return null;
        Annotation feign = ann(iface, "org.springframework.cloud.openfeign.FeignClient");
        if (feign != null) {
            Map<String, Object> p = base(iface, "rest", source);
            String name = str(feign, "name") != null ? str(feign, "name") : str(feign, "value");
            p.put("target", resolve(name, env));
            p.put("url", resolve(str(feign, "url"), env));
            String prefix = resolve(str(feign, "path"), env);
            List<String> calls = new ArrayList<>();
            for (Method m : iface.getDeclaredMethods()) {
                for (Map.Entry<String, String> v : VERBS.entrySet()) {
                    Annotation a = ann(m, v.getKey());
                    if (a == null) continue;
                    String verb = v.getValue();
                    if (verb == null) {
                        Object ms = attr(a, "method");
                        verb = ms instanceof Object[] arr && arr.length > 0 ? arr[0].toString() : "GET";
                    }
                    calls.add(verb + " " + join(prefix == null ? "" : prefix, resolve(first(attr(a, "path"), attr(a, "value")), env)) + " ← " + m.getName());
                }
            }
            p.put("calls", calls);
            unresolved(p);
            return p;
        }
        Class<?> entity = generic(iface, "org.springframework.data.repository.Repository", 0);
        String kind = "spring-data";
        if (entity == null) {
            entity = generic(iface, "com.baomidou.mybatisplus.core.mapper.BaseMapper", 0);
            kind = "mybatis-plus";
        }
        if (entity == null && ann(iface, "org.apache.ibatis.annotations.Mapper") != null) {
            Map<String, Object> p = base(iface, "table", source);
            p.put("framework", "mybatis");
            p.put("resource", null);
            p.put("note", "table is in the SQL of the mapper XML or @Select; not read");
            return p;
        }
        if (entity == null) return null;
        Map<String, Object> p = base(iface, "table", source);
        p.put("framework", kind);
        p.put("entity", entity.getName());
        String table = str(ann(entity, "jakarta.persistence.Table"), "name");
        if (table == null) table = str(ann(entity, "com.baomidou.mybatisplus.annotation.TableName"), "value");
        if (table == null) table = snake(entity.getSimpleName()); // Boot's default naming: CamelCase -> snake_case
        p.put("resource", table);
        return p;
    }

    private static final Map<String, String> VERBS = new LinkedHashMap<>();
    static {
        VERBS.put("org.springframework.web.bind.annotation.GetMapping", "GET");
        VERBS.put("org.springframework.web.bind.annotation.PostMapping", "POST");
        VERBS.put("org.springframework.web.bind.annotation.PutMapping", "PUT");
        VERBS.put("org.springframework.web.bind.annotation.DeleteMapping", "DELETE");
        VERBS.put("org.springframework.web.bind.annotation.PatchMapping", "PATCH");
        VERBS.put("org.springframework.web.bind.annotation.RequestMapping", null);
    }

    private static Map<String, Object> base(Class<?> iface, String means, String source) {
        Map<String, Object> p = new LinkedHashMap<>();
        p.put("type", iface.getName());
        p.put("means", means);
        p.put("source", source);
        return p;
    }

    private static void unresolved(Map<String, Object> p) {
        if (p.toString().contains("${")) p.put("unresolved", true);
    }

    private static Class<?> generic(Class<?> iface, String superName, int idx) {
        try {
            Class<?> sup = Class.forName(superName, false, iface.getClassLoader());
            if (!sup.isAssignableFrom(iface)) return null;
            return ResolvableType.forClass(iface).as(sup).getGeneric(idx).resolve();
        } catch (ClassNotFoundException e) {
            return null;
        }
    }

    static String snake(String s) {
        return s.replaceAll("([a-z0-9])([A-Z])", "$1_$2").toLowerCase();
    }

    /** Methods whose calls go through an AOP proxy (async, transactional) on a bean class. */
    static List<Map<String, Object>> aop(Class<?> c, String source) {
        List<Map<String, Object>> out = new ArrayList<>();
        boolean classTx = ann(c, "org.springframework.transaction.annotation.Transactional") != null;
        boolean classAsync = ann(c, "org.springframework.scheduling.annotation.Async") != null;
        for (Method m : c.getDeclaredMethods()) {
            if (!Modifier.isPublic(m.getModifiers()) || m.isSynthetic()) continue;
            List<String> what = new ArrayList<>();
            if (classAsync || ann(m, "org.springframework.scheduling.annotation.Async") != null) what.add("async");
            if (classTx || ann(m, "org.springframework.transaction.annotation.Transactional") != null) what.add("transactional");
            if (ann(m, "org.springframework.cache.annotation.Cacheable") != null) what.add("cacheable");
            if (what.isEmpty()) continue;
            Map<String, Object> p = new LinkedHashMap<>();
            p.put("type", c.getName() + "#" + m.getName());
            p.put("means", String.join("+", what));
            p.put("note", "call passes through a proxy to the same method" + (what.contains("async") ? "; runs on another thread" : ""));
            p.put("source", source);
            out.add(p);
        }
        return out;
    }

    static String typeName(ResolvableType t) {
        return t.toString().replace("?", "Object");
    }

    static List<String> sorted(List<String> l) {
        String[] a = l.toArray(String[]::new);
        Arrays.sort(a);
        return List.of(a);
    }
}
