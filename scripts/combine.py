"""Join the docs into one Markdown file for a single PDF; links between docs become in-PDF jumps.
usage: combine.py OUT.md (run in docs/)"""
import re, sys
order = ["Overview", "weaver-architecture", "weaver-features", "analyzer-spec", "edges-spec", "questions-spec", "build-spec", "mcp-spec",
         "falcon-architecture", "rca-spec", "drift-spec"]
parts = []
for i, n in enumerate(order):
    t = open(n + ".md", encoding="utf-8").read()
    # "Related documents" tables only list the PDF's own contents
    t = re.sub(r"\*\*Related documents\*\*\s*\n\n(\|[^\n]*\n)+", "", t)
    if n == "Overview":  # its document table only repeats the PDF's own contents
        t = re.sub(r"\n---\s*\n+## 4\. Documents.*", "\n", t, flags=re.S)
    t = re.sub(r"\]\(((?:" + "|".join(order) + r"))\.md(#[^)]*)?\)", lambda m: "](#doc-" + m.group(1) + ")", t)
    # documents follow each other with a rule, not a page break: no half-empty pages
    mark = "" if i == 0 else '<div class="doc"></div>\n\n'
    parts.append(mark + '<a id="doc-%s"></a>\n\n' % n + t)
open(sys.argv[1], "w", encoding="utf-8").write("\n\n".join(parts))
