//go:build workflowanalysis

package development

import (
	"context"
	"crypto/rand"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Test the stock overlay mount's gofer upper, not runsc's disposable tmpfs
// filestore. This wrapper affects only the disposable qualification sandbox.
type analysisUpperDriver struct {
	*RunscDriver
	users string
}

func (d analysisUpperDriver) Start(ctx context.Context, start SandboxStart) error {
	upper := filepath.Join(d.users, start.UserID, "dev-sandbox/upper")
	if err := os.MkdirAll(upper, 0700); err != nil {
		return err
	}
	for i := range start.Mounts {
		if start.Mounts[i].Behavior == MountSandboxSource {
			start.Mounts[i].Target = "/workspace/lower"
			start.Mounts[i].Behavior = MountReadOnly
			start.Mounts[i].Writable = false
			start.Mounts[i].Executable = true
		}
	}
	start.Mounts = append(start.Mounts, SandboxMount{
		MountDefinition: MountDefinition{ID: "upper", Target: "/workspace/upper", Behavior: MountPersistent, Writable: true},
		HostSource:      upper,
	})
	if err := d.RunscDriver.Start(ctx, start); err != nil {
		return err
	}
	args := append(d.flags(start.SandboxID, "exec"), "--cap=CAP_SYS_ADMIN", start.SandboxID, "/bin/sh", "-c",
		"mkdir -p /workspace/packages; mount -t overlay overlay -o lowerdir=/workspace/lower,upperdir=/workspace/upper,userxattr /workspace/packages")
	_, err := d.commandOutput(ctx, args...)
	return err
}

func TestWorkflowAnalysisUpper(t *testing.T) {
	m, d, shared := analysisRuntime(t)
	ctx := context.Background()
	m.driver = analysisUpperDriver{d, m.config.UsersRoot}
	// Only this opt-in test grants mount authority inside its gVisor sandbox.
	caps := developmentRootCapabilities
	developmentRootCapabilities = append(append([]string(nil), caps...), "CAP_SYS_ADMIN")
	t.Cleanup(func() { developmentRootCapabilities = caps })
	whiteout := filepath.Join(t.TempDir(), "whiteout")
	t.Logf("native host whiteout creation=%v", unix.Mknod(whiteout, unix.S_IFCHR|0600, 0))
	// Real allocated, incompressible assets; the label edit must copy none of them.
	const assetCount, assetSize = 1024, 64 << 10
	assets := filepath.Join(shared, "assets")
	if err := os.MkdirAll(assets, 0700); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < assetCount; n++ {
		body := make([]byte, assetSize)
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(assets, fmt.Sprintf("%04d.bin", n)), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(shared, "label.txt"), "Old label\n")
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "Asset-heavy fixture"}} {
		if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), args...); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	sandbox, err := m.Create(ctx, "upperproof")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, name := range []string{"runsc-run.log", "process.log", "user.log"} {
				body, _ := os.ReadFile(filepath.Join(d.config.LogRoot, sandbox.SandboxID, name))
				for _, marker := range []string{"panic:", "fatal error:", "SIGSEGV:", "SIGBUS:", "SIGSYS:"} {
					if at := strings.Index(string(body), marker); at >= 0 {
						t.Logf("%s: %s", name, body[at:min(at+2500, len(body))])
					}
				}
			}
		}
		_ = m.Delete(context.Background(), sandbox.UserID)
	})
	t.Logf("initial sandbox with persistent upper=%s; assets=%d bytes in %d files", time.Since(started), assetCount*assetSize, assetCount)
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	for _, command := range []string{"cat untouched.txt", "printf 'New label\n' >label.txt", "mkdir -p ignored; printf durable >ignored/artifact"} {
		t.Logf("native step: %s", command)
		analysisExec(t, d, sandbox, prefix+command)
	}
	upper := filepath.Join(m.sandboxRootForUser(sandbox.UserID), "upper")
	var files, copiedAssets int
	var bytes, allocated int64
	if err := filepath.WalkDir(upper, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		files++
		bytes += st.Size
		allocated += st.Blocks * 512
		if strings.Contains(path, "/assets/") {
			copiedAssets++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("after one label edit and ignored artifact: upper files=%d logical bytes=%d allocated bytes=%d copied asset files=%d", files, bytes, allocated, copiedAssets)
	if copiedAssets != 0 {
		t.Errorf("small edit copied %d unrelated assets", copiedAssets)
	}
	writeTestFile(t, filepath.Join(shared, "replacement.tmp"), "Shared update\n")
	if err := os.Rename(filepath.Join(shared, "replacement.tmp"), filepath.Join(shared, "untouched.txt")); err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat label.txt untouched.txt; stat -c %s untouched.txt"); got != "New label\nShared update\n14\n" {
		t.Errorf("same-package freshness=%q", got)
	}
	if got, err := os.ReadFile(filepath.Join(shared, "label.txt")); err != nil || string(got) != "Old label\n" {
		t.Fatalf("private write escaped: %q, %v", got, err)
	}
	// A retained old descriptor must not make new path lookups return old data.
	analysisExec(t, d, sandbox, prefix+"sh -c 'exec 9<untouched.txt; touch /tmp/upper-ready; for n in $(seq 1 600); do test -f /tmp/upper-release && break; sleep .05; done; cat <&9 >/tmp/upper-held-content' </dev/null >/tmp/upper-held-log 2>&1 &")
	analysisExec(t, d, sandbox, "for n in $(seq 1 100); do test -f /tmp/upper-ready && exit 0; sleep .05; done; exit 1")
	writeTestFile(t, filepath.Join(shared, "replacement.tmp"), "Another update\n")
	if err := os.Rename(filepath.Join(shared, "replacement.tmp"), filepath.Join(shared, "untouched.txt")); err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat untouched.txt"); got != "Another update\n" {
		t.Errorf("open old descriptor leaves fresh lookup stale: %q", got)
	}
	analysisExec(t, d, sandbox, "touch /tmp/upper-release; for n in $(seq 1 100); do test -s /tmp/upper-held-content && exit 0; sleep .05; done; exit 1")
	if got := analysisExec(t, d, sandbox, "cat /tmp/upper-held-content"); got != "Shared update\n" {
		t.Errorf("old descriptor changed identity after shared replacement: %q", got)
	}
	// Native Git must not manufacture changes when shared HEAD advances.
	if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Shared update"); err != nil {
		t.Fatal(err)
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
	if got := analysisExec(t, d, sandbox, prefix+"cat label.txt ignored/artifact"); got != "New label\ndurable" {
		t.Errorf("abrupt recreation lost private files: %q", got)
	}
	t.Log("private label and ignored artifact survived abrupt runtime recreation without a checkpoint")
	t.Logf("native Git after shared publication=%q", analysisExec(t, d, sandbox, prefix+"git status --short"))
	t.Log("This storage probe does not qualify merge-original capture, live upper retirement, native syscall coverage, or production publication.")
}
