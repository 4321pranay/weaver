"""Score spring-probe output against sample/TRUTH.json, per level and merged.
usage: python3 test/score.py [service]   (after: spring-probe capture sample/shop)"""
import json, os, sys

svc = sys.argv[1] if len(sys.argv) > 1 else "shop"
cache = os.path.expanduser(f"~/.cache/spring-probe/{svc}")
truth = json.load(open(os.path.join(os.path.dirname(__file__), "..", "sample", "TRUTH.json")))["cases"]
outs = {lv: os.path.join(cache, "work", lv + ".json") for lv in ("aot", "runtime", "static")}
outs = {lv: json.load(open(p)) for lv, p in outs.items() if os.path.exists(p)}
outs["merged"] = json.load(open(os.path.join(cache, "latest.json")))
raw = {lv: json.dumps(d) for lv, d in outs.items()}


def check(c, d, lv):
    """-> (ok, what the probe said)"""
    if "inject" in c:
        f = next((x for x in d["injection"] if x["at"] == c["inject"]), None)
        if f is None:
            g = next((x for x in d["gaps"] if x.get("at") == c["inject"]), None)
            return False, f"gap {[s.split('.')[-1] for s in g.get('candidates', [])]}" if g else "-"
        if "beans" in c:
            return f.get("beans") == c["beans"], str(f.get("beans"))
        return f.get("impl") == c["impl"], f.get("impl", "").split(".")[-1]
    if "proxy" in c:
        f = next((x for x in d["proxies"] if x["type"] == c["proxy"]), None)
        if f is None:
            return False, "-"
        bad = {k: f.get(k) for k, v in c["expect"].items() if f.get(k) != v}
        return not bad, "ok" if not bad else str(bad)
    if "entry" in c:
        f = next((x for x in d["entrypoints"] if x["symbol"] == c["entry"]), None)
        return (f is not None and f["resource"] == c["resource"]), f["resource"] if f else "-"
    if "gap" in c:
        f = next((x for x in d["gaps"] if x.get("at") == c["gap"]), None)
        return f is not None, f["reason"] if f else "-"
    if "absentBean" in c:
        present = any(b["name"] == c["absentBean"] for b in d["beans"])
        return not present, "present" if present else "absent"
    if "secrets" in c:
        leaked = [s for s in c["secrets"] if s in raw[lv]]
        return not leaked, "leaked " + str(leaked) if leaked else "none"
    if "property" in c:
        f = next((x for x in d["properties"] if x["key"] == c["property"]), None)
        return (f is not None and f["value"] == c["value"]), f["value"] if f else "-"


levels = list(outs)
print(f"| case | problem | {' | '.join(levels)} |")
print("|---|---|" + "---|" * len(levels))
score = {lv: 0 for lv in levels}
for c in truth:
    cells = []
    for lv in levels:
        ok, said = check(c, outs[lv], lv)
        score[lv] += ok
        cells.append(("✅ " if ok else "❌ ") + said)
    print(f"| {c['id']} | {c['problem']} | {' | '.join(cells)} |")
print(f"| | **score** | {' | '.join(f'**{score[lv]}/{len(truth)}**' for lv in levels)} |")
