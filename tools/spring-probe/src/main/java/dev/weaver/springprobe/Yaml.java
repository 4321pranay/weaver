package dev.weaver.springprobe;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

// ponytail: tiny block-style YAML, for the same reason as Json: no dependency that could clash with
// the target app's classpath. Writes what weaver's files look like (every string double-quoted, so
// `no` or `1.10` stay strings); reads block maps and lists, quoted and plain scalars, comments, and
// JSON-style flow values. Enough for answers.yaml and weaver's input, not a general YAML parser.
final class Yaml {
    private Yaml() {}

    static String write(Object v) {
        StringBuilder b = new StringBuilder();
        if (v instanceof Map<?, ?> m && !m.isEmpty() || v instanceof List<?> l && !l.isEmpty()) block(b, v, 0);
        else b.append(out(v)).append('\n');
        return b.toString();
    }

    private static void block(StringBuilder b, Object v, int ind) {
        String pad = " ".repeat(ind);
        if (v instanceof Map<?, ?> m) {
            for (Map.Entry<?, ?> e : m.entrySet()) {
                b.append(pad).append(key(String.valueOf(e.getKey()))).append(':');
                value(b, e.getValue(), ind + 2);
            }
        } else if (v instanceof List<?> l) {
            for (Object x : l) {
                if (x instanceof Map<?, ?> m && !m.isEmpty()) {
                    // "- k: v" with the map's other keys aligned under the first
                    StringBuilder inner = new StringBuilder();
                    block(inner, m, ind + 2);
                    b.append(pad).append("- ").append(inner.substring(ind + 2));
                } else {
                    b.append(pad).append('-');
                    value(b, x, ind + 2);
                }
            }
        }
    }

    private static void value(StringBuilder b, Object v, int ind) {
        if (v instanceof Map<?, ?> m && !m.isEmpty() || v instanceof List<?> l && !l.isEmpty()) {
            b.append('\n');
            block(b, v, ind);
        } else {
            b.append(' ').append(out(v)).append('\n');
        }
    }

    private static String out(Object v) {
        if (v == null) return "null";
        if (v instanceof Number || v instanceof Boolean) return v.toString();
        if (v instanceof Map<?, ?>) return "{}";
        if (v instanceof List<?>) return "[]";
        return Json.write(v.toString()).trim(); // a JSON string is a valid YAML double-quoted scalar
    }

    private static String key(String k) {
        return k.matches("[A-Za-z0-9_][A-Za-z0-9_.\\-/]*") ? k : out(k);
    }

    // ---- reading ----

    private record Line(int ind, String text, int no) {}

    static Object parse(String s) {
        List<Line> lines = new ArrayList<>();
        String[] raw = s.split("\n", -1);
        for (int n = 0; n < raw.length; n++) {
            String l = stripComment(raw[n]).replaceAll("\\s+$", "");
            if (l.isBlank() || l.equals("---")) continue;
            int ind = 0;
            while (ind < l.length() && l.charAt(ind) == ' ') ind++;
            lines.add(new Line(ind, l.substring(ind), n + 1));
        }
        if (lines.isEmpty()) return new LinkedHashMap<String, Object>();
        int[] at = {0};
        Object v = node(lines, at, lines.get(0).ind());
        if (at[0] != lines.size()) throw new IllegalArgumentException("yaml line " + lines.get(at[0]).no() + ": unexpected indentation");
        return v;
    }

    private static Object node(List<Line> ls, int[] at, int ind) {
        Line first = ls.get(at[0]);
        if (!isMapEntry(first.text()) && !first.text().startsWith("-")) { // a bare value: {}, [], "x"
            at[0]++;
            return scalar(first.text());
        }
        if (first.text().startsWith("- ") || first.text().equals("-")) {
            List<Object> out = new ArrayList<>();
            while (at[0] < ls.size() && ls.get(at[0]).ind() == ind && (ls.get(at[0]).text().startsWith("- ") || ls.get(at[0]).text().equals("-"))) {
                Line l = ls.get(at[0]);
                String rest = l.text().equals("-") ? "" : l.text().substring(2).trim();
                if (rest.isEmpty()) {
                    at[0]++;
                    out.add(at[0] < ls.size() && ls.get(at[0]).ind() > ind ? node(ls, at, ls.get(at[0]).ind()) : null);
                } else if (isMapEntry(rest)) {
                    // "- k: v": a map whose keys sit at the column after "- "
                    ls.set(at[0], new Line(ind + 2 + (l.text().length() - 2 - l.text().substring(2).stripLeading().length()), rest, l.no()));
                    out.add(node(ls, at, ls.get(at[0]).ind()));
                } else {
                    at[0]++;
                    out.add(scalar(rest));
                }
            }
            return out;
        }
        Map<String, Object> out = new LinkedHashMap<>();
        while (at[0] < ls.size() && ls.get(at[0]).ind() == ind) {
            Line l = ls.get(at[0]);
            if (!isMapEntry(l.text())) throw new IllegalArgumentException("yaml line " + l.no() + ": expected key: value");
            int c = colon(l.text());
            String k = unquote(l.text().substring(0, c).trim());
            String rest = l.text().substring(c + 1).trim();
            at[0]++;
            if (!rest.isEmpty()) out.put(k, scalar(rest));
            else if (at[0] < ls.size() && (ls.get(at[0]).ind() > ind || ls.get(at[0]).ind() == ind && ls.get(at[0]).text().startsWith("-")))
                out.put(k, node(ls, at, ls.get(at[0]).ind()));
            else out.put(k, null);
        }
        return out;
    }

    private static boolean isMapEntry(String t) {
        return colon(t) > 0;
    }

    /** The index of the ':' that ends a key (outside quotes, followed by space or end), or -1. */
    private static int colon(String t) {
        char q = 0;
        for (int i = 0; i < t.length(); i++) {
            char ch = t.charAt(i);
            if (q != 0) {
                if (ch == '\\' && q == '"') i++;
                else if (ch == q) q = 0;
            } else if (ch == '"' || ch == '\'') {
                if (i == 0) q = ch;
            } else if (ch == ':' && (i + 1 == t.length() || t.charAt(i + 1) == ' ')) {
                return i;
            } else if (i == 0 && (ch == '[' || ch == '{')) {
                return -1;
            }
        }
        return -1;
    }

    private static Object scalar(String t) {
        if (t.startsWith("\"") || t.startsWith("[") || t.startsWith("{")) return Json.parse(t);
        if (t.startsWith("'")) return unquote(t);
        return switch (t) {
            case "null", "~" -> null;
            case "true" -> true;
            case "false" -> false;
            default -> t; // plain scalars stay strings: a cron, a version, "no" are values, not numbers
        };
    }

    private static String unquote(String t) {
        if (t.length() >= 2 && t.startsWith("'") && t.endsWith("'")) return t.substring(1, t.length() - 1).replace("''", "'");
        if (t.length() >= 2 && t.startsWith("\"")) return (String) Json.parse(t);
        return t;
    }

    private static String stripComment(String l) {
        char q = 0;
        for (int i = 0; i < l.length(); i++) {
            char ch = l.charAt(i);
            if (q != 0) {
                if (ch == '\\' && q == '"') i++;
                else if (ch == q) q = 0;
            } else if (ch == '"' || ch == '\'') q = ch;
            else if (ch == '#' && (i == 0 || l.charAt(i - 1) == ' ')) return l.substring(0, i);
        }
        return l;
    }
}
