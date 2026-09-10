package development

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"the8020/kernel/cbus/core"
)

func installTestDevelopmentAssets(t *testing.T, root string) {
	t.Helper()
	command := exec.Command("bash", "../../install-development-assets.sh", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install development assets: %v\n%s", err, output)
	}
	// Kernel unit tests need the mount/bootstrap contract, not package policy.
	builtin := filepath.Join(root, "packages", "the8020", "dev-skills")
	if _, err := os.Stat(builtin); os.IsNotExist(err) {
		writeTestFile(t, filepath.Join(builtin, "workspace.md"), "Test workspace guidance\n")
		writeTestFile(t, filepath.Join(builtin, "setup-agent-skills.sh"), "exit 0\n")
	}
}

func TestDevelopmentGuidance(t *testing.T) {
	root := t.TempDir()
	installTestDevelopmentAssets(t, root)
	writeTestFile(t, filepath.Join(root, "scripts", "retired.sh"), "stale")
	installTestDevelopmentAssets(t, root)
	if _, err := os.Stat(filepath.Join(root, "scripts", "retired.sh")); !os.IsNotExist(err) {
		t.Fatal("retired installed helpers survived replacement")
	}

	config := Config{Root: root}
	if err := validateMountProfile(config, DefaultMountProfile()); err != nil {
		t.Fatal(err)
	}
	for _, mount := range DefaultMountProfile() {
		if mount.Target == "/workspace/AGENTS.md" || mount.Target == "/workspace/CLAUDE.md" {
			if mount.Source != "packages/the8020/dev-skills/workspace.md" || mount.Behavior != MountReadOnly {
				t.Fatalf("workspace guidance must mount the package-owned file read-only: %+v", mount)
			}
		}
		if mount.Behavior != MountReadOnly {
			continue
		}
		source, err := applicationMountSource(config, mount.Source)
		if err != nil {
			t.Fatal(err)
		}
		spec := developmentSpec(SandboxStart{Mounts: []SandboxMount{{MountDefinition: mount, HostSource: source}}}, root)
		found := false
		for _, actual := range spec.Mounts {
			if actual.Destination == mount.Target {
				found = true
				if !strings.Contains(strings.Join(actual.Options, ","), ",ro,") {
					t.Fatalf("writable guidance mount: %+v", actual)
				}
			}
		}
		if !found {
			t.Fatalf("missing mount %s", mount.Target)
		}
	}
	writeTestFile(t, filepath.Join(root, "node", "private"), "private")
	if err := os.Symlink(filepath.Join(root, "node", "private"), filepath.Join(root, "scripts", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "scripts", "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"node/private", "scripts/escape", "scripts/pipe", "../outside"} {
		if _, err := applicationMountSource(config, source); err == nil {
			t.Fatalf("unsafe read-only source accepted: %s", source)
		}
	}
}

func TestDevelopmentSystemURL(t *testing.T) {
	platform := newTestPlatform(t)
	systemURL := "http://127.0.0.1:18080"
	platform.manager.config.SystemURL = func() string { return systemURL }
	for _, next := range []string{systemURL, "http://127.0.0.1:18081"} {
		systemURL = next
		id, err := platform.manager.EnsureSandbox(context.Background(), "urlproof")
		if err != nil {
			t.Fatal(err)
		}
		platform.driver.mu.Lock()
		start := platform.driver.views[id].start
		platform.driver.mu.Unlock()
		spec := developmentSpec(start, t.TempDir())
		if !slices.Contains(spec.Process.Env, "DEVELOPMENT_SYSTEM_URL="+systemURL) {
			t.Fatalf("sandbox did not receive current system URL %s", systemURL)
		}
		if start.Endpoint == systemURL || start.Endpoint == "" {
			t.Fatal("system URL replaced the separate activation endpoint")
		}
		if _, err := platform.manager.Stop(context.Background(), "urlproof"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRootlessDevelopmentGuidance(t *testing.T) {
	if os.Getenv("THE8020_GUIDANCE_E2E") != "1" {
		t.Skip("set THE8020_GUIDANCE_E2E=1 after portable runtime installation")
	}
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	installTestDevelopmentAssets(t, root)
	builtin := filepath.Join(root, "packages", "the8020", "dev-skills")
	if err := os.RemoveAll(builtin); err != nil {
		t.Fatal(err)
	}
	if err := copyDirectory(context.Background(), filepath.Join(source, "..", "dev-skills"), builtin); err != nil {
		t.Fatalf("guidance E2E requires the sibling dev-skills checkout: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(builtin, ".git")); err != nil {
		t.Fatal(err)
	}
	packages := filepath.Join(root, "packages")
	if err := os.MkdirAll(packages, 0700); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	driver := NewRootlessDriver(RootlessConfig{
		RunscPath:   filepath.Join(source, ".development/runtime/gvisor/bin/runsc"),
		RuntimeRoot: filepath.Join(runtimeRoot, "runsc"), SandboxRoot: filepath.Join(runtimeRoot, "sandboxes"), LogRoot: filepath.Join(runtimeRoot, "logs"),
	})
	registry := core.NewRegistry(nil)
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("host-network-ok")) }))
	defer probe.Close()
	manager, err := New(Config{
		Root: root, PackagesRoot: packages, UsersRoot: filepath.Join(root, "users"), RuntimeRoot: runtimeRoot,
		ImageRoot: filepath.Join(source, ".development/runtime/development/rootfs"), ImageRecord: filepath.Join(source, ".development/runtime/development/image.json"), Driver: driver,
		ActivationGateway: NewCommandBusGateway(registry),
		SystemURL:         func() string { return probe.URL },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	registerTestActivationCommands(t, registry, manager)
	initializeTestRepository(t, manager, "the8020/dev-skills", "Test Developer", "developer@example.test", "Initial skills")
	sandbox, err := manager.Create(context.Background(), "skillproof")
	if err != nil {
		t.Fatal(err)
	}
	check := `set -eu
for i in $(seq 1 100); do test "$(cat /proc/1/comm)" = sleep && break; sleep .05; done
test "$(cat /proc/1/comm)" = sleep
cmp /workspace/AGENTS.md /workspace/CLAUDE.md
for guide in /workspace/AGENTS.md /workspace/CLAUDE.md /workspace/skills/builtin/8020-dev/SKILL.md; do
  test -s "$guide"
  if (printf forbidden >> "$guide") 2>/dev/null; then exit 1; fi
done
for discovery in /root/.agents/skills /root/.claude/skills; do
  for skill in 8020-dev the8020-dev-db the8020-dev-services the8020-dev-uui the8020-dev-types the8020-dev-programs; do
    cmp "$discovery/$skill/SKILL.md" "/workspace/skills/builtin/$skill/SKILL.md"
  done
done
grep -Fq 'always read /workspace/AGENTS.md' /root/.codex/AGENTS.md
grep -Fq 'always read /workspace/AGENTS.md' /root/.claude/CLAUDE.md
curl --fail --silent --max-time 10 "${DEVELOPMENT_SYSTEM_URL:?}" | grep -Fx host-network-ok
`
	execute := func(userID, command string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, err := manager.Shell(ctx, userID, command)
		if err != nil {
			t.Fatalf("native guidance check: %v\n%s", err, output.Output)
		}
		return output.Output
	}
	execute(sandbox.UserID, check)
	execute(sandbox.UserID, `set -eu
mkdir -p /workspace/skills/custom/my-skill /workspace/skills/custom/the8020-dev-types
printf '%s\n' '---' 'name: my-skill' 'description: A private developer test skill.' '---' 'Private skill' > /workspace/skills/custom/my-skill/SKILL.md
printf '%s\n' '---' 'name: the8020-dev-types' 'description: Private types guidance.' '---' 'Private types' > /workspace/skills/custom/the8020-dev-types/SKILL.md
/workspace/scripts/setup-agent-skills.sh
`)
	privateCheck := `set -eu
for discovery in /root/.agents/skills /root/.claude/skills; do
  for skill in my-skill the8020-dev-types; do
    test "$(readlink "$discovery/$skill")" = "/workspace/skills/custom/$skill"
    cmp "$discovery/$skill/SKILL.md" "/workspace/skills/custom/$skill/SKILL.md"
  done
done
`
	execute(sandbox.UserID, privateCheck)
	privateRoot := filepath.Join(root, "users", sandbox.UserID, "dev-sandbox", "skills")
	if info, err := os.Stat(privateRoot); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private skills storage mode: %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(privateRoot, "my-skill", "SKILL.md")); err != nil || !strings.Contains(string(data), "Private skill") {
		t.Fatalf("custom skill did not reach user storage: %q, %v", data, err)
	}
	other, err := manager.Create(context.Background(), "skillother")
	if err != nil {
		t.Fatal(err)
	}
	execute(other.UserID, check+`test ! -e /workspace/skills/custom/my-skill
test ! -e /root/.agents/skills/my-skill
`)

	// Edit through the normal private package view and publish with the real helper.
	execute(sandbox.UserID, `set -eu
printf '\nPublished skill edit\n' >> /workspace/packages/the8020/dev-skills/8020-dev/SKILL.md
printf '\nPublished workspace guide\n' >> /workspace/packages/the8020/dev-skills/workspace.md
mkdir /workspace/packages/the8020/dev-skills/new-shipped
printf '%s\n' '---' 'name: new-shipped' 'description: A new shipped test skill.' '---' 'Shipped skill' > /workspace/packages/the8020/dev-skills/new-shipped/SKILL.md
! grep -Fq 'Published skill edit' /workspace/skills/builtin/8020-dev/SKILL.md
! grep -Fq 'Published workspace guide' /workspace/AGENTS.md
test ! -e /workspace/skills/builtin/new-shipped
`)
	var activation ActivationResult
	output := execute(sandbox.UserID, "activate --json --package the8020/dev-skills --message 'Update shipped skills'")
	if err := json.Unmarshal([]byte(output), &activation); err != nil || !activation.Success {
		t.Fatalf("skill activation: %q, %v", output, err)
	}
	assertActivationComplete(t, manager, sandbox.UserID)
	publishedCheck := `set -eu
grep -Fq 'Published skill edit' /workspace/skills/builtin/8020-dev/SKILL.md
/workspace/scripts/setup-agent-skills.sh
for discovery in /root/.agents/skills /root/.claude/skills; do
  cmp "$discovery/new-shipped/SKILL.md" /workspace/skills/builtin/new-shipped/SKILL.md
done
`
	workspaceCheck := `cmp /workspace/AGENTS.md /workspace/CLAUDE.md
grep -Fq 'Published workspace guide' /workspace/AGENTS.md
`
	execute(sandbox.UserID, publishedCheck+privateCheck+workspaceCheck)
	// Another running sandbox sees the package publication without reinstalling it.
	execute(other.UserID, publishedCheck+"test ! -e /workspace/skills/custom/my-skill\n")
	// File mounts pick up the published guide on the next start.
	if _, err := manager.Restart(context.Background(), other.UserID); err != nil {
		t.Fatal(err)
	}
	execute(other.UserID, check+workspaceCheck)
	if _, err := manager.Restart(context.Background(), sandbox.UserID); err != nil {
		t.Fatal(err)
	}
	execute(sandbox.UserID, publishedCheck+privateCheck+workspaceCheck)
	if _, err := manager.ResetSource(context.Background(), sandbox.UserID, true); err != nil {
		t.Fatal(err)
	}
	execute(sandbox.UserID, publishedCheck+privateCheck)
	if _, err := manager.FactoryReset(context.Background(), sandbox.UserID, true); err != nil {
		t.Fatal(err)
	}
	execute(sandbox.UserID, check+publishedCheck+workspaceCheck+"test ! -e /workspace/skills/custom/my-skill\n")
	t.Log("built-in activation, merged discovery, private persistence/isolation, read-only mounts, host network, and factory reset verified")
}
