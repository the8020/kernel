package model

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testLimits() ResourceLimits {
	return ResourceLimits{PIDMaximum: 128, TmpfsMaximum: 64_000_000}
}

func testProfile() RuntimeProfile {
	return RuntimeProfile{WorkloadType: WorkloadJob, ImageDigest: testDigest, DependencyMode: DependencyCachedOnly, Permissions: Permissions{ReadPaths: []string{"/artifacts/b", "/artifacts/a"}}, Mounts: []Mount{{Source: "/host/b", Target: "/artifacts/b", ReadOnly: true, Purpose: "artifact", Persistence: "execution"}}, NetworkMode: "netstack", DenoStartupFlags: []string{"--no-prompt", "--cached-only"}, ResourceClass: "job-default"}
}

func TestRuntimeProfileHashIsCanonicalAndCompatibilityComplete(t *testing.T) {
	profile := testProfile()
	first, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	profile.Permissions.ReadPaths = []string{"/artifacts/a", "/artifacts/b", "/artifacts/a"}
	profile.DenoStartupFlags = []string{"--cached-only", "--no-prompt"}
	second, err := profile.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("canonical hashes differ: %s != %s", first, second)
	}
	profile.DependencyMode = DependencyOnline
	third, _ := profile.Hash()
	if third == first {
		t.Fatal("dependency mode did not affect compatibility hash")
	}
	profile = testProfile()
	profile.WorkloadType = WorkloadService
	fourth, _ := profile.Hash()
	if fourth == first {
		t.Fatal("workload type did not affect compatibility hash")
	}
	profile = testProfile()
	profile.EgressAllowed = true
	fifth, _ := profile.Hash()
	if fifth == first {
		t.Fatal("egress policy did not affect compatibility hash")
	}
}

func TestRuntimeProfileRejectsEgressPermissionsWhenPolicyDisabled(t *testing.T) {
	profile := testProfile()
	profile.Permissions.NetworkHosts = []string{"example.com:443"}
	if _, err := profile.Hash(); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("error=%v", err)
	}
}

func TestRuntimeProfileRejectsPermissionEscalatingDenoOptions(t *testing.T) {
	profile := testProfile()
	for _, option := range []string{"-A", "--allow-all", "--allow-run=git", "--allow-ffi"} {
		profile.DenoStartupFlags = []string{option}
		if _, err := profile.Hash(); err == nil || !strings.Contains(err.Error(), "unsafe Deno startup option") {
			t.Errorf("option %q error = %v", option, err)
		}
	}
}

func TestSandboxSpecValidation(t *testing.T) {
	profile := testProfile()
	hash, _ := profile.Hash()
	spec := SandboxSpec{SandboxID: "sandbox-1", RuntimeGroupID: "group-1", WorkloadType: WorkloadJob, GroupKey: "job:one", OwnerIDs: []string{"one"}, ImageDigest: testDigest, RuntimeProfile: profile, ProfileHash: hash, ResourceLimits: testLimits(), Network: NetworkConfiguration{Mode: "netstack", NetworkName: "the8020", SandboxIP: "10.88.0.2"}, InternalPorts: []int{8000, 9229}, Mounts: profile.Mounts, Permissions: profile.Permissions, DependencyMode: DependencyCachedOnly, Lifecycle: LifecyclePolicy{StopGracePeriod: 5 * time.Second}}
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}

	tests := []struct {
		name string
		edit func(*SandboxSpec)
		want string
	}{
		{"cross type profile", func(value *SandboxSpec) { value.RuntimeProfile.WorkloadType = WorkloadService }, "profile identity"},
		{"mount outside profile", func(value *SandboxSpec) { value.Mounts = nil }, "mounts and permissions"},
		{"permission outside profile", func(value *SandboxSpec) { value.Permissions.ReadPaths = []string{"/etc"} }, "mounts and permissions"},
		{"mutable image reference", func(value *SandboxSpec) { value.ImageDigest = "denoland/deno:latest" }, "immutable sha256"},
		{"duplicate owner", func(value *SandboxSpec) { value.OwnerIDs = []string{"one", "one"} }, "unique"},
		{"host network", func(value *SandboxSpec) { value.Network.Mode = "host" }, "netstack"},
		{"bad limits", func(value *SandboxSpec) { value.ResourceLimits.PIDMaximum = 0 }, "PID"},
		{"duplicate port", func(value *SandboxSpec) { value.InternalPorts = []int{8000, 8000} }, "duplicate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := spec
			test.edit(&candidate)
			if err := candidate.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestWarmSandboxCannotHaveOwners(t *testing.T) {
	profile := testProfile()
	hash, _ := profile.Hash()
	spec := SandboxSpec{SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: WorkloadJob, GroupKey: "shared", OwnerIDs: []string{"owner"}, ImageDigest: testDigest, RuntimeProfile: profile, ProfileHash: hash, ResourceLimits: testLimits(), Network: NetworkConfiguration{Mode: "netstack", NetworkName: "the8020"}, Mounts: profile.Mounts, Permissions: profile.Permissions, DependencyMode: DependencyCachedOnly, Lifecycle: LifecyclePolicy{Warm: true}}
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "warm sandbox") {
		t.Fatalf("error = %v", err)
	}
}

func TestSandboxStateTransitions(t *testing.T) {
	legal := [][2]SandboxState{{StateCreating, StateStarting}, {StateStarting, StateReady}, {StateReady, StateActive}, {StateActive, StateDraining}, {StateDraining, StateStopping}, {StateStopping, StateStopped}, {StateStopped, StateDeleting}, {StateActive, StateFailed}, {StateFailed, StateDeleting}}
	for _, transition := range legal {
		if !ValidTransition(transition[0], transition[1]) {
			t.Errorf("expected legal transition %s -> %s", transition[0], transition[1])
		}
	}
	illegal := [][2]SandboxState{{StateCreating, StateActive}, {StateStopped, StateReady}, {StateDeleting, StateReady}, {SandboxState("UNKNOWN"), StateReady}}
	for _, transition := range illegal {
		if ValidTransition(transition[0], transition[1]) {
			t.Errorf("accepted illegal transition %s -> %s", transition[0], transition[1])
		}
	}
}

func TestNewID(t *testing.T) {
	first, err := NewID("sandbox")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := NewID("sandbox")
	if first == second || !strings.HasPrefix(first, "sandbox-") {
		t.Fatalf("IDs are not unique/prefixed: %q %q", first, second)
	}
}

func TestNewCompactIDs(t *testing.T) {
	tests := []struct {
		name     string
		pattern  string
		generate func() (string, error)
	}{
		{name: "sandbox", pattern: `^sbx-[a-z0-9]{8}$`, generate: NewSandboxID},
		{name: "runtime group", pattern: `^rgp-[a-z0-9]{8}$`, generate: NewRuntimeGroupID},
		{name: "Worker", pattern: `^wrk-[a-z0-9]{8}$`, generate: NewWorkerID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, err := test.generate()
			if err != nil {
				t.Fatal(err)
			}
			second, err := test.generate()
			if err != nil {
				t.Fatal(err)
			}
			if matched, _ := regexp.MatchString(test.pattern, first); !matched {
				t.Fatalf("unexpected ID %q", first)
			}
			if first == second {
				t.Fatal("compact IDs must be random")
			}
		})
	}
	for _, value := range []string{"", "dev-abc12345", "sbx-short", "sbx-ABC12345", "sbx-abc12345-extra"} {
		if IsSandboxID(value) {
			t.Errorf("invalid sandbox ID %q was accepted", value)
		}
	}
	if !IsSandboxID("sbx-abc12345") {
		t.Fatal("valid sandbox ID was rejected")
	}
}
