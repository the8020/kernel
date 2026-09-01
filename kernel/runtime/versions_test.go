package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadVersionsAndArchitectureChecksums(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	versions, err := LoadVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	if versions.Deno.Version != "2.9.4" || versions.RuntimeImage.Name == "" || versions.DevelopmentImage.Name == "" {
		t.Fatalf("versions: %#v", versions)
	}
	amd64, err := versions.Checksums("x86_64")
	if err != nil || len(amd64.Containerd) != 64 || len(amd64.Runsc) != 128 || !strings.HasPrefix(amd64.DenoManifest, "sha256:") {
		t.Fatalf("amd64 checksums: %#v %v", amd64, err)
	}
	arm64, err := versions.Checksums("arm64")
	if err != nil || arm64.Runsc == amd64.Runsc {
		t.Fatalf("arm64 checksums: %#v %v", arm64, err)
	}
	if _, err := versions.Checksums("mips64"); err == nil {
		t.Fatal("accepted unsupported architecture")
	}
}

func TestLoadVersionsRejectsUnknownAndFloatingValues(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "defaults", "config", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "defaults", "config", "runtime", "versions.toml"))
	if err != nil {
		t.Fatal(err)
	}
	floating := strings.Replace(string(source), `version = "2.9.4"`, `version = "latest"`, 1)
	if err := os.WriteFile(filepath.Join(root, "defaults", "config", "runtime", "versions.toml"), []byte(floating), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVersions(root); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("floating error: %v", err)
	}
	unknown := string(source) + "\nunknown = true\n"
	if err := os.WriteFile(filepath.Join(root, "defaults", "config", "runtime", "versions.toml"), []byte(unknown), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVersions(root); err == nil {
		t.Fatalf("unknown-field error: %v", err)
	}
}

func TestImageSmokeUsesVersionedRuntimeControlEnvelopes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "defaults", "config", "runtime", "build-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, expected := range []string{
		`RUNTIME_PROTOCOL=$(awk`,
		`$RUNTIME_SOURCE/build-image.sh`,
		`SMOKE_RECORD`,
		`smoke-stop.json`,
		`SMOKE_NAMESPACE`,
		`namespaces create "$SMOKE_NAMESPACE"`,
		`snapshots remove "$SMOKE_ID"`,
		`namespaces remove "$SMOKE_NAMESPACE"`,
		`namespaces list -q`,
		`\"message_type\":\"start_worker\"`,
		`\"message_type\":\"job_start\"`,
		`\"message_type\":\"stop_worker\"`,
		`\"runtime_group_id\":\"$SMOKE_ID\"`,
		`\"correlation_id\":\"smoke-start\"`,
		`\"correlation_id\":\"smoke-job\"`,
		`\"correlation_id\":\"smoke-stop\"`,
	} {
		if !strings.Contains(source, expected) {
			t.Errorf("runtime image smoke script is missing %q", expected)
		}
	}
	installer, err := os.ReadFile(filepath.Join("..", "..", "defaults", "config", "runtime", "install-host.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`CURRENT_GVISOR`, `version_at_least "$CURRENT_GVISOR" "$GVISOR_RELEASE"`, `preserving compatible gVisor`} {
		if !strings.Contains(string(installer), expected) {
			t.Errorf("runtime host installer is missing %q", expected)
		}
	}
}
