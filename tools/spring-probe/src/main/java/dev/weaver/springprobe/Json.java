package dev.weaver.springprobe;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

// ponytail: tiny JSON so the jar has no dependencies that could clash with the target app's classpath.
final class Json {
    private final String s;
    private int i;

    private Json(String s) { this.s = s; }

    static Map<String, Object> map(Object... kv) {
        Map<String, Object> m = new LinkedHashMap<>();
        for (int i = 0; i < kv.length; i += 2) if (kv[i + 1] != null) m.put((String) kv[i], kv[i + 1]);
        return m;
    }

    static Object parse(String s) {
        Json j = new Json(s);
        Object v = j.value();
        j.ws();
        if (j.i != s.length()) throw new IllegalArgumentException("trailing data at " + j.i);
        return v;
    }

    static String write(Object v) {
        StringBuilder b = new StringBuilder();
        write(b, v, 0);
        return b.append('\n').toString();
    }

    private static void write(StringBuilder b, Object v, int ind) {
        if (v == null) b.append("null");
        else if (v instanceof String str) str(b, str);
        else if (v instanceof Number || v instanceof Boolean) b.append(v);
        else if (v instanceof Map<?, ?> m) {
            if (m.isEmpty()) { b.append("{}"); return; }
            b.append("{");
            String sep = "\n";
            for (Map.Entry<?, ?> e : m.entrySet()) {
                b.append(sep).append("  ".repeat(ind + 1));
                str(b, String.valueOf(e.getKey()));
                b.append(": ");
                write(b, e.getValue(), ind + 1);
                sep = ",\n";
            }
            b.append("\n").append("  ".repeat(ind)).append("}");
        } else if (v instanceof Iterable<?> l) {
            List<Object> items = new ArrayList<>();
            l.forEach(items::add);
            if (items.isEmpty()) { b.append("[]"); return; }
            boolean flat = items.stream().noneMatch(x -> x instanceof Map || x instanceof Iterable);
            b.append("[");
            for (int k = 0; k < items.size(); k++) {
                if (k > 0) b.append(flat ? ", " : ",");
                if (!flat) b.append("\n").append("  ".repeat(ind + 1));
                write(b, items.get(k), ind + 1);
            }
            if (!flat) b.append("\n").append("  ".repeat(ind));
            b.append("]");
        } else str(b, v.toString());
    }

    private static void str(StringBuilder b, String s) {
        b.append('"');
        for (char c : s.toCharArray()) {
            switch (c) {
                case '"' -> b.append("\\\"");
                case '\\' -> b.append("\\\\");
                case '\n' -> b.append("\\n");
                case '\r' -> b.append("\\r");
                case '\t' -> b.append("\\t");
                default -> {
                    if (c < 0x20) b.append(String.format("\\u%04x", (int) c));
                    else b.append(c);
                }
            }
        }
        b.append('"');
    }

    private void ws() { while (i < s.length() && Character.isWhitespace(s.charAt(i))) i++; }

    private Object value() {
        ws();
        char c = s.charAt(i);
        if (c == '{') {
            i++;
            Map<String, Object> m = new LinkedHashMap<>();
            ws();
            if (s.charAt(i) == '}') { i++; return m; }
            while (true) {
                ws();
                String k = string();
                ws();
                expect(':');
                m.put(k, value());
                ws();
                if (s.charAt(i++) == '}') return m;
            }
        }
        if (c == '[') {
            i++;
            List<Object> l = new ArrayList<>();
            ws();
            if (s.charAt(i) == ']') { i++; return l; }
            while (true) {
                l.add(value());
                ws();
                if (s.charAt(i++) == ']') return l;
            }
        }
        if (c == '"') return string();
        if (s.startsWith("true", i)) { i += 4; return true; }
        if (s.startsWith("false", i)) { i += 5; return false; }
        if (s.startsWith("null", i)) { i += 4; return null; }
        int st = i;
        while (i < s.length() && "+-0123456789.eE".indexOf(s.charAt(i)) >= 0) i++;
        String n = s.substring(st, i);
        if (n.isEmpty()) throw new IllegalArgumentException("bad JSON at " + st);
        return n.contains(".") || n.contains("e") || n.contains("E") ? (Object) Double.parseDouble(n) : (Object) Long.parseLong(n);
    }

    private void expect(char c) {
        if (s.charAt(i++) != c) throw new IllegalArgumentException("expected " + c + " at " + (i - 1));
    }

    private String string() {
        expect('"');
        StringBuilder b = new StringBuilder();
        while (true) {
            char c = s.charAt(i++);
            if (c == '"') return b.toString();
            if (c != '\\') { b.append(c); continue; }
            char e = s.charAt(i++);
            switch (e) {
                case 'n' -> b.append('\n');
                case 't' -> b.append('\t');
                case 'r' -> b.append('\r');
                case 'b' -> b.append('\b');
                case 'f' -> b.append('\f');
                case 'u' -> { b.append((char) Integer.parseInt(s.substring(i, i + 4), 16)); i += 4; }
                default -> b.append(e);
            }
        }
    }
}
