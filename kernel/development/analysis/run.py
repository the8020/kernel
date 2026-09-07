#!/usr/bin/env python3
"""Run opt-in workflow experiments against production code in disposable roots."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]

if sys.argv[1:] not in (["runtime"], ["fuse"], ["races"]):
    raise SystemExit("usage: run.py runtime|fuse|races")
selected = sys.argv[1]

env = os.environ.copy()
env.update(
    GOCACHE=str(ROOT / ".development/cache/go-build"),
    GOMODCACHE=str(ROOT / ".development/cache/go-mod"),
    GOWORK="off",
)
with tempfile.TemporaryDirectory(prefix="workflow-go-overlay-") as temporary:
    overlay = Path(temporary) / "overlay.json"
    overlay.write_text(json.dumps({"Replace": {
        str(HERE.parent / "workflow_analysis_test.go"): str(HERE / "runtime_test.go"),
        str(HERE.parent / "workflow_fuse_test.go"): str(HERE / "fuse_test.go"),
        str(HERE.parent / "workflow_races_test.go"): str(HERE / "races_test.go"),
    }}))
    result = subprocess.run([
        str(ROOT / ".development/toolchains/go/bin/go"), "test",
        "-overlay", str(overlay), "-tags=workflowanalysis",
        "./kernel/development", "-run", "^TestWorkflowAnalysis" + selected.title() + "$", "-v",
        "-count=1", "-timeout=10m",
    ], cwd=ROOT, env=env)
    raise SystemExit(result.returncode)
