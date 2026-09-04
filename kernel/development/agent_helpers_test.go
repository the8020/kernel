package development

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentInstallHelpers(t *testing.T) {
	if !strings.Contains(":"+developmentPath+":", ":/root/.local/bin:") {
		t.Fatalf("development PATH does not expose native user binaries: %q", developmentPath)
	}
	var scriptsMount *MountDefinition
	for _, mount := range DefaultMountProfile() {
		if mount.ID == "scripts" {
			current := mount
			scriptsMount = &current
			break
		}
	}
	if scriptsMount == nil || scriptsMount.Target != "/workspace/scripts" || scriptsMount.Behavior != MountReadOnly || scriptsMount.Writable || !scriptsMount.Executable {
		t.Fatalf("development scripts mount is not read-only and executable: %#v", scriptsMount)
	}

	scriptsRoot := filepath.Join("..", "..", "defaults", "scripts")
	for _, name := range []string{"install-codex.sh", "install-claude.sh"} {
		info, err := os.Stat(filepath.Join(scriptsRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable", name)
		}
	}

	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	deno, err := exec.LookPath("deno")
	if err != nil {
		deno, err = filepath.Abs(filepath.Join("..", "..", ".development", "runtime", "development", "rootfs", "usr", "bin", "deno"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(deno); err != nil {
			t.Skip("the materialized development Deno runtime is unavailable")
		}
	}
	if err := os.Symlink(deno, filepath.Join(bin, "deno")); err != nil {
		t.Fatal(err)
	}
	fakeCurl := `#!/bin/sh
case "$*" in
  *chatgpt.com/codex/install.sh*)
    cat <<'INSTALL'
#!/bin/sh
set -eu
test "$CODEX_RELEASE" = latest
test "$CODEX_INSTALL_DIR" = "$HOME/.local/bin"
test "$CODEX_NON_INTERACTIVE" = 1
mkdir -p "$HOME/.local/bin"
cat >"$HOME/.local/bin/codex" <<'COMMAND'
#!/bin/sh
echo 'codex-cli test-version'
COMMAND
chmod 0755 "$HOME/.local/bin/codex"
INSTALL
    ;;
  *claude.ai/install.sh*)
    cat <<'INSTALL'
#!/bin/bash
set -eu
mkdir -p "$HOME/.local/bin"
cat >"$HOME/.local/bin/claude" <<'COMMAND'
#!/bin/sh
echo 'test-version (Claude Code)'
COMMAND
chmod 0755 "$HOME/.local/bin/claude"
INSTALL
    ;;
  *)
    echo "unexpected curl request: $*" >&2
    exit 1
    ;;
esac
`
	writeExecutableTestFile(t, filepath.Join(bin, "curl"), fakeCurl)

	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexOriginal := "model = \"preserved-model\"\napproval_policy = \"on-request\"\n\n[features]\napps = true\n"
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(codexOriginal), 0o600); err != nil {
		t.Fatal(err)
	}

	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeOriginal := `{
  "theme": "dark",
  "permissions": {
    "allow": ["Read"]
  }
}
`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(claudeOriginal), 0o600); err != nil {
		t.Fatal(err)
	}

	testPath := strings.Join([]string{bin, filepath.Join(home, ".local", "bin"), "/usr/bin", "/bin"}, ":")
	for _, test := range []struct {
		script string
		want   []string
	}{
		{"install-codex.sh", []string{"Installing the latest OpenAI Codex", "codex-cli test-version", "👍 Codex installed in YOLO mode", "  codex"}},
		{"install-claude.sh", []string{"Installing the latest Claude Code", "test-version (Claude Code)", "👍 Claude Code installed in YOLO mode", "  claude"}},
	} {
		for run := 0; run < 2; run++ {
			command := exec.Command(filepath.Join(scriptsRoot, test.script))
			command.Env = append(os.Environ(), "HOME="+home, "PATH="+testPath, "CODEX_HOME=", "CLAUDE_CONFIG_DIR=")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%s run %d: %v\n%s", test.script, run+1, err, output)
			}
			for _, wanted := range test.want {
				if !strings.Contains(string(output), wanted) {
					t.Errorf("%s output does not contain %q:\n%s", test.script, wanted, output)
				}
			}
		}
	}

	codexConfig, err := os.ReadFile(filepath.Join(codexDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	codexText := string(codexConfig)
	for _, wanted := range []string{"approval_policy = \"never\"", "sandbox_mode = \"danger-full-access\"", "model = \"preserved-model\"", "[features]", "apps = true"} {
		if !strings.Contains(codexText, wanted) {
			t.Errorf("Codex configuration does not contain %q:\n%s", wanted, codexText)
		}
	}
	if strings.Count(codexText, "approval_policy =") != 1 || strings.Count(codexText, "sandbox_mode =") != 1 {
		t.Errorf("Codex configuration is not idempotent:\n%s", codexText)
	}

	claudeSettings, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	claudeText := string(claudeSettings)
	for _, wanted := range []string{`"theme": "dark"`, `"allow": [`, `"Read"`, `"defaultMode": "bypassPermissions"`, `"skipDangerousModePermissionPrompt": true`} {
		if !strings.Contains(claudeText, wanted) {
			t.Errorf("Claude settings do not contain %q:\n%s", wanted, claudeText)
		}
	}
}

func writeExecutableTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
