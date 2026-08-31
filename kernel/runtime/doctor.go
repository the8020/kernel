package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ImageRecord struct {
	SchemaVersion int       `json:"schema_version"`
	Name          string    `json:"name"`
	Digest        string    `json:"digest"`
	BaseDigest    string    `json:"base_digest"`
	DenoVersion   string    `json:"deno_version"`
	SourceHash    string    `json:"source_hash"`
	BuiltAt       time.Time `json:"built_at"`
}

type SmokeRecord struct {
	PassedAt    time.Time `json:"passed_at"`
	ImageDigest string    `json:"image_digest"`
	Runtime     string    `json:"runtime"`
}

type ContainerdProbe interface {
	Version(context.Context) (string, error)
	ImagePresent(context.Context, string) (bool, string, error)
}

type DoctorConfig struct {
	Root             string
	Versions         Versions
	ContainerdSocket string
	CgroupRoot       string
	CNIPluginDir     string
	CNIConfigPath    string
	ImageRecordPath  string
	SmokeRecordPath  string
	OperatingSystem  string
	Architecture     string
	EffectiveUID     int
	PrivilegeProbe   func() bool
	MinimumDiskBytes uint64
	LookupPath       func(string) (string, error)
	Run              func(context.Context, string, ...string) ([]byte, error)
	SocketProbe      func(context.Context, string) error
	CgroupWriteProbe func(string) (bool, error)
	DiskProbe        func(string) (uint64, error)
	Probe            ContainerdProbe
}

type DoctorReport struct {
	LinuxSupported             bool            `json:"linux_supported"`
	Architecture               string          `json:"architecture"`
	ArchitectureSupported      bool            `json:"architecture_supported"`
	HostPrivileges             bool            `json:"host_privileges"`
	DiskBytesAvailable         uint64          `json:"disk_bytes_available"`
	DiskSufficient             bool            `json:"disk_sufficient"`
	ContainerdAvailable        bool            `json:"containerd_available"`
	ContainerdVersion          string          `json:"containerd_version,omitempty"`
	ContainerdVersionSupported bool            `json:"containerd_version_supported"`
	ContainerdSocket           string          `json:"containerd_socket"`
	ContainerdSocketAccessible bool            `json:"containerd_socket_accessible"`
	RunscAvailable             bool            `json:"runsc_available"`
	RunscVersion               string          `json:"runsc_version,omitempty"`
	RunscVersionSupported      bool            `json:"runsc_version_supported"`
	RunscShimAvailable         bool            `json:"runsc_shim_available"`
	CgroupV2                   bool            `json:"cgroup_v2"`
	CgroupWritable             bool            `json:"cgroup_writable"`
	CgroupControllers          []string        `json:"cgroup_controllers"`
	MissingCgroupControllers   []string        `json:"missing_cgroup_controllers,omitempty"`
	NetworkNamespaces          bool            `json:"network_namespaces"`
	CNIPlugins                 map[string]bool `json:"cni_plugins"`
	NetworkConfiguration       bool            `json:"network_configuration"`
	RuntimeImageAvailable      bool            `json:"runtime_image_available"`
	RuntimeImageName           string          `json:"runtime_image_name"`
	RuntimeImageDigest         string          `json:"runtime_image_digest,omitempty"`
	RuntimeImageRecorded       bool            `json:"runtime_image_recorded"`
	GVisorSmokeStatus          string          `json:"gvisor_smoke_status"`
	GVisorSmokePassedAt        time.Time       `json:"gvisor_smoke_passed_at,omitempty"`
	Failures                   []string        `json:"failures,omitempty"`
	Ready                      bool            `json:"ready"`
}

type Doctor struct{ config DoctorConfig }

func NewDoctor(config DoctorConfig) *Doctor {
	if config.ContainerdSocket == "" {
		config.ContainerdSocket = "/run/containerd/containerd.sock"
	}
	if config.CgroupRoot == "" {
		config.CgroupRoot = "/sys/fs/cgroup"
	}
	if config.CNIPluginDir == "" {
		config.CNIPluginDir = "/opt/cni/bin"
	}
	if config.CNIConfigPath == "" {
		config.CNIConfigPath = "/etc/cni/net.d/the8020.conflist"
	}
	if config.OperatingSystem == "" {
		config.OperatingSystem = goruntime.GOOS
	}
	if config.Architecture == "" {
		config.Architecture = goruntime.GOARCH
	}
	if config.EffectiveUID == 0 && os.Geteuid() != 0 {
		config.EffectiveUID = os.Geteuid()
	}
	if config.LookupPath == nil {
		config.LookupPath = exec.LookPath
	}
	if config.PrivilegeProbe == nil {
		effectiveUID := config.EffectiveUID
		config.PrivilegeProbe = func() bool {
			return effectiveUID == 0 && effectiveCapability(21) && effectiveCapability(12)
		}
	}
	if config.Run == nil {
		config.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	if config.SocketProbe == nil {
		config.SocketProbe = func(ctx context.Context, path string) error {
			connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
			if err == nil {
				err = connection.Close()
			}
			return err
		}
	}
	if config.CgroupWriteProbe == nil {
		config.CgroupWriteProbe = filesystemWritable
	}
	if config.MinimumDiskBytes == 0 {
		config.MinimumDiskBytes = 5 << 30
	}
	if config.DiskProbe == nil {
		config.DiskProbe = statFilesystem
	}
	return &Doctor{config: config}
}

func (d *Doctor) Inspect(ctx context.Context) DoctorReport {
	config := d.config
	report := DoctorReport{
		LinuxSupported: config.OperatingSystem == "linux", Architecture: config.Architecture,
		HostPrivileges: config.PrivilegeProbe(), ContainerdSocket: config.ContainerdSocket,
		RuntimeImageName: config.Versions.RuntimeImage.Name, CNIPlugins: map[string]bool{},
		GVisorSmokeStatus: "not_run",
	}
	_, architectureError := config.Versions.Checksums(config.Architecture)
	report.ArchitectureSupported = architectureError == nil
	if _, err := config.LookupPath("containerd"); err == nil {
		report.ContainerdAvailable = true
	}
	probeContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := config.SocketProbe(probeContext, config.ContainerdSocket); err == nil {
		report.ContainerdSocketAccessible = true
	}
	if config.Probe != nil {
		if version, err := config.Probe.Version(probeContext); err == nil {
			report.ContainerdAvailable = true
			report.ContainerdSocketAccessible = true
			report.ContainerdVersion = normalizedVersion(version)
		}
	}
	if report.ContainerdVersion == "" && report.ContainerdAvailable {
		if output, err := config.Run(probeContext, "containerd", "--version"); err == nil {
			report.ContainerdVersion = firstVersion(string(output))
		}
	}
	report.ContainerdVersionSupported = versionAtLeast(report.ContainerdVersion, config.Versions.Containerd.MinimumVersion)
	if _, err := config.LookupPath("runsc"); err == nil {
		report.RunscAvailable = true
		if output, runErr := config.Run(probeContext, "runsc", "--version"); runErr == nil {
			report.RunscVersion = strings.TrimSpace(string(output))
			report.RunscVersionSupported = versionAtLeast(gvisorRelease(report.RunscVersion), config.Versions.GVisor.Release)
		}
	}
	if _, err := config.LookupPath("containerd-shim-runsc-v1"); err == nil {
		report.RunscShimAvailable = true
	}
	controllers, err := readWords(filepath.Join(config.CgroupRoot, "cgroup.controllers"))
	if err == nil {
		report.CgroupV2 = true
		report.CgroupControllers = controllers
		if writable, probeErr := config.CgroupWriteProbe(config.CgroupRoot); probeErr == nil {
			report.CgroupWritable = writable
		}
	}
	for _, required := range []string{"cpu", "memory", "pids"} {
		if !contains(controllers, required) {
			report.MissingCgroupControllers = append(report.MissingCgroupControllers, required)
		}
	}
	if _, err := os.Stat("/proc/self/ns/net"); err == nil {
		report.NetworkNamespaces = true
	}
	for _, plugin := range []string{"bridge", "host-local", "loopback"} {
		info, statErr := os.Stat(filepath.Join(config.CNIPluginDir, plugin))
		report.CNIPlugins[plugin] = statErr == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
	}
	if info, err := os.Stat(config.CNIConfigPath); err == nil && info.Mode().IsRegular() {
		report.NetworkConfiguration = true
	}
	if stats, err := config.DiskProbe(config.Root); err == nil {
		report.DiskBytesAvailable = stats
		report.DiskSufficient = stats >= config.MinimumDiskBytes
	}
	var image ImageRecord
	if readJSON(config.ImageRecordPath, &image) == nil && image.SchemaVersion == config.Versions.RuntimeImageSchema && image.Name == config.Versions.RuntimeImage.Name && sha256Digest(image.Digest) && sha256Digest(image.SourceHash) && image.BaseDigest == config.Versions.Deno.BaseImageDigest && image.DenoVersion == config.Versions.Deno.Version {
		report.RuntimeImageRecorded = true
		report.RuntimeImageDigest = image.Digest
		if config.Probe != nil {
			present, name, probeErr := config.Probe.ImagePresent(probeContext, image.Digest)
			report.RuntimeImageAvailable = probeErr == nil && present && (name == "" || name == image.Name)
		} else {
			report.RuntimeImageAvailable = report.ContainerdSocketAccessible
		}
	}
	var smoke SmokeRecord
	if readJSON(config.SmokeRecordPath, &smoke) == nil && !smoke.PassedAt.IsZero() && smoke.Runtime == "io.containerd.runsc.v1" && smoke.ImageDigest == report.RuntimeImageDigest {
		report.GVisorSmokeStatus = "passed"
		report.GVisorSmokePassedAt = smoke.PassedAt
	}
	report.Failures = doctorFailures(report)
	report.Ready = len(report.Failures) == 0
	return report
}

func doctorFailures(report DoctorReport) []string {
	checks := []struct {
		ok      bool
		failure string
	}{
		{report.LinuxSupported, "host operating system is not Linux"},
		{report.ArchitectureSupported, "host architecture is unsupported"},
		{report.HostPrivileges, "runtime host privileges are unavailable"},
		{report.DiskSufficient, "runtime host has insufficient free disk space"},
		{report.ContainerdAvailable, "containerd is unavailable"},
		{report.ContainerdVersionSupported, "containerd version is unsupported"},
		{report.ContainerdSocketAccessible, "containerd socket is inaccessible"},
		{report.RunscAvailable, "runsc is unavailable"},
		{report.RunscVersionSupported, "gVisor version is unsupported"},
		{report.RunscShimAvailable, "containerd-shim-runsc-v1 is unavailable"},
		{report.CgroupV2, "cgroup v2 is unavailable"},
		{report.CgroupWritable, "cgroup v2 filesystem is read-only or not delegated"},
		{len(report.MissingCgroupControllers) == 0, "required cgroup controllers are unavailable"},
		{report.NetworkNamespaces, "network namespaces are unavailable"},
		{allPlugins(report.CNIPlugins), "required CNI plugins are unavailable"},
		{report.NetworkConfiguration, "CNI network configuration is unavailable"},
		{report.RuntimeImageRecorded, "runtime image record is unavailable or stale"},
		{report.RuntimeImageAvailable, "runtime image is unavailable in containerd"},
		{report.GVisorSmokeStatus == "passed", "gVisor smoke test has not passed for the current image"},
	}
	var failures []string
	for _, check := range checks {
		if !check.ok {
			failures = append(failures, check.failure)
		}
	}
	return failures
}

func gvisorRelease(output string) string {
	for _, field := range strings.Fields(output) {
		field = strings.Trim(field, ",;()")
		if strings.HasPrefix(field, "release-") {
			return strings.TrimPrefix(field, "release-")
		}
	}
	return ""
}

func readWords(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	words := strings.Fields(string(data))
	sort.Strings(words)
	return words, nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func statFilesystem(path string) (uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return stats.Bavail * uint64(stats.Bsize), nil
}

func filesystemWritable(path string) (bool, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return false, err
	}
	const readOnlyFilesystem = 1
	return stats.Flags&readOnlyFilesystem == 0, nil
}

func firstVersion(output string) string {
	for _, field := range strings.Fields(output) {
		candidate := normalizedVersion(field)
		if _, _, ok := parseVersion(candidate); ok {
			return candidate
		}
	}
	return ""
}

func normalizedVersion(value string) string {
	return strings.Trim(strings.TrimPrefix(strings.TrimSpace(value), "v"), ",;()")
}

func versionAtLeast(actual, minimum string) bool {
	aMajor, aRest, aOK := parseVersion(normalizedVersion(actual))
	mMajor, mRest, mOK := parseVersion(normalizedVersion(minimum))
	if !aOK || !mOK {
		return false
	}
	a := append([]int{aMajor}, aRest...)
	m := append([]int{mMajor}, mRest...)
	for index := 0; index < len(a) && index < len(m); index++ {
		if a[index] != m[index] {
			return a[index] > m[index]
		}
	}
	return len(a) >= len(m)
}

func parseVersion(value string) (int, []int, bool) {
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, nil, false
	}
	values := make([]int, len(parts))
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return 0, nil, false
		}
		values[index] = value
	}
	return values[0], values[1:], true
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func allPlugins(plugins map[string]bool) bool {
	for _, name := range []string{"bridge", "host-local", "loopback"} {
		if !plugins[name] {
			return false
		}
	}
	return true
}
