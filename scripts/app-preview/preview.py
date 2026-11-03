#!/usr/bin/env python3
"""Preview the pick-list MCP App in headless Chrome, with this script playing
the host.

It runs the built server over stdio as a host would: initialize declaring
io.modelcontextprotocol/ui, resources/read of the view, check_list, then
get_store_layout with the stops, the call the view makes. It writes a host
page that loads the view in an iframe under the spec's default CSP, answers
ui/initialize, sends the tool input and result, and answers the view's
get_store_layout call from the recorded response. Chrome then screenshots it.

    go build -o /tmp/snag-mcp ./cmd/snag-mcp
    scripts/app-preview/preview.py /tmp/snag-mcp out/ mentone \\
        '{"store":"6400","location":"3194","items":[{"item_number":"0563516"}]}' \\
        --width 720 --theme dark --click '[data-floor="1"]' --hover 2 --fail-layout

Options after the arguments: --width N (default 720), --theme light|dark,
--click SELECTOR (repeatable, clicked in order inside the view),
--hover STOP (hover a stop's line), --fail-layout (the layout call fails,
to see the PNG fallback), --record DIR (reuse results recorded by an earlier
run in DIR instead of calling Bunnings; the binary is then not run, and
`go run ./scripts/example-store -demo DIR` writes such a recording for the
synthetic store).

Writes <name>.html, <name>.png and <name>.log (the messages the host saw).
Needs Google Chrome; set CHROME to its binary elsewhere than macOS.
"""
import json, os, subprocess, sys

CHROME = os.environ.get("CHROME", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
HERE = os.path.dirname(os.path.abspath(__file__))


def opts(argv):
    o = {"width": 720, "theme": "light", "click": [], "hover": 0, "fail": False, "record": None}
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--width": o["width"] = int(argv[i + 1]); i += 1
        elif a == "--theme": o["theme"] = argv[i + 1]; i += 1
        elif a == "--click": o["click"].append(argv[i + 1]); i += 1
        elif a == "--hover": o["hover"] = int(argv[i + 1]); i += 1
        elif a == "--fail-layout": o["fail"] = True
        elif a == "--record": o["record"] = argv[i + 1]; i += 1
        else: raise SystemExit("unknown option " + a)
        i += 1
    return o


def record(binary, args):
    """Talk to the server the way a host does; return view, check_list and layout results."""
    p = subprocess.Popen([binary], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
    nid = [0]

    def call(method, params):
        nid[0] += 1
        p.stdin.write(json.dumps({"jsonrpc": "2.0", "id": nid[0], "method": method, "params": params}) + "\n")
        p.stdin.flush()
        while True:
            m = json.loads(p.stdout.readline())
            if m.get("id") == nid[0]:
                if "error" in m:
                    raise SystemExit(f"{method}: {m['error']}")
                return m["result"]

    call("initialize", {"protocolVersion": "2025-11-25", "clientInfo": {"name": "app-preview", "version": "1"},
                        "capabilities": {"extensions": {"io.modelcontextprotocol/ui": {"mimeTypes": ["text/html;profile=mcp-app"]}}}})
    p.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
    p.stdin.flush()
    tools = call("tools/list", {})["tools"]
    check = next(t for t in tools if t["name"] == "check_list")
    uri = check["_meta"]["ui"]["resourceUri"]
    view = call("resources/read", {"uri": uri})["contents"][0]
    res = call("tools/call", {"name": "check_list", "arguments": args})
    layout = None
    sc = res.get("structuredContent") or {}
    if sc.get("store_map"):
        largs = {"store_id": sc["pick_list"]["store"]["store_id"], "map_id": sc["store_map"]["map_id"],
                 "stops": [s["address"] for s in sc["store_map"]["stops"]]}
        layout = {"arguments": largs, "result": call("tools/call", {"name": "get_store_layout", "arguments": largs})}
    p.stdin.close()
    p.wait()
    return {"tool": check, "view": view, "arguments": args, "result": res, "layout": layout}


def main():
    if len(sys.argv) < 5:
        raise SystemExit(__doc__)
    binary, out, name, args = sys.argv[1], sys.argv[2], sys.argv[3], json.loads(sys.argv[4])
    o = opts(sys.argv[5:])
    os.makedirs(out, exist_ok=True)
    rec_path = os.path.join(o["record"] or out, name + ".json")
    if o["record"]:
        rec = json.load(open(rec_path))
    else:
        rec = record(binary, args)
        json.dump(rec, open(rec_path, "w"))
    case = {"rec": rec, "theme": o["theme"], "width": o["width"], "click": o["click"], "hover": o["hover"], "fail": o["fail"]}
    page = open(os.path.join(HERE, "host.html")).read().replace("/*CASE*/null", json.dumps(case).replace("</", "<\\/"))
    html = os.path.join(os.path.abspath(out), name + ".html")
    open(html, "w").write(page)

    def chrome(*extra):
        return subprocess.run([CHROME, "--headless=new", "--hide-scrollbars", "--disable-gpu", "--no-first-run",
                               "--virtual-time-budget=8000", "--force-device-scale-factor=2", *extra, "file://" + html],
                              capture_output=True, text=True, timeout=120)
    # First pass for the height the view reports and the message log.
    dom = chrome(f"--window-size={o['width'] + 24},1200", "--dump-dom").stdout
    height, log = 1200, ""
    if 'id="log"' in dom:
        log = dom.split('id="log">', 1)[1].split("</pre>", 1)[0]
        for line in log.splitlines():
            if line.startswith(("height ", "measured ")):
                height = int(line.split()[1])
    open(os.path.join(out, name + ".log"), "w").write(log)
    case["height"] = height - 2
    open(html, "w").write(open(os.path.join(HERE, "host.html")).read().replace("/*CASE*/null", json.dumps(case).replace("</", "<\\/")))
    png = os.path.join(os.path.abspath(out), name + ".png")
    chrome(f"--window-size={o['width'] + 24},{height + 24}", f"--screenshot={png}")
    print(png)


main()
