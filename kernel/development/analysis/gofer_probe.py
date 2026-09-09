#!/usr/bin/env python3
"""Qualify a limited Gofer copy-up transport in an isolated real sandbox."""

import hashlib
import errno
import json
import os
from pathlib import Path
import platform
import shutil
import shlex
import socket
import stat
import statistics
import subprocess
import sys
import tempfile
import time

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
SDK = "v0.0.0-20260815055033-7d8fb7f28de4"
BUILD = ROOT / ".development/workflow-gofer-build"


def sdk_overlay(setstat):
    """Share disposable SDK fixes without changing cached or installed inputs."""
    cached = ROOT / ".development/go-mod-cache" / ("gvisor.dev/gvisor@" + SDK)
    local = BUILD / ("sdk-" + SDK)
    if not local.exists():
        # Go forbids overlays under GOMODCACHE. Hardlinks avoid a second SDK copy.
        shutil.copytree(cached, local, copy_function=os.link, symlinks=True)
    (BUILD / "go.mod").write_text(
        "module the8020-workflow-gofer-probe\n\ngo 1.26.5\n\n"
        f"require gvisor.dev/gvisor {SDK}\n\nreplace gvisor.dev/gvisor => ./{local.name}\n")
    stat_needle = "// regular file data, so there's no cache to truncate either.)\n\t\t\treturn nil"
    directory_needle = ("\t\tif fd.controlFD.node.isDeleted() {\n\t\t\treturn unix.EINVAL\n\t\t}\n"
                        "\t\treturn fd.impl.Getdent64")
    symlink_needle = ("if fd.safelyRead(func() error {\n\t\tif fd.node.isDeleted() {\n"
                       "\t\t\treturn unix.EINVAL\n\t\t}\n\t\tn, err = fd.impl.Readlink")
    replacements, fixes = {}, {}
    for prefix, relative, needle, replacement in (
        ("", "pkg/sentry/fsimpl/gofer/gofer.go", stat_needle,
         stat_needle.replace("return nil", "return failureErr") if setstat else stat_needle),
        ("removed_directory_", "pkg/lisafs/handlers.go", directory_needle,
         directory_needle.replace("return unix.EINVAL", "return nil // Removed directories have no entries.")),
    ):
        source = local / relative
        before = source.read_text()
        assert before.count(needle) == 1, relative
        after = before.replace(needle, replacement)
        if relative == "pkg/lisafs/handlers.go":
            # Readlink uses the retained inode FD, including after unlink or
            # replacement. Both stock fsgofer and the sparse owner support it.
            assert after.count(symlink_needle) == 1, relative
            after = after.replace(symlink_needle, "if err := fd.safelyRead(func() error {\n\t\tn, err = fd.impl.Readlink")
        changed = BUILD / ("sdk-" + prefix + Path(relative).name)
        changed.write_text(after)
        replacements[str(source)] = str(changed)
        fixes.update({prefix + "file": relative,
                      prefix + "before_sha256": hashlib.sha256(before.encode()).hexdigest(),
                      prefix + "after_sha256": hashlib.sha256(after.encode()).hexdigest()})
    overlay = BUILD / "sdk-overlay.json"
    overlay.write_text(json.dumps({"Replace": replacements}))
    return overlay, fixes


def main():
    if len(sys.argv) > 2 or (len(sys.argv) == 2 and sys.argv[1] not in ("writes", "atomic", "namespace", "publication")):
        raise SystemExit("usage: gofer_probe.py [writes|atomic|namespace|publication]")
    mode = sys.argv[1] if len(sys.argv) == 2 else "transport"
    writes_only = mode in ("writes", "atomic")
    BUILD.mkdir(parents=True, exist_ok=True)
    overlay, sdk_fixes = sdk_overlay(setstat=False)
    shutil.copyfile(HERE / "gofer_probe.go", BUILD / "main.go")
    env = os.environ.copy()
    env.update(
        CGO_ENABLED="0", GOWORK="off",
        GOCACHE=str(ROOT / ".development/cache/go-build"),
        GOMODCACHE=str(ROOT / ".development/go-mod-cache"),
    )
    subprocess.run([
        str(ROOT / ".development/toolchains/go/bin/go"), "build", "-mod=mod",
        "-overlay", str(overlay), "-o", "runsc", "main.go",
    ], cwd=BUILD, env=env, check=True)
    subprocess.run([
        str(ROOT / ".development/toolchains/go/bin/go"), "build",
        "-o", str(BUILD / "native-client"), str(HERE / "native_probe.go"),
    ], cwd=BUILD, env=env, check=True)
    result = {
        "sdk": SDK, "sdk_fixes": sdk_fixes, "environment": platform.uname()._asdict(),
        "source_sha256": {
            name: hashlib.sha256((HERE / name).read_bytes()).hexdigest()
            for name in ("gofer_probe.go", "gofer_probe.py", "native_probe.go", "git_probe.py")
        },
        "checks": {}, "commands": [], "full_workflow_qualified": False,
        "mode": mode,
        "timing_boundary": ("create/write/file-sync/close/rename/open-parent/directory-sync/close"
                            if mode == "atomic" else "open/truncate/write/file-sync/close"
                            if mode == "writes" else "functional checks; command durations include runtime exec"),
        "unimplemented_gates": [
            "directory rename, retained symlink descriptors, full metadata and hardlink semantics",
            "transactional copy-up recovery and concurrent upstream in-place writes",
            "remaining lower-hardlink and detached lower-handle mutation cases",
            "private Git ownership, actual activation/schema/hooks and conflict installation",
            "automatic pending retirement, snapshot reclamation, durable publication recovery",
            "bounded enumeration, watcher behavior, concurrency and deployment profiles",
        ],
    }

    def check(name, passed, detail):
        result["checks"][name] = {"passed": bool(passed), "detail": detail}
        print(name, "PASS" if passed else "FAIL", detail, flush=True)

    with tempfile.TemporaryDirectory(prefix="workflow-gofer-", dir=BUILD) as temp:
        work = Path(temp)
        lower, storage = work / "shared", work / "storage"
        storage.mkdir()
        upper, base = storage / "upper", storage / "base"
        for directory in (lower, upper, base, storage / "deleted", storage / "snapshots", storage / "lower"):
            directory.mkdir()
        (lower / "label.txt").write_text("Old label\n")
        (lower / "untouched.txt").write_text("before\n")
        (lower / "handles.txt").write_bytes(b"before\n" + bytes(4089))
        (lower / "atomic.txt").write_text("Original atomic\n")
        (lower / "remove.txt").write_text("Original removed\n")
        (lower / "ignored").mkdir()
        (lower / "rename").mkdir()
        (lower / "renamed" / "directory").mkdir(parents=True)
        for name in ("new.txt", "replace.txt", "invalid.txt", "same.txt"):
            (lower / "rename" / name).write_text("shared rename\n")
        os.link(lower / "rename/same.txt", lower / "rename/alias.txt")
        (lower / "renamed/target.txt").write_text("old target\n")
        (lower / "links/directory").mkdir(parents=True)
        for name, target in {"from": "../../host-only-source", "target": "../../host-only-target", "remove": "../../host-only-removed", "keep": "../../host-only-kept"}.items():
            (lower / "links" / name).symlink_to(target)
        (lower / "nested/deep").mkdir(parents=True)
        for name in ("nested/remove.txt", "nested/deep/remove.txt"):
            (lower / name).write_text("original nested\n")
        for name in ("nested/keep.txt", "nested/deep/keep.txt"):
            (lower / name).write_text("untouched sibling\n")
        for name in ("shared-empty", "remove-shared/deep", "recreate-shared"):
            (lower / name).mkdir(parents=True)
        for name in ("remove-shared/deep/old.txt", "recreate-shared/old.txt"):
            (lower / name).write_text("original directory child\n")
        publish_body = b"first\n" + b"context\n" * 20 + b"last\n"
        for name in ("publish-clean.txt", "publish-late.txt", "publish-held.txt", "publish-mode.txt", "publish-recover.txt"):
            (lower / name).write_bytes(publish_body)
        (lower / "publish-mmap.txt").write_bytes(b"before\n" + bytes(4089))
        (lower / "reads").mkdir()
        for n in range(10):
            (lower / "reads" / str(n)).write_bytes(b"read working set\n")
        (lower / "label-link").symlink_to("label.txt")
        (work / "outside").write_text("host-only\n")
        (lower / "escape").symlink_to("../../outside")
        shutil.copyfile(BUILD / "runsc", lower / "client")
        (lower / "client").chmod(0o755)
        shutil.copyfile(BUILD / "native-client", lower / "native-client")
        (lower / "native-client").chmod(0o755)
        assets = lower / "assets"
        assets.mkdir()
        count = int(os.environ.get("WORKFLOW_GOFER_ASSETS", "2048" if mode == "transport" else "4"))
        if not 1 <= count <= 2048:
            raise ValueError("WORKFLOW_GOFER_ASSETS must be 1..2048")
        for n in range(count):
            (assets / f"{n:04d}.bin").write_bytes(os.urandom(1 << 20))
        result["assets"] = {
            "files": count, "logical_bytes": count << 20,
            "allocated_bytes": sum(p.stat().st_blocks * 512 for p in assets.iterdir()),
        }
        print("allocated asset fixture", result["assets"], flush=True)
        small_tree = lower / "small"
        small_tree.mkdir()
        small_count = 10000 if mode == "transport" else 0
        for n in range(small_count):
            (small_tree / f"{n:05d}.txt").write_bytes(b"small source file\n" * 10)
        result["small_tree"] = {"files": small_count, "logical_bytes": small_count * 180}
        if writes_only:
            for profile in ("sparse", "stock_rpc", "stock_directfs"):
                for size in (64, 65536, 1048576):
                    target = lower / "writes" / profile / str(size)
                    target.mkdir(parents=True)
                    for n in range(31):
                        (target / str(n)).write_bytes(b"\x7f" * size)
        for args in (["init", "-q"], ["config", "user.name", "Fixture"],
                     ["config", "user.email", "fixture@example.test"],
                     ["add", "label.txt", "untouched.txt", "atomic.txt", "remove.txt",
                      "publish-clean.txt", "publish-late.txt", "publish-held.txt", "publish-mode.txt",
                      "publish-mmap.txt", "publish-recover.txt"], ["commit", "-qm", "Base"]):
            subprocess.run(["git", "-C", str(lower), *args], check=True)
        config = {
            "ociVersion": "1.2.0",
            "root": {"path": str(ROOT / ".development/runtime/development/rootfs"), "readonly": True},
            "process": {
                "terminal": False, "user": {"uid": 0, "gid": 0},
                "args": ["/bin/sleep", "3600"], "env": ["PATH=/usr/bin:/bin", "HOME=/tmp"],
                "cwd": "/workspace/packages", "noNewPrivileges": True,
            },
            "linux": {"namespaces": [{"type": t} for t in ("pid", "ipc", "uts", "mount")]},
            "mounts": [
                {"destination": "/proc", "type": "proc", "source": "proc"},
                {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs"},
                {"destination": "/dev", "type": "tmpfs", "source": "tmpfs"},
                {"destination": "/dev/pts", "type": "devpts", "source": "devpts",
                 "options": ["newinstance", "ptmxmode=0666", "mode=0620"]},
                {"destination": "/workspace/packages", "type": "bind", "source": str(storage),
                 "options": ["rbind", "rw", "nosuid", "nodev", "rprivate", "overlayfs_stale_read"]},
            ],
            "annotations": {"workflow.probe." + key: str(value) for key, value in (
                ("lower", lower), ("upper", upper), ("base", base))},
        }
        listener = control_socket = control_stream = None
        if mode == "publication":
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            listener.bind(str(storage / "control.sock"))
            listener.listen(1)
            listener.settimeout(15)
            config["annotations"]["workflow.probe.control"] = "true"
            config["mounts"][-1]["options"].append("dcache=0")
            result["package_dentry_cache"] = 0
        (work / "config.json").write_text(json.dumps(config))
        command = [
            "unshare", "-Ur", "-m", "/bin/sh", "-c",
            'mount --bind "$1" "$2" && mount -o remount,bind,ro "$2" && shift 2 && exec "$@"',
            "gofer-probe", str(lower), str(storage / "lower"),
            str(BUILD / "runsc"), "--root=" + str(work / "state"),
            "--rootless=true", "--ignore-cgroups=true", "--platform=systrap", "--directfs=false",
            "--file-access-mounts=shared", "--network=none", "--overlay2=none", "--debug",
            "--debug-log=" + str(work / "debug.log"), "--log=" + str(work / "runtime.log"),
        ]

        def run(*args, required=True):
            nonlocal control_socket, control_stream
            start = time.monotonic()
            # Detached runtime processes inherit stdout; a pipe would keep
            # communicate() waiting after the CLI has already exited.
            with tempfile.TemporaryFile(mode="w+") as output:
                completed = subprocess.run(command + list(args), stdout=output,
                                           stderr=subprocess.STDOUT, text=True, timeout=45)
                output.seek(0)
                completed.stdout = output.read()
            result["commands"].append({
                "args": list(args), "exit": completed.returncode,
                "seconds": time.monotonic() - start, "output": completed.stdout[:4000],
            })
            if required and completed.returncode:
                raise RuntimeError(f"{args}: {completed.returncode}: {completed.stdout[:2000]}")
            if args[0] == "run" and completed.returncode == 0 and listener is not None:
                if control_stream is not None:
                    control_stream.close()
                    control_socket.close()
                control_socket, _ = listener.accept()
                control_socket.settimeout(15)
                control_stream = control_socket.makefile("rwb", buffering=0)
            return completed

        def shell(text, required=True):
            return run("exec", "--cwd=/workspace/packages", "probe", "/bin/sh", "-c", text, required=required)

        def usage():
            entries = [(p, p.lstat()) for root in (upper, base, storage / "snapshots") for p in root.rglob("*")]
            files = [(p, s) for p, s in entries if stat.S_ISREG(s.st_mode)]
            inodes = {(s.st_dev, s.st_ino): s for _, s in files}
            return {"files": len(files), "regular_inodes": len(inodes),
                    "logical_bytes": sum(s.st_size for s in inodes.values()),
                    "allocated_bytes": sum(s.st_blocks * 512 for s in inodes.values()),
                    "symlinks": sum(stat.S_ISLNK(s.st_mode) for _, s in entries),
                    "snapshot_entries": sum(p.is_relative_to(storage / "snapshots") for p, _ in files),
                    "deleted_markers": sum(p.is_file() for p in (storage / "deleted").rglob("*")),
                    "asset_copies": sum("assets" in p.relative_to(work).parts for p, _ in files),
                    "directory_allocation_included": False}

        def control(action, name, identity, expected_error=False):
            started = time.monotonic()
            control_stream.write(json.dumps({"Action": action, "Path": name, "ID": identity}).encode() + b"\n")
            answer = json.loads(control_stream.readline(65536))
            if expected_error:
                answer["snapshot_files"] = sorted(p.name for p in (storage / "snapshots" / identity).iterdir())
                try:
                    answer["host_snapshot_bytes"] = len((storage / "snapshots" / identity / "file").read_bytes())
                except OSError as error:
                    answer["host_snapshot_errno"] = error.errno
            result.setdefault("publication_control", []).append({"action": action, "path": name,
                "id": identity, "answer": answer, "seconds": time.monotonic() - started})
            if "error" in answer and not expected_error:
                raise RuntimeError(answer)
            return answer

        def publication():
            # Host Git sees only this trusted fixture and the snapshots created
            # by its Gofer. Production private Git still belongs in the sandbox.
            sys.dont_write_bytecode = True
            from git_probe import commit, git, mixed_candidate, oid, tree

            def edit(name, body, atomic=False):
                target = name + ".tmp" if atomic else name
                text = "printf %s " + shlex.quote(body.decode()) + " >" + shlex.quote(target)
                if atomic:
                    text += "; mv " + shlex.quote(target) + " " + shlex.quote(name)
                shell("set -e; " + text)

            def publish(name, identity, package_base):
                snapshot = storage / "snapshots" / identity
                current = oid(lower, "rev-parse", "HEAD")
                def value(which):
                    file = snapshot / which
                    return ("100755" if file.stat().st_mode & 0o111 else "100644", file.read_bytes())
                originals, edits = {name: value("base")}, {name: value("file")}
                status, merged, _ = mixed_candidate(lower, package_base, current, originals, edits)
                assert status == 0
                published = commit(lower, merged, current)
                git(lower, "update-ref", "HEAD", published, current)
                git(lower, "reset", "--hard", published)
                return published

            # A continuing shell retains cwd and an unrelated descriptor through
            # publication. These checks do not claim a retained-PTY integration.
            shell("sh -c 'exec 7<untouched.txt; echo $$ >/tmp/publish-pid; "
                  "touch /tmp/publish-ready; while ! test -f /tmp/publish-end; do sleep .05; done; "
                  "pwd >/tmp/publish-cwd; cat <&7 >/tmp/publish-held-read' "
                  "</dev/null >/tmp/publish-log 2>&1 &")
            shell("for n in $(seq 1 100); do test -f /tmp/publish-ready && exit 0; sleep .05; done; exit 1")
            process = shell("cat /tmp/publish-pid").stdout.strip()
            for kind in ("clean", "late", "held", "mode"):
                name, identity = "publish-" + kind + ".txt", kind + "-one"
                anchor = oid(lower, "rev-parse", "HEAD")
                private = publish_body.replace(b"last", b"private")
                if kind == "held":
                    text = "exec 8<>" + name + "; printf %s " + shlex.quote(private.decode()) + " >&8; "
                    text += "touch /tmp/writer-ready; while ! test -f /tmp/writer-release; do sleep .05; done; "
                    text += "printf 'late via retained FD\\n' >&8; exec 8>&-; touch /tmp/writer-done"
                    shell("sh -c " + shlex.quote(text) + " </dev/null >/tmp/writer-log 2>&1 &")
                    shell("for n in $(seq 1 100); do test -f /tmp/writer-ready && exit 0; sleep .05; done; exit 1")
                else:
                    edit(name, private)
                control("capture", name, identity)
                check("captured_original_" + kind,
                      (storage / "snapshots" / identity / "base").read_bytes() == publish_body
                      and (storage / "snapshots" / identity / "file").read_bytes() == private,
                      "exact original and private snapshot")
                advanced = commit(lower, tree(lower, anchor, {
                    name: ("100644", publish_body.replace(b"first", b"shared"))}), anchor)
                git(lower, "reset", "--hard", advanced)
                if kind == "late":
                    edit(name, private.replace(b"private", b"later"), atomic=True)
                if kind == "mode":
                    shell("chmod +x " + name)
                published = publish(name, identity, anchor)
                check("published_merge_" + kind, (lower / name).read_bytes() == private.replace(b"first", b"shared"),
                      "both non-overlapping edits published from captured bytes")
                acknowledged = control("acknowledge", name, identity)
                duplicate = control("acknowledge", name, identity)
                check("idempotent_ack_" + kind, duplicate["status"] == "already_acknowledged", duplicate)
                if kind == "clean":
                    check("clean_retirement", acknowledged["status"] == "retired"
                          and not (upper / name).exists() and not (base / name).exists()
                          and shell("cat " + name).stdout.encode() == private.replace(b"first", b"shared"), acknowledged)
                else:
                    reason = {"late": "later_content", "held": "open_references", "mode": "later_mode"}[kind]
                    check("preserve_" + kind, acknowledged.get("reason") == reason
                          and (base / name).read_bytes() == private, acknowledged)
                    if kind == "held":
                        shell("touch /tmp/writer-release; for n in $(seq 1 100); do test -f /tmp/writer-done && exit 0; sleep .05; done; exit 1")
                    late = {"late": private.replace(b"private", b"later"),
                            "held": private + b"late via retained FD\n", "mode": private}[kind]
                    check("later_bytes_" + kind, (upper / name).read_bytes() == late
                          and (lower / name).read_bytes() != late, "later edit stays private")
                    control("capture", name, kind + "-two")
                    check("next_original_" + kind,
                          (storage / "snapshots" / (kind + "-two") / "base").read_bytes() == private,
                          "next merge starts at captured private version")
                    publish(name, kind + "-two", published)
                    second = control("acknowledge", name, kind + "-two")
                    check("next_publication_" + kind, second["status"] == "retired"
                          and (lower / name).read_bytes() == late.replace(b"first", b"shared")
                          and shell("cat " + name).stdout.encode() == late.replace(b"first", b"shared")
                          and (kind != "mode" or (lower / name).stat().st_mode & 0o111 == 0o111), second)
                    stale = control("acknowledge", name, identity, expected_error=True)
                    check("stale_ack_" + kind, stale.get("errno") == errno.ESTALE
                          and not (base / name).exists(), stale)
                    verified = control("verify", name, identity)
                    check("old_snapshot_readable_" + kind,
                          verified["sha256"] == hashlib.sha256(private).hexdigest(), verified)
                check("continuing_process_" + kind, shell("kill -0 " + process, required=False).returncode == 0,
                      {"pid": process})
            shell("./client workflow-held-mmap-client </dev/null >/tmp/map-log 2>&1 &")
            shell("for n in $(seq 1 100); do test -f /tmp/map-ready && exit 0; sleep .05; done; exit 1")
            anchor = oid(lower, "rev-parse", "HEAD")
            control("capture", "publish-mmap.txt", "mmap-one")
            published = publish("publish-mmap.txt", "mmap-one", anchor)
            ack = control("acknowledge", "publish-mmap.txt", "mmap-one")
            check("mmap_keeps_private_inode", ack.get("reason") == "open_references", ack)
            shell("touch /tmp/map-release; for n in $(seq 1 100); do test -f /tmp/map-done && exit 0; sleep .05; done; exit 1")
            check("later_mmap_bytes", (upper / "publish-mmap.txt").read_bytes()[16:26] == b"later mmap"
                  and (lower / "publish-mmap.txt").read_bytes()[16:26] == bytes(10), "closed application FD, live writable mapping")
            control("capture", "publish-mmap.txt", "mmap-two")
            publish("publish-mmap.txt", "mmap-two", published)
            ack = control("acknowledge", "publish-mmap.txt", "mmap-two")
            check("mmap_second_publication", ack["status"] == "retired"
                  and (lower / "publish-mmap.txt").read_bytes()[16:26] == b"later mmap", ack)
            shell("touch /tmp/publish-end; for n in $(seq 1 100); do test -s /tmp/publish-held-read && exit 0; sleep .05; done; exit 1")
            check("publication_cwd_and_descriptor", shell("cat /tmp/publish-cwd; cat /tmp/publish-held-read").stdout ==
                  "/workspace/packages\nshared replacement\n", "same process, cwd, and retained descriptor")
            name = "publish-recover.txt"
            private = publish_body.replace(b"last", b"private")
            anchor = oid(lower, "rev-parse", "HEAD")
            edit(name, private)
            control("capture", name, "recover-one")
            publish(name, "recover-one", anchor)
            edit(name, private + b"later durable edit\n", atomic=True)
            ack = control("acknowledge", name, "recover-one")
            check("publication_recovery_setup", ack.get("reason") == "later_content", ack)
            check("publication_asset_cost", usage()["asset_copies"] == 0, usage())

        try:
            run("run", "--detach", "--bundle=" + str(work), "probe")
            check("mount_initialization", usage()["files"] == 0, usage())
            seen = shell("find assets -type f | wc -l").stdout.strip()
            check("complete_asset_namespace", seen == str(count), seen)
            shell("printf 'New label\n' >label.txt")
            small = usage()
            result["small_edit_storage"] = small
            check("small_edit_cost", small["files"] == 2 and small["logical_bytes"] == 20
                  and small["asset_copies"] == 0, small)
            check("original_before_truncate", (base / "label.txt").read_text() == "Old label\n",
                  (base / "label.txt").read_text())
            check("private_isolation", (lower / "label.txt").read_text() == "Old label\n",
                  (lower / "label.txt").read_text())
            check("native_symlink", shell("cat label-link").stdout == "New label\n", "relative symlink")
            check("host_escape_denied", shell("cat escape", required=False).returncode != 0,
                  "host sibling file is not accessible")
            shell("sh -c 'exec 9<untouched.txt; touch /tmp/ready; "
                  "while ! test -f /tmp/release; do sleep .05; done; cat <&9 >/tmp/held' "
                  "</dev/null >/tmp/held-log 2>&1 &")
            shell("for n in $(seq 1 100); do test -f /tmp/ready && exit 0; sleep .05; done; exit 1")
            (lower / "replacement").write_text("shared replacement\n")
            (lower / "replacement").replace(lower / "untouched.txt")
            fresh = shell("cat untouched.txt; stat -c %s untouched.txt").stdout
            check("fresh_path_with_old_descriptor", fresh == "shared replacement\n19\n", fresh)
            shell("touch /tmp/release; for n in $(seq 1 100); do test -s /tmp/held && exit 0; sleep .05; done; exit 1")
            held = shell("cat /tmp/held").stdout
            check("old_descriptor_identity", held == "before\n", held)
            hashes = shell("git hash-object untouched.txt; cat untouched.txt | git hash-object --stdin").stdout.splitlines()
            check("native_git_hash_coherence", len(hashes) == 2 and hashes[0] == hashes[1], hashes)
            native = shell("./client workflow-file-client", required=False)
            check("elf_and_first_copy_mmap", native.returncode == 0, native.stdout)
            check("mmap_isolation", (lower / "handles.txt").read_bytes()[:7] == b"before\n",
                  "shared source unchanged")
            write = shell("set -e; git add label.txt; git diff --cached --name-only; "
                          "git commit -qm 'Private label'; git log -1 --format=%s", required=False)
            check("native_git_commit", write.returncode == 0 and write.stdout == "label.txt\nPrivate label\n", write.stdout)
            check("shared_git_isolation", subprocess.check_output(
                ["git", "-C", str(lower), "log", "-1", "--format=%s"], text=True) == "Base\n", "shared HEAD remains Base")
            (lower / "label.txt").write_text("Shared label\n")
            subprocess.run(["git", "-C", str(lower), "add", "label.txt"], check=True)
            subprocess.run(["git", "-C", str(lower), "commit", "-qm", "Shared label"], check=True)
            shared_commit = subprocess.check_output(
                ["git", "-C", str(lower), "rev-parse", "HEAD"], text=True).strip()
            conflict = shell("git -c merge.conflictStyle=diff3 merge --no-edit " + shared_commit, required=False)
            markers = shell("cat label.txt").stdout
            stages = shell("git show :1:label.txt; git show :2:label.txt; git show :3:label.txt").stdout
            check("mounted_git_conflict", conflict.returncode == 1
                  and all(marker in markers for marker in ("<<<<<<<", "|||||||", "=======", ">>>>>>>"))
                  and stages == "Old label\nNew label\nShared label\n", {"output": conflict.stdout, "markers": markers, "stages": stages})
            run("kill", "probe", "KILL")
            run("delete", "--force", "probe")
            run("run", "--detach", "--bundle=" + str(work), "probe")
            recovered = shell("cat label.txt; git show :1:label.txt; git show :2:label.txt; git show :3:label.txt").stdout
            check("mounted_conflict_durability", recovered == markers + stages, "markers and all three index stages survive recreation")
            resolved = shell("set -e; printf 'Resolved label\n' >label.txt; git add label.txt; "
                             "git commit -qm 'Resolved native merge'; git log -1 --format=%s; "
                             "git ls-files -u; git merge-base --is-ancestor " + shared_commit + " HEAD", required=False)
            check("mounted_git_resolution", resolved.returncode == 0 and resolved.stdout == "Resolved native merge\n", resolved.stdout)
            native = shell("./native-client", required=False)
            check("native_namespace_tools", native.returncode == 0, native.stdout)
            renamed = shell("./native-client rename-lower", required=False)
            check("shared_file_rename", renamed.returncode == 0, renamed.stdout)
            check("rename_originals_and_isolation", all(
                (lower / "rename" / name).read_text() == "shared rename\n"
                and (base / "rename" / name).read_text() == "shared rename\n"
                and not (upper / "rename" / name).exists()
                and (storage / "deleted" / "rename" / name).is_file()
                for name in ("new.txt", "replace.txt"))
                and (lower / "renamed/target.txt").read_text() == "old target\n"
                and (base / "renamed/target.txt").read_text() == "old target\n"
                and not (base / "renamed/new.txt").exists(),
                "source deletions and destination originals remain private")
            check("rename_noop_and_rejection_cost", all(
                not (root / "rename" / name).exists()
                for root in (upper, base, storage / "deleted")
                for name in ("same.txt", "alias.txt", "invalid.txt")),
                "no-op and rejected renames do not copy or hide shared files")
            linked = shell("./native-client symlink-lower", required=False)
            check("shared_symlink_mutations_and_retained_descriptors", linked.returncode == 0, linked.stdout)
            check("symlink_originals_without_dereferencing", all(
                os.readlink(lower / "links" / name) == target
                and os.readlink(base / "links" / name) == target
                for name, target in {"from": "../../host-only-source", "target": "../../host-only-target", "remove": "../../host-only-removed", "keep": "../../host-only-kept"}.items()),
                "native links retain their target text; no target is read")
            removed = shell("./native-client remove-directories", required=False)
            check("nested_delete_and_private_rmdir", removed.returncode == 0, removed.stdout)
            check("nested_deletion_originals", all(
                (lower / name).read_text() == "original nested\n"
                and (base / name).read_text() == "original nested\n"
                for name in ("nested/remove.txt", "nested/deep/remove.txt")),
                "shared contents and nested originals are intact")
            removed = shell("./native-client remove-shared-directories", required=False)
            check("shared_directory_removal_and_recreation", removed.returncode == 0, removed.stdout)
            check("shared_directory_removal_originals", all(
                (lower / name).read_text() == "original directory child\n"
                and (base / name).read_text() == "original directory child\n"
                for name in ("remove-shared/deep/old.txt", "recreate-shared/old.txt")),
                "directory removal preserves original files and shared contents")
            atomic = shell("set -e; exec 8<atomic.txt; printf 'Atomic private\n' >atomic.tmp; "
                           "mv atomic.tmp atomic.txt; cat atomic.txt; cat <&8; rm remove.txt; "
                           "test ! -e remove.txt", required=False)
            check("atomic_save_and_delete", atomic.returncode == 0 and atomic.stdout == "Atomic private\nOriginal atomic\n"
                  and (base / "atomic.txt").read_text() == "Original atomic\n"
                  and (base / "remove.txt").read_text() == "Original removed\n", atomic.stdout)
            check("deleted_name_enumeration", "remove.txt" not in shell("ls -1").stdout.splitlines(),
                  "deleted lower name is absent from directory entries")
            check("namespace_asset_copy_cost", usage()["asset_copies"] == 0, usage())
            run("kill", "probe", "KILL")
            run("delete", "--force", "probe")
            run("run", "--detach", "--bundle=" + str(work), "probe")
            check("runtime_loss_durability", shell("cat label.txt").stdout == "Resolved label\n"
                  and (base / "label.txt").read_text() == "Old label\n", usage())
            durable = shell("set -e; cat atomic.txt; test ! -e remove.txt; git log -1 --format=%s; "
                            "cat ignored/native/file", required=False)
            check("namespace_and_git_durability", durable.returncode == 0
                  and durable.stdout == "Atomic private\nResolved native merge\nreplacement", durable.stdout)
            renamed = shell("set -e; test ! -e rename/new.txt; test ! -e rename/replace.txt; "
                            "cat renamed/new.txt renamed/target.txt", required=False)
            check("shared_rename_durability", renamed.returncode == 0
                  and renamed.stdout == "private rename\nprivate rename\n", renamed.stdout)
            linked = shell("set -e; test ! -L links/from; test ! -L links/target; test ! -L links/remove; test ! -L links/keep; readlink links/moved links/new", required=False)
            check("symlink_mutation_runtime_recreation", linked.returncode == 0
                  and linked.stdout == "../../host-only-kept\n/tmp/private-symlink-target\n", linked.stdout)
            (lower / "nested/keep.txt").write_text("shared sibling update\n")
            removed = shell("set -e; test -d nested/deep; test ! -e private-remove; "
                            "test ! -e nested/remove.txt; test ! -e nested/deep/remove.txt; "
                            "cat nested/keep.txt nested/deep/keep.txt", required=False)
            check("nested_deletion_recreation_and_live_siblings", removed.returncode == 0
                  and removed.stdout == "shared sibling update\nuntouched sibling\n", removed.stdout)
            for name in ("shared-empty/new.txt", "remove-shared/deep/new.txt", "recreate-shared/new.txt"):
                (lower / name).write_text("later shared addition\n")
            removed = shell("set -e; test ! -e shared-empty; test ! -e remove-shared; "
                            "test ! -e recreate-shared/old.txt; test ! -e recreate-shared/new.txt; "
                            "cat recreate-shared/private.txt", required=False)
            check("removed_directories_hide_later_shared_children_after_recreation", removed.returncode == 0
                  and removed.stdout == "new private directory\n", removed.stdout)
            result["sandbox_git_version"] = shell("git --version").stdout.strip()
            if mode == "publication":
                publication()
            run("delete", "--force", "probe")
            # Same allocated asset tree, sequential runs, unflushed host caches.
            # Debugging is disabled for every timed profile. These are component
            # traversal timings, excluding fixture creation and runtime startup.
            command[:] = [arg for arg in command if arg != "--debug" and not arg.startswith("--debug-log=")]
            annotations = config["annotations"]
            mount = config["mounts"][-1]
            result["traversal"] = {}
            result["writes"] = {}
            if mode == "publication":
                result["working_set_reads"] = {}
                for profile in ("default_cache", "dcache_0"):
                    mount["options"] = [option for option in mount["options"] if not option.startswith("dcache=")]
                    if profile == "dcache_0":
                        mount["options"].append("dcache=0")
                    (work / "config.json").write_text(json.dumps(config))
                    run("run", "--detach", "--bundle=" + str(work), "probe")
                    if profile == "default_cache":
                        private = publish_body.replace(b"last", b"private")
                        ack = control("acknowledge", "publish-recover.txt", "recover-one")
                        verified = control("verify", "publish-recover.txt", "recover-one")
                        check("publication_checkpoint_recreation", ack["status"] == "already_acknowledged"
                              and (base / "publish-recover.txt").read_bytes() == private
                              and shell("cat publish-recover.txt").stdout.encode() == private + b"later durable edit\n"
                              and verified["sha256"] == hashlib.sha256(private).hexdigest(),
                              "captured original, later bytes, and acknowledgement identity survive recreation")
                    measured = shell("./client workflow-read-client")
                    timings = json.loads(measured.stdout)
                    result["working_set_reads"][profile] = timings
                    check("read_working_set_" + profile, len(timings) == 31,
                          {"median_ms_per_10_files": statistics.median(timings) / 1e6,
                           "p95_ms_per_10_files": sorted(timings)[29] / 1e6})
                    run("delete", "--force", "probe")
            for profile, directfs in (() if mode in ("namespace", "publication") else (("sparse", False), ("stock_rpc", False), ("stock_directfs", True))):
                config["annotations"] = annotations if profile == "sparse" else {}
                mount["source"] = str(storage if profile == "sparse" else lower)
                mount["options"][1] = "rw" if profile == "sparse" or writes_only else "ro"
                command[:] = ["--directfs=" + str(directfs).lower() if arg.startswith("--directfs=")
                              else arg for arg in command]
                (work / "config.json").write_text(json.dumps(config))
                run("run", "--detach", "--bundle=" + str(work), "probe")
                if writes_only:
                    measured = shell("./client workflow-write-client " + profile + (" atomic" if mode == "atomic" else ""))
                    timings = json.loads(measured.stdout)
                    result["writes"][profile] = timings
                    for size, phases in timings.items():
                        print("writes", profile, size, {
                            phase: {"median_ms": statistics.median(values) / 1e6,
                                    "p95_ms": sorted(values)[29] / 1e6}
                            for phase, values in phases.items()
                        }, flush=True)
                    valid = True
                    for size in (64, 65536, 1048576):
                        expected = bytearray(bytes(range(251)) * (size // 251 + 1))[:size]
                        expected[0] = 2
                        for n in range(31):
                            relative = Path("writes") / profile / str(size) / str(n)
                            output_root = upper if profile == "sparse" else lower
                            valid &= (output_root / relative).read_bytes() == expected
                            if profile == "sparse":
                                valid &= (base / relative).read_bytes() == b"\x7f" * size
                                valid &= (lower / relative).read_bytes() == b"\x7f" * size
                    check("write_data_" + profile, valid, "all final bytes and applicable originals checked")
                else:
                    result["traversal"][profile] = {}
                for tree in (() if writes_only else ("assets", "small")):
                    samples = []
                    for _ in range(4):
                        shell("set -e; find " + tree + " -type f -exec cat {} + >/dev/null")
                        samples.append(result["commands"][-1]["seconds"])
                    result["traversal"][profile][tree] = {
                        "samples_seconds": samples, "first_seconds": samples[0],
                        "warm_median_seconds": statistics.median(samples[1:]),
                    }
                if not writes_only:
                    print("traversal", profile, result["traversal"][profile], flush=True)
                run("delete", "--force", "probe")
        except Exception as error:
            result["error"] = str(error)
            print("probe error", error, flush=True)
        finally:
            run("delete", "--force", "probe", required=False)
            if control_stream is not None:
                control_stream.close()
                control_socket.close()
            if listener is not None:
                listener.close()
            logs = "\n".join(p.read_text(errors="replace") for p in work.glob("debug*") if p.is_file())
            (BUILD / "sparse-debug.log").write_text(logs)
            result["extension_loaded"] = "via extension workflow-sparse-probe" in logs
            result["runtime_errors"] = [line for line in logs.splitlines()
                                        if any(token in line for token in ("panic:", "FATAL ERROR", "SIGSYS"))][-20:]
    result["component_checks_pass"] = ("error" not in result and result["extension_loaded"]
                                       and all(c["passed"] for c in result["checks"].values()))
    output_name = {"writes": "gofer-write-results.json", "namespace": "gofer-namespace-results.json",
                   "atomic": "gofer-atomic-results.json",
                   "publication": "gofer-publication-results.json",
                   "transport": "gofer-results.json"}[mode]
    (HERE / output_name).write_text(json.dumps(result, indent=2) + "\n")
    print("Full workflow remains unqualified; this probe omits required operations.", flush=True)
    return 0 if result["component_checks_pass"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
