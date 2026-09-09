#!/usr/bin/env python3
"""Disposable Git correctness matrix and candidate preparation microbenchmark.

Private-index preparation is a proposal, not production activation. Timings
include Git subprocess startup but exclude schema hooks and source publication.
"""

import json
import os
from pathlib import Path
import platform
import resource
import shutil
import statistics
import subprocess
import sys
import tempfile
import time


ENV = os.environ.copy()
ENV.update(GIT_AUTHOR_NAME="Analysis", GIT_AUTHOR_EMAIL="analysis@example.test",
           GIT_COMMITTER_NAME="Analysis", GIT_COMMITTER_EMAIL="analysis@example.test",
           GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null")


def git(repo, *args, data=None, index=None, allowed=(0,)):
    env = ENV | ({"GIT_INDEX_FILE": str(index)} if index else {})
    result = subprocess.run(["git", "-C", str(repo), *args], input=data,
                            capture_output=True, env=env, timeout=60)
    if result.returncode not in allowed:
        raise RuntimeError(f"git {args}: {result.returncode}: {result.stderr.decode()}")
    return result


def oid(repo, *args, **kwargs):
    return git(repo, *args, **kwargs).stdout.decode().strip()


def tree(repo, base, changes):
    """The dirty path list comes from the filesystem, not a worktree scan."""
    with tempfile.TemporaryDirectory() as temp:
        index = Path(temp) / "index"
        git(repo, "read-tree", base, index=index)
        records = bytearray()
        for path, value in changes.items():
            if value is None:
                mode, blob = "0", "0" * 40
            else:
                mode, content = value
                blob = oid(repo, "hash-object", "-w", "--stdin", data=content)
            records.extend(f"{mode} {blob}\t".encode() + path.encode() + b"\0")
        git(repo, "update-index", "-z", "--index-info", data=bytes(records), index=index)
        return oid(repo, "write-tree", index=index)


def commit(repo, tree_oid, parent=None):
    return oid(repo, "commit-tree", tree_oid, *(["-p", parent] if parent else []),
               data=b"Disposable analysis candidate\n")


def merge(repo, shared, private):
    result = git(repo, "-c", "merge.conflictStyle=diff3", "merge-tree",
                 "--write-tree", "-z", shared, private, allowed=(0, 1))
    merged = result.stdout.split(b"\0", 1)[0].decode()
    return result.returncode, merged, result.stdout


def read(repo, tree_oid, path):
    return git(repo, "show", tree_oid + ":" + path, allowed=(0, 128)).stdout


def mixed_candidate(repo, package_base, shared, originals, edits):
    """Retain base topology, then substitute each path's observed original."""
    parent = commit(repo, tree(repo, package_base, originals))
    current = commit(repo, oid(repo, "rev-parse", shared + "^{tree}"), parent)
    private = commit(repo, tree(repo, parent, edits), parent)
    return merge(repo, current, private)


def fixture(root, count):
    repo = root / "repo"
    repo.mkdir()
    git(repo, "init", "-q")
    for i in range(count):
        path = repo / f"files/{i // 100:04d}/{i:06d}.txt"
        path.parent.mkdir(exist_ok=True, parents=True)
        path.write_bytes(b"first\n" + b"context\n" * 20 + b"last\n")
    git(repo, "add", "-A")
    git(repo, "commit", "-qm", "Base")
    return repo, oid(repo, "rev-parse", "HEAD")


def correctness():
    results = []
    with tempfile.TemporaryDirectory(prefix="workflow-git-cases-") as temp:
        repo, initial = fixture(Path(temp), 1)
        body = b"first\n" + b"context\n" * 20 + b"last\n"
        cases = [
            ("disjoint_lines", {"file": ("100644", body.replace(b"first", b"A"))},
             {"file": ("100644", body.replace(b"last", b"B"))}, 0, b"A\n"),
            ("same_line", {"file": ("100644", b"A\n")}, {"file": ("100644", b"B\n")}, 1, b"<<<<<<<"),
            ("identical_edit", {"file": ("100644", b"same\n")}, {"file": ("100644", b"same\n")}, 0, b"same\n"),
            ("add_add", {"new": ("100644", b"A\n")}, {"new": ("100644", b"B\n")}, 1, None),
            ("delete_modify", {"file": None}, {"file": ("100644", b"B\n")}, 1, None),
            ("rename_edit", {"file": None, "renamed": ("100644", body)},
             {"file": ("100644", body.replace(b"last", b"B"))}, 0, None),
            ("binary_conflict", {"binary": ("100644", b"\0A")}, {"binary": ("100644", b"\0B")}, 1, None),
            ("executable_and_content", {"file": ("100755", body)},
             {"file": ("100644", body.replace(b"last", b"B"))}, 0, None),
            ("symlink_conflict", {"link": ("120000", b"one")}, {"link": ("120000", b"two")}, 1, None),
            ("whitespace_filename", {"space and\nnewline": ("100644", b"A")},
             {"space and\nnewline": ("100644", b"B")}, 1, None),
        ]
        baseline_tree = tree(repo, initial, {"file": ("100644", body),
            "binary": ("100644", b"\0base"), "link": ("120000", b"base")})
        base = commit(repo, baseline_tree, initial)
        for name, shared_changes, private_changes, expected_status, expected_bytes in cases:
            shared = commit(repo, tree(repo, base, shared_changes), base)
            private = commit(repo, tree(repo, base, private_changes), base)
            status, merged, diagnostic = merge(repo, shared, private)
            assert status == expected_status, (name, status, diagnostic)
            if expected_bytes:
                assert expected_bytes in read(repo, merged, "file"), name
            if name == "disjoint_lines":
                assert b"B\n" in read(repo, merged, "file")
            if name == "rename_edit":
                assert b"B\n" in read(repo, merged, "renamed")
            if name == "executable_and_content":
                assert git(repo, "ls-tree", merged, "file").stdout.startswith(b"100755 ")
                assert b"B\n" in read(repo, merged, "file")
            if name == "whitespace_filename":
                assert b"\tspace and\nnewline\0" in diagnostic
            results.append({"case": name, "merge_exit": status, "pass": True})
        # Original file content from first touch, including a later first touch.
        late_tree = tree(repo, base, {"file": ("100644", b"later shared\n")})
        synthetic_base = commit(repo, late_tree)
        shared = commit(repo, late_tree, synthetic_base)
        private = commit(repo, tree(repo, late_tree, {"file": ("100644", b"later private\n")}), synthetic_base)
        status, merged, _ = merge(repo, shared, private)
        assert status == 0 and read(repo, merged, "file") == b"later private\n"
        results.append({"case": "first_touch_after_shared_advance", "pass": True})
        # Two dirty paths first touched at different shared generations need
        # different originals. A package-wide old HEAD would falsely conflict.
        initial_tree = tree(repo, base, {"early": ("100644", body), "late": ("100644", body)})
        old = commit(repo, initial_tree)
        late_original = body.replace(b"first", b"shared-before-first-touch")
        current_tree = tree(repo, old, {
            "early": ("100644", body.replace(b"first", b"shared-early")),
            "late": ("100644", late_original.replace(b"last", b"shared-late")),
            "untouched": ("100644", b"new shared path\n"),
        })
        originals = {"early": ("100644", body), "late": ("100644", late_original)}
        edits = {"early": ("100644", body.replace(b"last", b"private-early")),
                 "late": ("100644", late_original.replace(b"shared-before-first-touch", b"private-late"))}
        current = commit(repo, current_tree, old)
        status, merged, _ = mixed_candidate(repo, old, current, originals, edits)
        assert status == 0
        assert read(repo, merged, "early") == body.replace(b"first", b"shared-early").replace(b"last", b"private-early")
        assert read(repo, merged, "late") == body.replace(b"first", b"private-late").replace(b"last", b"shared-late")
        assert read(repo, merged, "untouched") == b"new shared path\n"
        naive_current = commit(repo, current_tree, old)
        naive_private = commit(repo, tree(repo, old, edits), old)
        assert merge(repo, naive_current, naive_private)[0] == 1
        results.append({"case": "mixed_first_touch_bases_with_live_untouched_paths", "pass": True})
        # Seeding the synthetic base from today's tree loses rename ancestry:
        # the destination is already present, so Git sees delete/modify instead.
        renamed = commit(repo, tree(repo, old, {
            "early": None, "renamed-early": ("100644", body),
            "late": ("100644", late_original),
        }), old)
        status, merged, _ = mixed_candidate(repo, old, renamed, originals, edits)
        assert status == 0 and b"private-early\n" in read(repo, merged, "renamed-early")
        assert b"private-late\n" in read(repo, merged, "late")
        assert mixed_candidate(repo, renamed, renamed, originals, edits)[0] == 1
        results.append({"case": "mixed_bases_retain_shared_rename_topology", "pass": True})
        # Resolve against exactly the shared version shown, then merge a new advance.
        first = commit(repo, tree(repo, base, {"file": ("100644", body.replace(b"first", b"A"))}), base)
        resolved = commit(repo, tree(repo, first, {"file": ("100644", body.replace(b"first", b"A+B"))}), first)
        newer = commit(repo, tree(repo, first, {"file": ("100644", body.replace(b"first", b"A").replace(b"last", b"C"))}), first)
        status, merged, _ = merge(repo, newer, resolved)
        assert status == 0 and b"A+B" in read(repo, merged, "file") and b"C\n" in read(repo, merged, "file")
        results.append({"case": "resolve_then_shared_advances", "pass": True})
        # Compare-and-swap reference publication cannot silently replace another publisher.
        git(repo, "update-ref", "refs/heads/publish", first)
        one = git(repo, "update-ref", "refs/heads/publish", newer, first)
        two = git(repo, "update-ref", "refs/heads/publish", resolved, first, allowed=(0, 128))
        assert one.returncode == 0 and two.returncode != 0
        results.append({"case": "stale_publication_compare_and_swap", "pass": True})
        # Native private branches keep the correct base across publications if
        # the published merge retains the captured private commit as a parent.
        # Squashing it away while leaving the private branch unchanged does not.
        private_one = commit(repo, tree(repo, base, {"file": ("100644", body.replace(b"last", b"private-one"))}), base)
        status, merged, _ = merge(repo, first, private_one)
        assert status == 0
        published = oid(repo, "commit-tree", merged, "-p", first, "-p", private_one, data=b"Publish native branch\n")
        private_two = commit(repo, tree(repo, private_one, {"file": ("100644", body.replace(b"last", b"private-two"))}), private_one)
        status, merged_again, _ = merge(repo, published, private_two)
        assert status == 0 and read(repo, merged_again, "file") == body.replace(b"first", b"A").replace(b"last", b"private-two")
        squashed = commit(repo, merged, first)
        assert merge(repo, squashed, private_two)[0] == 1
        results.append({"case": "native_merge_ancestry_preserves_next_activation_base", "pass": True})
    return results


def sparse_native_conflict():
    """Use ordinary Git conflict state without checking out unchanged assets."""
    with tempfile.TemporaryDirectory(prefix="workflow-native-conflict-") as temp:
        root = Path(temp)
        repo, initial = fixture(root, 1)
        changes = {f"assets/{n:04d}.bin": ("100644", b"asset\0" * 1024) for n in range(1024)}
        changes.update({"label.txt": ("100644", b"Original label\n"),
                        "untouched.txt": ("100644", b"Before\n")})
        base = commit(repo, tree(repo, initial, changes), initial)
        private = commit(repo, tree(repo, base, {"label.txt": ("100644", b"Your label\n")}), base)
        shared = commit(repo, tree(repo, base, {
            "label.txt": ("100644", b"Shared label\n"),
            "untouched.txt": ("100644", b"Shared unrelated update\n"),
        }), base)
        primary = root / "active-workspace"
        primary.mkdir()
        later_edit = primary / "label.txt"
        later_edit.write_bytes(b"A newer edit made after activation capture\n")
        resolution = root / "conflict"
        start = time.perf_counter()
        git(repo, "worktree", "add", "--detach", "--no-checkout", str(resolution), private)
        git(resolution, "sparse-checkout", "set", "--no-cone", "--stdin", data=b"/label.txt\n")
        git(resolution, "read-tree", "--reset", "-u", "HEAD")
        conflict = git(resolution, "-c", "merge.conflictStyle=diff3", "merge", "--no-edit", shared, allowed=(1,))
        prepare_ms = (time.perf_counter() - start) * 1000
        markers = (resolution / "label.txt").read_bytes()
        for expected in (b"<<<<<<<", b"|||||||", b"=======", b">>>>>>>",
                         b"Original label", b"Your label", b"Shared label"):
            assert expected in markers, markers
        for stage, expected in ((1, b"Original label\n"), (2, b"Your label\n"), (3, b"Shared label\n")):
            assert git(resolution, "show", f":{stage}:label.txt").stdout == expected
        status = git(resolution, "status", "--porcelain=v1", "-z").stdout
        assert b"UU label.txt\0" in status, status
        assert not any(entry[:2] in (b" D", b"D ") for entry in status.split(b"\0")), status
        assert not (resolution / "assets").exists()
        assert not (resolution / "untouched.txt").exists()
        # The agent only needs an ordinary file edit, git add, and git commit.
        start = time.perf_counter()
        (resolution / "label.txt").write_bytes(b"Resolved label\n")
        git(resolution, "add", "label.txt")
        git(resolution, "commit", "-qm", "Resolve label conflict")
        resolved = oid(resolution, "rev-parse", "HEAD")
        assert set(oid(resolution, "show", "-s", "--format=%P", "HEAD").split()) == {private, shared}
        # A publication retry must merge another shared advance, keeping the
        # chosen resolution and the original workspace's newer, uncaptured edit.
        newer = commit(repo, tree(repo, shared, {
            "untouched.txt": ("100644", b"Another shared update during resolution\n"),
        }), shared)
        git(resolution, "merge", "--no-edit", newer)
        retry_ms = (time.perf_counter() - start) * 1000
        assert (resolution / "label.txt").read_bytes() == b"Resolved label\n"
        assert git(resolution, "show", "HEAD:untouched.txt").stdout == b"Another shared update during resolution\n"
        assert git(resolution, "status", "--porcelain=v1").stdout == b""
        assert not (resolution / "assets").exists()
        assert later_edit.read_bytes() == b"A newer edit made after activation capture\n"
        return {"case": "sparse_native_git_conflict_resolution", "pass": True,
                "git_conflict_exit": conflict.returncode, "conflict_status": status.decode(),
                "conflict_file": markers.decode(), "resolved_commit": resolved,
                "materialized_asset_files": 0, "asset_tree_entries": 1024,
                "original_workspace_later_edit_preserved": True,
                "prepare_and_conflict_ms": prepare_ms, "resolve_and_retry_ms": retry_ms,
                "measurement": "one trusted host Git fixture; excludes filesystem capture, transfer, schema/hooks and publication",
                "full_activation_qualified": False}


def benchmark(count):
    samples = {"index_candidate_ms": [], "merge_tree_only_ms": [],
               "mixed_base_candidate_ms": [],
               "two_worktree_prepare_ms": [], "worktree_cleanup_ms": []}
    with tempfile.TemporaryDirectory(prefix="workflow-git-bench-") as temp:
        root = Path(temp)
        repo, base = fixture(root, count)
        path = "files/0000/000000.txt"
        body = read(repo, base, path)
        shared = commit(repo, tree(repo, base, {path: ("100644", body.replace(b"first", b"A"))}), base)
        changes = {path: ("100644", body.replace(b"last", b"B"))}
        for _ in range(5):
            start = time.perf_counter()
            private = commit(repo, tree(repo, base, changes), base)
            status, merged, _ = merge(repo, shared, private)
            assert status == 0
            samples["index_candidate_ms"].append((time.perf_counter() - start) * 1000)
            start = time.perf_counter()
            merge(repo, shared, private)
            samples["merge_tree_only_ms"].append((time.perf_counter() - start) * 1000)
            start = time.perf_counter()
            status, mixed, _ = mixed_candidate(repo, base, shared, {path: ("100644", body)}, changes)
            assert status == 0 and mixed == merged
            samples["mixed_base_candidate_ms"].append((time.perf_counter() - start) * 1000)
            patch = git(repo, "diff", "--binary", base, private).stdout
            first, second = root / "base-worktree", root / "merge-worktree"
            start = time.perf_counter()
            git(repo, "worktree", "add", "--detach", str(first), base)
            git(first, "apply", "--index", "--binary", "-", data=patch)
            git(first, "commit", "-qm", "Private change")
            candidate = oid(first, "rev-parse", "HEAD")
            git(repo, "worktree", "add", "--detach", str(second), shared)
            git(second, "cherry-pick", candidate)
            assert oid(second, "rev-parse", "HEAD^{tree}") == merged
            samples["two_worktree_prepare_ms"].append((time.perf_counter() - start) * 1000)
            start = time.perf_counter()
            git(repo, "worktree", "remove", "--force", str(first))
            git(repo, "worktree", "remove", "--force", str(second))
            samples["worktree_cleanup_ms"].append((time.perf_counter() - start) * 1000)
    return {"tracked_files": count, "dirty_files": 1, "bytes_per_file": len(body),
            "samples": samples, "median": {k: statistics.median(v) for k, v in samples.items()}}


def portable_delta():
    """Prove the import merge can use plain original/edited files without old Git."""
    with tempfile.TemporaryDirectory(prefix="workflow-portable-") as temp:
        root = Path(temp)
        original = root / "old-sandbox"
        original.mkdir()
        base = b"first\n" + b"context\n" * 20 + b"last\n"
        for name, data in (("base", base), ("files", base.replace(b"last", b"B"))):
            (original / name).mkdir()
            (original / name / "file").write_bytes(data)
        (original / "manifest.json").write_text(json.dumps({
            "schema": 1, "entries": [{"path": "file", "kind": "modified", "mode": "100644"}]}))
        portable = root / "transferred"
        shutil.copytree(original, portable)
        shutil.rmtree(original)
        target = root / "new-sandbox"
        target.mkdir()
        repo, initial = fixture(target, 1)
        current_tree = tree(repo, initial, {"file": ("100644", base.replace(b"first", b"A"))})
        imported_base = tree(repo, current_tree, {"file": ("100644", (portable / "base/file").read_bytes())})
        parent = commit(repo, imported_base)
        current = commit(repo, current_tree, parent)
        local = commit(repo, tree(repo, imported_base, {"file": ("100644", (portable / "files/file").read_bytes())}), parent)
        status, merged, _ = merge(repo, current, local)
        result = read(repo, merged, "file")
        assert status == 0 and b"A\n" in result and b"B\n" in result
        return {"case": "portable_plain_file_delta_without_original_sandbox_or_git_history", "pass": True}


def binary_benchmark():
    with tempfile.TemporaryDirectory(prefix="workflow-binary-") as temp:
        repo, initial = fixture(Path(temp), 1)
        data = [os.urandom(2 * 1024 * 1024) for _ in range(10)]
        baseline = tree(repo, initial, {f"bin/{i}": ("100644", content) for i, content in enumerate(data)})
        base = commit(repo, baseline, initial)
        samples = []
        for trial in range(3):
            changes = {f"bin/{i}": ("100644", content[:1024] + bytes([trial]) + content[1025:])
                       for i, content in enumerate(data)}
            start = time.perf_counter()
            private = commit(repo, tree(repo, base, changes), base)
            status, _, _ = merge(repo, base, private)
            assert status == 0
            samples.append((time.perf_counter() - start) * 1000)
        return {"files": 10, "bytes_per_file": len(data[0]), "new_distinct_blobs_each_trial": True,
                "candidate_ms": samples, "median_ms": statistics.median(samples)}


def portable_rename():
    """Carry native base objects explicitly when a transfer must retain renames."""
    with tempfile.TemporaryDirectory(prefix="workflow-portable-rename-") as temp:
        root = Path(temp)
        source = root / "original"
        source.mkdir()
        repo, initial = fixture(source, 1)
        body = b"first\n" + b"context\n" * 20 + b"last\n"
        base = commit(repo, tree(repo, initial, {"file": ("100644", body)}))
        workspace = source / "workspace"
        (workspace / "originals").mkdir(parents=True)
        (workspace / "files").mkdir()
        (workspace / "originals/file").write_bytes(body)
        (workspace / "files/file").write_bytes(body.replace(b"last", b"private"))
        (workspace / "manifest.json").write_text(json.dumps({
            "schema": 1, "package_base": base,
            "entries": [{"path": "file", "kind": "modified", "mode": "100644"}]}))
        git(repo, "update-ref", "refs/heads/export-base", base)
        git(repo, "bundle", "create", str(workspace / "base.bundle"), "refs/heads/export-base")
        portable = root / "transferred"
        shutil.copytree(workspace, portable)
        shutil.rmtree(source)
        target = root / "target"
        target.mkdir()
        repo, initial = fixture(target, 1)
        current = commit(repo, tree(repo, initial, {"renamed": ("100644", body)}), initial)
        git(repo, "fetch", "-q", str(portable / "base.bundle"), "refs/heads/export-base")
        manifest = json.loads((portable / "manifest.json").read_text())
        originals = {"file": ("100644", (portable / "originals/file").read_bytes())}
        edits = {"file": ("100644", (portable / "files/file").read_bytes())}
        status, merged, _ = mixed_candidate(repo, manifest["package_base"], current, originals, edits)
        assert status == 0 and read(repo, merged, "renamed") == body.replace(b"last", b"private")
        return {"case": "portable_git_base_bundle_retains_shared_rename", "pass": True,
                "fixture_bundle_bytes": (portable / "base.bundle").stat().st_size}


def native_benchmark(count, reuse_validation=True):
    """Explicit checkout, capture, merge and source switch; no runtime/hooks.

    Capture is an ordinary private commit; publication never resets its working
    files. Native merge ancestry is covered by correctness(). No host-power-loss
    durability claim follows from this timing run.
    """
    samples = []
    with tempfile.TemporaryDirectory(prefix="workflow-native-git-") as temp:
        root = Path(temp)
        source, base = fixture(root, count)
        path = "files/0000/000000.txt"
        original = read(source, base, path)
        advanced = commit(source, tree(source, base, {
            path: ("100644", original.replace(b"first", b"shared"))}), base)
        for trial in range(5):
            private = root / f"private-{trial}"
            git(source, "reset", "--hard", base)
            usage = resource.getrusage(resource.RUSAGE_CHILDREN)
            started = time.perf_counter()
            git(root, "clone", "--no-local", str(source), str(private))
            clone_ms = (time.perf_counter() - started) * 1000
            git(source, "reset", "--hard", advanced)
            private_body = original.replace(b"last", f"private-{trial}".encode())
            expected = private_body.replace(b"first", b"shared")
            (private / path).write_bytes(private_body)
            (private / "ignored").mkdir()
            # Capture honors ordinary ignore rules without making ignored files
            # ephemeral. The explicit fixture edit adds this Git ignore rule.
            (private / ".gitignore").write_text("ignored/\n")
            (private / "ignored/artifact").write_bytes(b"durable artifact")
            started = time.perf_counter()
            git(private, "add", "-A")
            git(private, "commit", "-qm", "Capture private candidate")
            captured = oid(private, "rev-parse", "HEAD")
            capture_ms = (time.perf_counter() - started) * 1000
            # A save after immutable capture must neither change the candidate
            # nor disappear when it is published.
            (private / path).write_bytes(b"later save\n")
            started = time.perf_counter()
            git(source, "fetch", "-q", str(private), captured)
            status, merged, _ = merge(source, advanced, captured)
            assert status == 0
            published = oid(source, "commit-tree", merged, "-p", advanced, "-p", captured, data=b"Publish native candidate\n")
            prepare_ms = (time.perf_counter() - started) * 1000
            candidate = root / "validation"
            started = time.perf_counter()
            if candidate.exists():
                git(candidate, "reset", "--hard", published)
            else:
                git(source, "worktree", "add", "--detach", str(candidate), published)
            validation_ms = (time.perf_counter() - started) * 1000
            assert (candidate / path).read_bytes() == expected
            started = time.perf_counter()
            git(source, "update-ref", "HEAD", published, advanced)
            git(source, "reset", "--hard", published)
            publish_ms = (time.perf_counter() - started) * 1000
            cleanup_ms = 0
            if not reuse_validation:
                started = time.perf_counter()
                git(source, "worktree", "remove", str(candidate))
                cleanup_ms = (time.perf_counter() - started) * 1000
            after = resource.getrusage(resource.RUSAGE_CHILDREN)
            assert (source / path).read_bytes() == expected
            assert (private / path).read_bytes() == b"later save\n"
            assert (private / "ignored/artifact").read_bytes() == b"durable artifact"
            assert oid(private, "rev-parse", "HEAD") == captured
            git(private, "diff", "--cached", "--quiet")
            assert not (private / ".git/objects/info/alternates").exists()
            samples.append({"clone_ms": clone_ms, "capture_ms": capture_ms,
                "transfer_merge_commit_ms": prepare_ms, "source_switch_ms": publish_ms,
                "validation_refresh_ms": validation_ms,
                "validation_cleanup_ms": cleanup_ms,
                "activation_ms": capture_ms + prepare_ms + validation_ms + publish_ms + cleanup_ms,
                "git_user_seconds": after.ru_utime - usage.ru_utime,
                "git_system_seconds": after.ru_stime - usage.ru_stime,
                "git_input_blocks": after.ru_inblock - usage.ru_inblock,
                "git_output_blocks": after.ru_oublock - usage.ru_oublock})
        storage = [entry.stat(follow_symlinks=False) for entry in private.rglob("*")]
        final_cleanup_ms = 0
        if candidate.exists():
            started = time.perf_counter()
            git(source, "worktree", "remove", str(candidate))
            final_cleanup_ms = (time.perf_counter() - started) * 1000
        shutil.rmtree(source)
        git(private, "fsck", "--full")
        assert (private / "ignored/artifact").read_bytes() == b"durable artifact"
        return {"tracked_files": count, "reuse_validation": reuse_validation, "samples": samples,
            "median": {key: statistics.median(sample[key] for sample in samples) for key in samples[0]},
            "private_logical_bytes": sum(entry.st_size for entry in storage),
            "private_allocated_bytes": sum(entry.st_blocks * 512 for entry in storage),
            "final_validation_cleanup_ms": final_cleanup_ms,
            "independent_of_deleted_source": True}


def hardlink_refresh():
    """Characterize Git reads caused by validation links, without changing data."""
    with tempfile.TemporaryDirectory(prefix="workflow-git-links-") as temporary:
        root = Path(temporary)
        repo, view = root / "repo", root / "validation"
        repo.mkdir()
        view.mkdir()
        git(repo, "init", "-q")
        for number in range(4):
            file = repo / f"{number}.bin"
            file.write_bytes(os.urandom(1 << 20))
            os.utime(file, (1700000000, 1700000000))
        git(repo, "add", "-A")
        git(repo, "commit", "-qm", "Allocated assets")
        observations = []

        def status(name, expected_scans):
            result = subprocess.run(
                ["git", "-C", str(repo), "status", "--porcelain", "--untracked-files=no"],
                env=ENV | {"GIT_TRACE2_EVENT": "1"}, capture_output=True,
                check=True, timeout=60)
            events = [json.loads(line) for line in result.stderr.splitlines()]
            scans = [int(event["value"]) for event in events
                     if event.get("key") == "refresh/sum_scan"]
            assert scans == [expected_scans] and result.stdout == b"", (name, scans, result.stdout)
            observations.append({"case": name, "content_scans": scans[0], "clean": True})

        status("before_links", 0)
        before = (repo / "0.bin").stat()
        # Some Git builds compare ctime only to whole-second precision.
        time.sleep(1.05)
        for number in range(4):
            os.link(repo / f"{number}.bin", view / f"{number}.bin")
        after = (repo / "0.bin").stat()
        assert before.st_mtime_ns == after.st_mtime_ns and before.st_ctime_ns != after.st_ctime_ns
        status("after_link_creation", 4)
        status("after_index_refresh", 0)
        time.sleep(1.05)
        shutil.rmtree(view)
        status("after_link_removal", 4)
        status("after_second_refresh", 0)
        return {"full_workflow_qualified": False, "host_git": oid(repo, "--version"),
                "asset_bytes": 4 << 20, "asset_count": 4, "observations": observations,
                "mtime_unchanged_ctime_changed": True,
                "boundary": "native Git Trace2 content-scan counts; unchanged allocated files; no activation or disk-cold timing"}


if __name__ == "__main__":
    if sys.argv[1:] == ["hardlinks"]:
        print(json.dumps(hardlink_refresh(), indent=2))
        raise SystemExit(0)
    if sys.argv[1:] == ["conflict"]:
        print(json.dumps(sparse_native_conflict(), indent=2))
        raise SystemExit(0)
    if sys.argv[1:] in (["native"], ["native-fresh"]):
        print(json.dumps({"native": [native_benchmark(n, sys.argv[1] == "native") for n in (100, 1000, 10000)]}, indent=2))
        raise SystemExit(0)
    if sys.argv[1:]:
        raise SystemExit("usage: git_probe.py [native|native-fresh|conflict|hardlinks]")
    result = {"environment": {"kernel": platform.release(), "arch": platform.machine(),
        "cpus": os.cpu_count(), "git": subprocess.check_output(["git", "--version"], text=True).strip()},
        "correctness": correctness() + [portable_delta(), portable_rename(), sparse_native_conflict()],
        "benchmark": [benchmark(n) for n in (100, 1000, 10000)],
        "binary": binary_benchmark()}
    usage = resource.getrusage(resource.RUSAGE_CHILDREN)
    result["git_subprocesses"] = {"max_rss_kib": usage.ru_maxrss,
                                  "total_user_seconds": usage.ru_utime,
                                  "total_system_seconds": usage.ru_stime}
    print(json.dumps(result, indent=2))
