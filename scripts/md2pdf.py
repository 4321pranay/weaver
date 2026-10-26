"""Render Markdown files to PDF: markdown-it (CommonMark) -> HTML (Mermaid drawn by a
local mermaid.js) -> headless Chrome --print-to-pdf.
usage: md2pdf.py OUT_DIR FILE.md..."""
import html, os, re, subprocess, sys, tempfile
from markdown_it import MarkdownIt

MERMAID = "/opt/Antigravity/resources/app/node_modules/mermaid/dist/mermaid.min.js"
CHROME = os.path.expanduser("~/.cache/puppeteer/chrome-headless-shell/linux-153.0.8010.36/"
                            "chrome-headless-shell-linux64/chrome-headless-shell")
CSS = """
@page{size:A4;margin:8mm 9mm}
body{font-family:"DejaVu Sans",Arial,sans-serif;font-size:8.3pt;line-height:1.25;color:#1d2330;margin:0}
h1{font-size:15pt;border-bottom:2px solid #2f6fb3;padding-bottom:2px;margin:10px 0 6px}
h2{font-size:11.5pt;color:#2f6fb3;margin:10px 0 4px;break-after:avoid}
h3{font-size:9.8pt;margin:8px 0 3px;break-after:avoid}
p,ul,ol{margin:3px 0} li{margin:0}
hr{display:none}
table{border-collapse:collapse;width:100%;margin:4px 0;font-size:7.5pt}
th,td{border:1px solid #c9d1dd;padding:2px 4px;vertical-align:top;text-align:left}
th{background:#eef2f7}
code{font-family:"DejaVu Sans Mono",monospace;font-size:8.3pt;background:#f2f4f7;padding:0 2px}
pre{background:#f6f8fa;border:1px solid #e1e5eb;padding:4px 6px;margin:4px 0;white-space:pre-wrap;word-break:break-word;font-size:7.1pt;line-height:1.22}
pre code{background:none;padding:0}
pre.mermaid{background:none;border:none;text-align:center;padding:0;margin:6px 0}
pre.mermaid svg{max-height:52mm;max-width:100%;height:auto}
blockquote{border-left:3px solid #c9d1dd;margin:6px 0;padding:2px 10px;color:#5b6475}
a{color:#2f6fb3;text-decoration:none}
.doc{border-top:3px solid #2f6fb3;margin-top:14px;padding-top:1px}
"""

def render(md_path, out_dir):
    text = open(md_path, encoding="utf-8").read()
    blocks = []
    def keep(m):  # mermaid source must reach mermaid.js untouched
        blocks.append(m.group(1))
        return f"\n\nMERMAIDBLOCK{len(blocks)-1}\n\n"
    text = re.sub(r"```mermaid\n(.*?)```", keep, text, flags=re.S)
    body = MarkdownIt("commonmark").enable("table").render(text)  # CommonMark: lists nest as GitHub shows them
    for i, src in enumerate(blocks):
        body = body.replace(f"<p>MERMAIDBLOCK{i}</p>", f'<pre class="mermaid">{html.escape(src)}</pre>')
    page = f"""<!doctype html><html><head><meta charset="utf-8"><style>{CSS}</style>
<script src="file://{MERMAID}"></script></head><body>{body}
<script>mermaid.initialize({{startOnLoad:false, theme:"neutral", securityLevel:"loose", flowchart:{{useMaxWidth:true}}}});
mermaid.run().then(()=>document.title="done").catch(e=>{{document.title="done";console.error(e)}});</script></body></html>"""
    with tempfile.NamedTemporaryFile("w", suffix=".html", delete=False, encoding="utf-8") as f:
        f.write(page)
    out = os.path.join(out_dir, os.path.splitext(os.path.basename(md_path))[0] + ".pdf")
    subprocess.run([CHROME, "--headless", "--no-sandbox", "--disable-gpu", "--allow-file-access-from-files",
                    "--no-pdf-header-footer", "--virtual-time-budget=15000",
                    f"--print-to-pdf={out}", "file://" + f.name],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.unlink(f.name)
    print(out)

if __name__ == "__main__":
    os.makedirs(sys.argv[1], exist_ok=True)
    for p in sys.argv[2:]:
        render(p, sys.argv[1])
