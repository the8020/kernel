"""Run isolated stock-serializer and headless/browser state qualification."""

import html
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
DENO = os.environ.get("DENO_COMMAND", str(ROOT / ".development/runtime/development/rootfs/usr/bin/deno"))
BROWSER = os.environ.get("TERMINAL_PROBE_BROWSER", shutil.which("chromium") or "chromium")
ENV = dict(os.environ, DENO_DIR=str(ROOT / ".development/cache/deno"))


def run(arguments, *, output=None):
    result = subprocess.run(arguments, cwd=ROOT, env=ENV, check=True, capture_output=True, text=True, timeout=60)
    if output is not None:
        output.write_text(result.stdout)
    return result.stdout


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--state-module", type=Path)
    arguments = parser.parse_args()
    state_name = "production" if arguments.state_module else "prototype"
    browser_name = "production-browser" if arguments.state_module else "browser"
    run([DENO, "run", "--no-config", "--no-lock", str(HERE / "terminal_state_probe.ts")],
        output=HERE / "terminal-state-results.json")
    with tempfile.TemporaryDirectory(prefix="the8020-terminal-state-") as temporary:
        directory = Path(temporary)
        imports = []
        if arguments.state_module:
            mapping = directory / "imports.json"
            mapping.write_text(json.dumps({"imports": {
                (HERE / "xterm_state.ts").as_uri(): arguments.state_module.resolve().as_uri()
            }}))
            imports = [f"--import-map={mapping}"]
        fixtures = directory / "fixtures.json"
        run([DENO, "run", "--no-config", "--no-lock", *imports, f"--allow-write={fixtures}",
             str(HERE / "terminal_state_probe.ts"), "--state", f"--fixtures={fixtures}"],
            output=HERE / f"terminal-state-{state_name}-results.json")
        run([DENO, "bundle", "--no-config", "--no-lock", *imports, "--platform", "browser", "--output",
             str(directory / "probe.js"), str(HERE / "terminal_state_browser_probe.ts")])
        page = directory / "probe.html"
        data = fixtures.read_text().replace("<", "\\u003c")
        page.write_text('<!doctype html><body><script>globalThis.terminalCases=' + data +
                        ';</script><script type="module" src="./probe.js"></script></body>')
        markup = run([BROWSER, "--headless", "--no-sandbox", "--disable-gpu", "--no-proxy-server",
                      "--allow-file-access-from-files", "--virtual-time-budget=10000", "--dump-dom", page.as_uri()])
        match = re.search(r'<pre id="result">(.*?)</pre>', markup, re.S)
        if not match:
            raise RuntimeError("Chromium did not produce a terminal-state result")
        browser = json.loads(html.unescape(match[1]))
        browser["browser"] = run([BROWSER, "--version"]).strip()
        (HERE / f"terminal-state-{browser_name}-results.json").write_text(json.dumps(browser, indent=2) + "\n")
    for name in ["terminal-state-results.json", f"terminal-state-{state_name}-results.json", f"terminal-state-{browser_name}-results.json"]:
        result = json.loads((HERE / name).read_text())
        print(f"{name}: {result['equal']}/{result['total']} equal continuations")
    # Stock mismatches characterize the rejected approach; prototype mismatches
    # fail this development check. This is not the complete Phase 1 gate.
    for name in [f"terminal-state-{state_name}-results.json", f"terminal-state-{browser_name}-results.json"]:
        result = json.loads((HERE / name).read_text())
        if result["equal"] != result["total"]:
            raise RuntimeError(f"state qualification failed: {name}")


if __name__ == "__main__":
    main()
