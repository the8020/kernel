//go:build workflowanalysis

package development

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Gate stock-runsc configuration before attempting a custom filesystem. All
// shared writes and destructive lifecycle operations use analysisRuntime's
// disposable source tree and sandbox; no production mount is changed.
func TestWorkflowAnalysisCoherence(t *testing.T) {
	m, d, repository := analysisRuntime(t)
	ctx := context.Background()
	writeTestFile(t, filepath.Join(repository, ".gitignore"), "ignored/\nbench/\n")
	for n := 0; n < 10000; n++ {
		writeTestFile(t, filepath.Join(repository, "bench", fmt.Sprintf("%05d.txt", n)), "small file\n")
	}
	writeTestFile(t, filepath.Join(repository, "replace.txt"), "old inode\n")
	writeTestFile(t, filepath.Join(repository, "type.txt"), "old type\n")
	writeTestFile(t, filepath.Join(repository, "removed.txt"), "remove me\n")
	writeTestFile(t, filepath.Join(repository, "directory/child"), "child\n")
	if _, err := gitCommand(ctx, repository, gitIdentity("Fixture", "fixture@example.test"), "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, repository, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "Coherence baseline"); err != nil {
		t.Fatal(err)
	}
	sandbox, err := m.Create(ctx, "coherence")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Delete(context.Background(), sandbox.UserID) })
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	// Populate lower inode, directory, negative lookup, and native Git caches.
	analysisExec(t, d, sandbox, prefix+"git status --porcelain; git hash-object untouched.txt replace.txt type.txt removed.txt; stat untouched.txt replace.txt type.txt directory/child >/dev/null; test ! -e added.txt; printf 'private\n' >same.txt; printf durable >/root/coherence-proof")
	writeTestFile(t, filepath.Join(repository, "untouched.txt"), "shared-advance\n")
	writeTestFile(t, filepath.Join(repository, "replacement.tmp"), "replacement with a new size\n")
	if err := os.Rename(filepath.Join(repository, "replacement.tmp"), filepath.Join(repository, "replace.txt")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"type.txt", "removed.txt"} {
		if err := os.Remove(filepath.Join(repository, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("replace.txt", filepath.Join(repository, "type.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repository, "directory"), filepath.Join(repository, "renamed")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repository, "added.txt"), "new file\n")
	if _, err := gitCommand(ctx, repository, gitIdentity("Fixture", "fixture@example.test"), "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, repository, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", "Shared mutation"); err != nil {
		t.Fatal(err)
	}
	head, err := gitCommand(ctx, repository, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ name, command, want string }{
		{"size", "stat -c %s untouched.txt", "15\n"},
		{"content", "cat untouched.txt", "shared-advance\n"},
		{"replacement", "cat replace.txt", "replacement with a new size\n"},
		{"replacement-size", "stat -c %s replace.txt", "28\n"},
		{"type", "readlink type.txt || true", "replace.txt\n"},
		{"removal", "if test -e removed.txt; then echo stale; else echo gone; fi", "gone\n"},
		{"directory-rename", "if test -e directory/child; then echo stale; fi; cat renamed/child", "child\n"},
		{"negative-cache", "cat added.txt", "new file\n"},
		{"private-isolation", "cat same.txt", "private\n"},
		{"new-git-object", "git cat-file -t " + strings.TrimSpace(string(head)), "commit\n"},
	} {
		t.Run(check.name, func(t *testing.T) {
			got, err := d.Exec(ctx, sandbox.SandboxID, prefix+check.command)
			if err != nil || string(got) != check.want {
				t.Errorf("want %q, got %q, error=%v", check.want, got, err)
			}
		})
	}
	pathHash := analysisExec(t, d, sandbox, prefix+"git hash-object untouched.txt")
	streamHash := analysisExec(t, d, sandbox, prefix+"cat untouched.txt | git hash-object --stdin")
	if pathHash != streamHash {
		t.Errorf("path hash=%q differs from full stream=%q", pathHash, streamHash)
	}
	shared, err := os.ReadFile(filepath.Join(repository, "same.txt"))
	if err != nil || string(shared) != "base\n" {
		t.Fatalf("private write escaped: %q, %v", shared, err)
	}
	status, err := d.Exec(ctx, sandbox.SandboxID, prefix+"git status --short")
	t.Logf("native status after shared mutations=%q error=%v", status, err)
	// Warm and first traversal, with a fixed newly created 10k-file tree. These
	// are cache observations, not host page-cache-controlled cold measurements.
	for n := 0; n < 4; n++ {
		started := time.Now()
		analysisExec(t, d, sandbox, prefix+"find bench -type f -exec cat {} + >/dev/null")
		t.Logf("10k traversal sample=%d duration=%s", n, time.Since(started))
	}
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, "cat /root/coherence-proof"); got != "durable" {
		t.Fatalf("system root lost persistence: %q", got)
	}
	t.Logf("private source after abrupt runtime recreation=%q", analysisExec(t, d, sandbox, prefix+"cat same.txt"))
}
