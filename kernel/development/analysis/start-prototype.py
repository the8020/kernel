#!/usr/bin/env python3
"""Start the built prototype in its own persistent, disposable review instance."""

import os
from pathlib import Path
import shutil
import subprocess
import sys
import tomllib

ROOT = Path(__file__).resolve().parents[3]
BUILD = ROOT / ".development/workflow-prototype"
INSTANCE = Path("/tmp/8020-prototype-review")
RUNTIME = ROOT / ".development/named-terminal-test/node/kernel"
READY = INSTANCE / ".prototype-ready"

if len(sys.argv) != 1:
    raise SystemExit("usage: python3 kernel/development/analysis/start-prototype.py")
for required in (BUILD / "kernel", BUILD / "admin", BUILD / "logd", BUILD / "runsc",
                 BUILD / "package-workspace", RUNTIME / "runtime/images/rootless/image.json",
                 RUNTIME / "runtime/images/development/image.json"):
    if not required.exists():
        raise SystemExit(f"Missing prototype input: {required}; see PROTOTYPE.md")

if not READY.is_file():
    # Never initialize over an existing instance, including a partial setup.
    INSTANCE.mkdir(mode=0o700)
    print(f"Preparing review instance: {INSTANCE}", flush=True)
    subprocess.run([str(BUILD / "kernel"), "--root", str(INSTANCE),
                    "--init-defaults", "--init-only"], check=True)
    shutil.copytree(ROOT / "defaults/config/runtime",
                    INSTANCE / "node/kernel/runtime/definitions", dirs_exist_ok=True)
    subprocess.run(["bash", str(ROOT / "install-development-assets.sh"),
                    str(INSTANCE)], check=True)
    # Materialize once: gVisor mount destinations cannot pass through host symlinks.
    for relative in ("runtime/images/rootless", "runtime/images/development", "bin/gvisor-bin"):
        shutil.copytree(RUNTIME / relative, INSTANCE / "node/kernel" / relative,
                        symlinks=True, dirs_exist_ok=True)
    shutil.copy2(BUILD / "runsc", INSTANCE / "node/kernel/bin/runsc")
    packages = tomllib.loads((ROOT / "defaults/bootstrap-packages.toml").read_text())
    for package in packages["packages"]:
        package_id = package["id"]
        target = INSTANCE / "packages" / package_id
        shutil.copytree(BUILD / "package-workspace" / package_id.split("/")[1], target)
        git = ["git", "-C", str(target)]
        subprocess.run([*git, "init", "-q", "-b", "main"], check=True)
        subprocess.run([*git, "add", "--all"], check=True)
        subprocess.run([*git, "-c", "user.name=Prototype review", "-c",
                        "user.email=prototype@the8020.local", "-c", "commit.gpgsign=false",
                        "commit", "-qm", "Prototype review package sources"], check=True)
    READY.touch(mode=0o600)

print("Starting prototype: http://127.0.0.1:8082/ (SSH port 22222)", flush=True)
print("First login setup and shutdown commands: kernel/development/analysis/PROTOTYPE.md", flush=True)
os.execv(str(BUILD / "kernel"), [str(BUILD / "kernel"), "--root", str(INSTANCE),
         "--set", "network.main_port=8082", "--set", "network.ssh_port=22222",
         "--set", f"database.location={INSTANCE / 'database/system.db'}",
         "--set", "sandbox.runtime.mode=rootless", "--set", "sandbox.warm_pool.size=0"])
