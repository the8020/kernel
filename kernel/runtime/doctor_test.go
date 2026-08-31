package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeContainerdProbe struct {
	version string
	present bool
	name    string
	err     error
}

func (p fakeContainerdProbe) Version(context.Context) (string, error) { return p.version, p.err }
func (p fakeContainerdProbe) ImagePresent(context.Context, string) (bool, string, error) {
	return p.present, p.name, p.err
}

func TestDoctorReportsHealthyPinnedRuntime(t *testing.T) {
	root := t.TempDir()
	versions := testVersions(t)
	cgroup := filepath.Join(root, "cgroup")
	cni := filepath.Join(root, "cni")
	if err := os.MkdirAll(cgroup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cni, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroup, "cgroup.controllers"), []byte("pids memory cpu io\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range []string{"bridge", "host-local", "loopback"} {
		if err := os.WriteFile(filepath.Join(cni, plugin), []byte("plugin"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "the8020.conflist")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + string(make([]byte, 0))
	digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imagePath := filepath.Join(root, "image.json")
	writeJSON(t, imagePath, ImageRecord{SchemaVersion: 1, Name: versions.RuntimeImage.Name, Digest: digest, BaseDigest: versions.Deno.BaseImageDigest, DenoVersion: versions.Deno.Version, SourceHash: digest, BuiltAt: time.Now()})
	smokePath := filepath.Join(root, "smoke.json")
	writeJSON(t, smokePath, SmokeRecord{PassedAt: time.Now(), ImageDigest: digest, Runtime: "io.containerd.runsc.v1"})
	doctor := NewDoctor(DoctorConfig{
		Root: root, Versions: versions, CgroupRoot: cgroup, CNIPluginDir: cni, CNIConfigPath: configPath,
		ImageRecordPath: imagePath, SmokeRecordPath: smokePath, OperatingSystem: "linux", Architecture: "arm64", EffectiveUID: 0,
		PrivilegeProbe: func() bool { return true },
		LookupPath:     func(name string) (string, error) { return "/bin/" + name, nil },
		Run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "runsc" {
				return []byte("runsc version release-20260817.0\n"), nil
			}
			return []byte("containerd github.com/containerd/containerd/v2 2.3.1\n"), nil
		},
		SocketProbe: func(context.Context, string) error { return nil },
		Probe:       fakeContainerdProbe{version: "2.3.1", present: true, name: versions.RuntimeImage.Name},
	})
	report := doctor.Inspect(context.Background())
	if !report.Ready || len(report.Failures) != 0 || !report.DiskSufficient || !report.CgroupWritable || !report.RunscVersionSupported || !report.RuntimeImageAvailable || report.GVisorSmokeStatus != "passed" {
		t.Fatalf("report: %#v", report)
	}
}

func TestGVisorReleaseRequiresPinnedOrNewerVersion(t *testing.T) {
	if release := gvisorRelease("runsc version release-20260817.0\n"); release != "20260817.0" || !versionAtLeast(release, "20260817.0") {
		t.Fatalf("pinned release=%q", release)
	}
	if release := gvisorRelease("runsc version release-20260818.0\n"); !versionAtLeast(release, "20260817.0") {
		t.Fatalf("newer release=%q", release)
	}
	if release := gvisorRelease("runsc version release-20260816.1\n"); versionAtLeast(release, "20260817.0") {
		t.Fatalf("older release accepted=%q", release)
	}
}

func TestDoctorRejectsInsufficientRuntimeDisk(t *testing.T) {
	doctor := NewDoctor(DoctorConfig{
		Root: t.TempDir(), Versions: testVersions(t), OperatingSystem: "linux", Architecture: "arm64", EffectiveUID: 0,
		LookupPath: func(string) (string, error) { return "", errors.New("missing") }, SocketProbe: func(context.Context, string) error { return errors.New("missing") },
		MinimumDiskBytes: 100, DiskProbe: func(string) (uint64, error) { return 99, nil },
	})
	report := doctor.Inspect(context.Background())
	if report.DiskSufficient || report.DiskBytesAvailable != 99 || !contains(report.Failures, "runtime host has insufficient free disk space") {
		t.Fatalf("report=%#v", report)
	}
}

func TestDoctorRejectsReadOnlyCgroupFilesystem(t *testing.T) {
	root := t.TempDir()
	cgroup := filepath.Join(root, "cgroup")
	if err := os.Mkdir(cgroup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroup, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doctor := NewDoctor(DoctorConfig{
		Root: root, Versions: testVersions(t), CgroupRoot: cgroup,
		OperatingSystem: "linux", Architecture: "arm64", EffectiveUID: 0,
		LookupPath:       func(string) (string, error) { return "", errors.New("missing") },
		SocketProbe:      func(context.Context, string) error { return errors.New("missing") },
		CgroupWriteProbe: func(string) (bool, error) { return false, nil },
	})
	report := doctor.Inspect(context.Background())
	if !report.CgroupV2 || report.CgroupWritable || !contains(report.Failures, "cgroup v2 filesystem is read-only or not delegated") {
		t.Fatalf("report: %#v", report)
	}
}

func TestDoctorReportsEveryUnavailableCapability(t *testing.T) {
	root := t.TempDir()
	versions := testVersions(t)
	doctor := NewDoctor(DoctorConfig{
		Root: root, Versions: versions, CgroupRoot: filepath.Join(root, "missing"), CNIPluginDir: filepath.Join(root, "missing-cni"),
		CNIConfigPath: filepath.Join(root, "missing.conflist"), ImageRecordPath: filepath.Join(root, "missing-image"), SmokeRecordPath: filepath.Join(root, "missing-smoke"),
		OperatingSystem: "darwin", Architecture: "mips64", EffectiveUID: 1000,
		LookupPath:  func(string) (string, error) { return "", errors.New("missing") },
		SocketProbe: func(context.Context, string) error { return errors.New("denied") },
	})
	report := doctor.Inspect(context.Background())
	if report.Ready || len(report.Failures) < 10 || report.ContainerdAvailable || report.RuntimeImageRecorded {
		t.Fatalf("degraded report: %#v", report)
	}
}

func testVersions(t *testing.T) Versions {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	versions, err := LoadVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
