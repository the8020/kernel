#!/usr/bin/env python3
"""Build the development workspace prototype or run its disposable checks."""

import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
go_command = os.environ.get("THE8020_BUILD_GO", str(ROOT / ".development/toolchains/go/bin/go"))

if (len(sys.argv) not in (2, 3)
        or sys.argv[1] not in ("runtime", "fuse", "races", "git", "coherence", "native", "upper", "sparse", "activation", "schema", "transaction", "cost", "rename", "prototype", "build")
        or (len(sys.argv) == 3 and sys.argv[2] not in ("current", "shared", "unpatched"))
        or (len(sys.argv) == 3 and sys.argv[2] == "unpatched" and sys.argv[1] != "sparse")):
    raise SystemExit("usage: run.py runtime|fuse|races|git|coherence|native|upper|sparse|activation|schema|transaction|cost|rename|prototype|build [current|shared|unpatched]")
installed_build = sys.argv[1] == "build"
selected = "prototype" if installed_build else sys.argv[1]
profile = sys.argv[2] if len(sys.argv) == 3 else "current"
print("workflow runtime profile:", profile, flush=True)

env = os.environ.copy()
env.update(
    GOCACHE=str(ROOT / ".development/cache/go-build"),
    GOMODCACHE=str(ROOT / ".development/cache/go-mod"),
    GOWORK="off",
)
with tempfile.TemporaryDirectory(prefix="workflow-go-overlay-") as temporary:
    overlay = Path(temporary) / "overlay.json"
    replacements = {
        str(HERE.parent / "workflow_analysis_test.go"): str(HERE / "runtime_test.go"),
        str(HERE.parent / "workflow_fuse_test.go"): str(HERE / "fuse_test.go"),
        str(HERE.parent / "workflow_races_test.go"): str(HERE / "races_test.go"),
        str(HERE.parent / "workflow_coherence_test.go"): str(HERE / "coherence_test.go"),
        str(HERE.parent / "workflow_native_test.go"): str(HERE / "native_test.go"),
        str(HERE.parent / "workflow_upper_test.go"): str(HERE / "upper_test.go"),
    }
    if selected in ("transaction", "sparse", "activation", "schema", "cost", "rename", "prototype"):
        # Apply the shared transaction contract only to copied compiler inputs.
        # Keep the production worktree and its concurrent changes untouched.
        patch = HERE / "activation-transaction.patch"
        candidate_root = Path(temporary) / "transaction"
        for line in patch.read_text().splitlines():
            if line.startswith("--- a/"):
                relative = Path(line.removeprefix("--- a/"))
                destination = candidate_root / relative
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / relative, destination)
                replacements[str(ROOT / relative)] = str(destination)
        subprocess.run(["git", "apply", str(patch)], cwd=candidate_root, check=True)
        races = Path(temporary) / "races_test.go"
        races.write_text((HERE / "races_test.go").read_text()
                         .replace("Prepare(context.Context, []deployment.Candidate)", "Prepare(context.Context, string, []deployment.Candidate)")
                         .replace("Complete(context.Context, bool)", "Complete(context.Context, string, bool)"))
        replacements[str(HERE.parent / "workflow_races_test.go")] = str(races)
    if selected == "transaction":
        replacements[str(ROOT / "kernel/packages/workflow_transaction_test.go")] = str(HERE / "transaction_test.go")
    if selected in ("sparse", "activation", "schema", "cost", "rename", "prototype"):
        assert profile in ("current", "unpatched"), "sparse has its own disposable mount profile"
        # Preserve errors in the shared result before command/HTTP adapters
        # serialize it. A failed activation may not have package results yet.
        for relative, before, after in (
            ("manager.go", '\t\tif err != nil || !result.Success {',
             '\t\tif err != nil {\n\t\t\tresult.Error = err.Error()\n\t\t}\n\t\tif err != nil || !result.Success {'),
        ):
            source = HERE.parent / relative
            candidate = Path(replacements.get(str(source), source)).read_text()
            assert candidate.count(before) == 1, relative
            changed = Path(temporary) / relative
            changed.write_text(candidate.replace(before, after))
            replacements[str(source)] = str(changed)
        sys.dont_write_bytecode = True
        import gofer_probe
        env["WORKFLOW_SPARSE_SDK"] = gofer_probe.SDK
        build = gofer_probe.BUILD
        build.mkdir(parents=True, exist_ok=True)
        (build / "main.go").write_bytes((HERE / "gofer_probe.go").read_bytes())
        sdk_overlay, sdk_fixes = gofer_probe.sdk_overlay(setstat=profile != "unpatched")
        env["WORKFLOW_SPARSE_SDK_FIX"] = json.dumps(sdk_fixes)
        subprocess.run([
            go_command, "build", "-mod=mod", "-ldflags", gofer_probe.LINKER_FLAGS,
            "-overlay", str(sdk_overlay), "-o", "runsc", "main.go"], cwd=build, check=True,
            env=env | {"CGO_ENABLED": "0", "GOMODCACHE": str(ROOT / ".development/go-mod-cache")})
        source = HERE.parent / "rootless.go"
        candidate = source.read_text()
        for before, after in (
            ('"--directfs=true"', '"--directfs=false"'),
            ('func developmentSpec(', 'func analysisOriginalDevelopmentSpec('),
        ):
            assert candidate.count(before) == 1, before
            candidate = candidate.replace(before, after)
        changed = Path(temporary) / "rootless.go"
        changed.write_text(candidate)
        replacements[str(source)] = str(changed)
        replacements[str(HERE.parent / "workflow_sparse_test.go")] = str(HERE / "sparse_test.go")
        # read-tree creates an index without worktree stat data. Refresh clean
        # entries before add, which otherwise rewrites even unchanged objects
        # when the alternate store is read-only. Dirty paths still reach add.
        source = HERE.parent / "activation.go"
        candidate = Path(replacements.get(str(source), source)).read_text()
        before = '\tdefer func() {\n\t\tsandbox.LastActivationResult = &result'
        assert candidate.count(before) == 1
        candidate = candidate.replace(before, '\tdefer func() {\n\t\tif returnErr != nil {\n\t\t\tresult.Error = returnErr.Error()\n\t\t}\n\t\tsandbox.LastActivationResult = &result')
        before = '\t\t\treturnErr = errors.Join(returnErr, saveErr)'
        assert candidate.count(before) == 1
        candidate = candidate.replace(before, before + '\n\t\t\tresult.Error = returnErr.Error()')
        before = '\tGIT_INDEX_FILE="$activation_index" git -C "$activation_repository" read-tree "$activation_base" >/dev/null'
        assert candidate.count(before) == 1
        candidate = candidate.replace(before, before + '''
\tactivation_refresh_status=0
\tGIT_INDEX_FILE="$activation_index" git -C "$activation_repository" update-index --refresh >/dev/null || activation_refresh_status=$?
\tif [ "$activation_refresh_status" -gt 1 ]; then
\t\treturn "$activation_refresh_status"
\tfi''')
        if selected in ("activation", "schema", "cost", "rename", "prototype"):
            before = "func (m *Manager) Activate("
            assert candidate.count(before) == 1
            candidate = candidate.replace(before, "func (m *Manager) analysisLegacyActivate(")
            candidate = candidate.replace("func (m *Manager) Preview(", "func (m *Manager) analysisLegacyPreview(")
            replacements[str(HERE.parent / "workflow_sparse_activation_test.go")] = str(HERE / "sparse_activation_test.go")
            if selected == "cost":
                replacements[str(HERE.parent / "workflow_activation_cost_test.go")] = str(HERE / "activation_cost_test.go")
            if selected == "schema":
                replacements[str(HERE.parent / "workflow_sparse_schema_test.go")] = str(HERE / "sparse_schema_test.go")
        changed = Path(temporary) / "activation.go"
        changed.write_text(candidate)
        replacements[str(source)] = str(changed)
    if selected == "upper":
        source = HERE.parent / "rootless.go"
        candidate = source.read_text()
        before = '"--directfs=true"'
        assert candidate.count(before) == 1
        directfs = os.environ.get("WORKFLOW_UPPER_DIRECTFS", "true")
        assert directfs in ("true", "false")
        print("persistent-upper directfs:", directfs, flush=True)
        candidate = candidate.replace(before, '"--debug", "--directfs=' + directfs + '"')
        changed = Path(temporary) / "rootless.go"
        changed.write_text(candidate)
        replacements[str(source)] = str(changed)
    if profile == "shared":
        # Disposable default-profile experiment only: all:self would also overlay
        # extra writable bind mounts in a custom production profile.
        source = HERE.parent / "rootless.go"
        candidate = source.read_text()
        for before, after in (
            ('annotations := map[string]string{}', '''annotations := map[string]string{
                "dev.gvisor.spec.rootfs.source": start.RootFS,
                "dev.gvisor.spec.rootfs.type": "bind",
            }'''),
            ('annotations[prefix+"share"] = "container"', 'annotations[prefix+"share"] = "shared"'),
            ('"--overlay2=none"', '"--overlay2=all:self"'),
        ):
            assert candidate.count(before) == 1, before
            candidate = candidate.replace(before, after)
        changed = Path(temporary) / "rootless.go"
        changed.write_text(candidate)
        replacements[str(source)] = str(changed)
    if selected == "prototype":
        # The installer and disposable review use exactly the same owners.
        destination = (ROOT / ".development/bin" if installed_build else
                       Path(os.environ.get("WORKFLOW_PROTOTYPE_OUTPUT", str(ROOT / ".development/workflow-prototype")))).resolve()
        destination.mkdir(parents=True, exist_ok=True)
        if installed_build:
            # The runtime installer publishes runsc once under node/kernel/bin.
            (destination / "runsc").unlink(missing_ok=True)
        else:
            shutil.copy2(build / "runsc", destination / ".runsc-stage")
            os.replace(destination / ".runsc-stage", destination / "runsc")
        replacements = {source: target for source, target in replacements.items()
                        if not source.endswith("_test.go")}
        implementations = (
            ("sparse_test.go", "workflow_sparse.go", "func analysisSparseRuntime(",
             ["crypto/rand", "runtime", "strconv", "testing", "the8020/kernel/console", "the8020/kernel/sandbox/backend"]),
            ("sparse_activation_test.go", "workflow_sparse_activation.go", "type analysisActivationHook struct",
             ["crypto/sha256", "runtime", "testing", "the8020/kernel/console", "the8020/kernel/sandbox/backend"]),
        )
        for source, name, end, excluded in implementations:
            candidate = (HERE / source).read_text().split(end)[0]
            candidate = candidate.replace("//go:build workflowanalysis", "")
            if source == "sparse_test.go":
                begin = candidate.index("func (d *analysisSparseDriver) request(")
                finish = candidate.index("func (d *analysisSparseDriver) exchange(")
                candidate = candidate[:begin] + candidate[finish:]
            candidate = "\n".join(line for line in candidate.splitlines()
                                  if not any('"' + package + '"' in line for package in excluded))
            candidate = candidate.replace("m.driver.(*analysisSparseDriver)", "m.analysisSparseFor(sandbox.SandboxID)")
            target = Path(temporary) / name
            target.write_text(candidate)
            replacements[str(HERE.parent / name)] = str(target)
        target = Path(temporary) / "workflow_prototype.go"
        target.write_text((HERE / "prototype.go").read_text().replace("//go:build ignore", ""))
        replacements[str(HERE.parent / target.name)] = str(target)
        for name, transforms in {
            "manager.go": [
                ('m := &Manager{config: config, driver: config.Driver,', 'config.Driver = analysisPrototype(config)\n\tm := &Manager{config: config, driver: config.Driver,'),
                ('if err := os.RemoveAll(m.overlayRoot(sandbox)); err != nil {', 'if err := m.analysisResetWorkspace(&sandbox); err != nil {'),
            ],
            "overlay.go": [
                ('func (m *Manager) checkpointOverlayLocked(', 'func (m *Manager) analysisLegacyCheckpointOverlayLocked('),
                ('func (m *Manager) restoreOverlayLocked(', 'func (m *Manager) analysisLegacyRestoreOverlayLocked('),
            ],
        }.items():
            source = HERE.parent / name
            candidate = Path(replacements.get(str(source), source)).read_text()
            for before, after in transforms:
                assert candidate.count(before) == 1, before
                candidate = candidate.replace(before, after)
            if name == "overlay.go":
                candidate += '\nfunc (m *Manager) checkpointOverlayLocked(context.Context, Sandbox) error { return nil }\nfunc (m *Manager) restoreOverlayLocked(context.Context, *Sandbox) error { return nil }\n'
            target = Path(temporary) / name
            target.write_text(candidate)
            replacements[str(source)] = str(target)
    overlay.write_text(json.dumps({"Replace": replacements}))
    if selected == "prototype":
        go = go_command
        subprocess.run([go, "run", "./kernel/cbus/gen"], cwd=ROOT, env=env, check=True)
        gofmt = Path(subprocess.check_output([go, "env", "GOROOT"], env=env, text=True).strip()) / "bin/gofmt"
        subprocess.run([str(gofmt), "-w", *map(str, (ROOT / ".development/generated").rglob("*.go"))], check=True)
        for name in ("kernel", "admin", "logd"):
            cwd = ROOT if name == "logd" else ROOT / ".development/generated"
            package = "./kernel/logd" if name == "logd" else "./cmd/" + name
            subprocess.run([go, "build", "-mod=mod", "-trimpath", "-overlay", str(overlay),
                            "-o", str(destination / name), package], cwd=cwd, env=env, check=True)
        if installed_build:
            print("Built process-preserving development workspace:", destination, flush=True)
            raise SystemExit(0)
        sources = destination / "package-workspace"
        if sources.exists():
            shutil.rmtree(sources)
        for package in ROOT.parent.iterdir():
            if not (package / "package.toml").is_file():
                continue
            files = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=package).decode().split("\0")
            for relative in files:
                if not relative or any(part.startswith(".env") for part in Path(relative).parts):
                    continue
                source = package / relative
                if not source.is_file():
                    continue
                target = sources / package.name / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source, target)
        print("Prototype binaries:", destination, flush=True)
        raise SystemExit(0)
    if selected == "transaction":
        print("checking existing transaction owners", flush=True)
        subprocess.run([
            str(ROOT / ".development/toolchains/go/bin/go"), "test", "-overlay", str(overlay),
            "./kernel/packages", "./kernel/database/...", "./kernel/development", "./kernel/deployment",
            "./kernel/app", "./kernel/cbus/discovery", "./kernel/execution/programs",
            "-count=1", "-timeout=5m",
        ], cwd=ROOT, env=env, check=True)
    result = subprocess.run([
        str(ROOT / ".development/toolchains/go/bin/go"), "test",
        "-overlay", str(overlay), "-tags=workflowanalysis",
        "./kernel/packages" if selected == "transaction" else "./kernel/development", "-run", "^TestWorkflowAnalysis" + selected.title() + "$", "-v",
        "-count=1", "-timeout=10m",
    ], cwd=ROOT, env=env)
    if selected == "transaction" and result.returncode == 0:
        record = {
            "full_workflow_qualified": False,
            "activation_identity_regression_passed": True,
            "failed_rollback_retry_passed": True,
            "unstarted_preparation_abort_passed": True,
            "independent_catalog_deployments_passed": True,
            "independent_coordinator_activations_passed": True,
            "independent_evaluator_deployments_passed": True,
            "activation_operation_ownership_passed": True,
            "live_source_switch_excludes_recovery_passed": True,
            "shared_source_locks_across_processes_passed": True,
            "publication_index_retry_passed": True,
            "independent_attempt_recovery_passed": True,
            "published_package_snapshot_passed": True,
            "preparation_preserves_published_availability_passed": True,
            "independent_runtime_index_requests_passed": True,
            "newer_index_requests_retained_passed": True,
            "concurrent_revision_acknowledgements_passed": True,
            "periodic_revision_monitor_progress_passed": True,
            "bounded_background_indexing_passed": True,
            "independent_declaration_indexing_passed": True,
            "concurrent_declaration_publication_preserved": True,
            "concurrent_activation_qualified": False,
            "existing_owner_checks_passed": True,
            "source_switch_recovery_qualified": False,
            "postgresql_concurrency_qualified": False,
            "observed_at": datetime.now(timezone.utc).isoformat(),
            "source_sha256": {name: hashlib.sha256((HERE / name).read_bytes()).hexdigest()
                              for name in ["activation-transaction.patch", "transaction_test.go", "run.py"]},
        }
        (HERE / "transaction-results.json").write_text(json.dumps(record, indent=2) + "\n")
    raise SystemExit(result.returncode)
