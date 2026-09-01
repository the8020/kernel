package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeAuto        Mode = "auto"
	ModeFull        Mode = "full"
	ModeRootless    Mode = "rootless"
	ModeUnavailable Mode = "unavailable"
)

type RootlessRecord struct {
	SchemaVersion int       `json:"schema_version"`
	ImageDigest   string    `json:"image_digest"`
	DenoVersion   string    `json:"deno_version"`
	SourceHash    string    `json:"source_hash"`
	RunscRelease  string    `json:"runsc_release"`
	BuiltAt       time.Time `json:"built_at"`
}

type RootlessSmokeRecord struct {
	PassedAt    time.Time `json:"passed_at"`
	ImageDigest string    `json:"image_digest"`
	Runtime     string    `json:"runtime"`
}

type RootlessDoctorConfig struct {
	Root            string
	Versions        Versions
	RunscPath       string
	RootFS          string
	RecordPath      string
	SmokeRecordPath string
	OperatingSystem string
	Architecture    string
	Run             func(context.Context, string, ...string) ([]byte, error)
	CapabilityProbe func(int) bool
	SeccompProbe    func() bool
}

type RootlessReport struct {
	LinuxSupported        bool      `json:"linux_supported"`
	Architecture          string    `json:"architecture"`
	ArchitectureSupported bool      `json:"architecture_supported"`
	RunscPath             string    `json:"runsc_path"`
	RunscAvailable        bool      `json:"runsc_available"`
	RunscVersion          string    `json:"runsc_version,omitempty"`
	RunscVersionSupported bool      `json:"runsc_version_supported"`
	RootFS                string    `json:"rootfs"`
	RootFSAvailable       bool      `json:"rootfs_available"`
	RuntimeRecorded       bool      `json:"runtime_recorded"`
	RuntimeImageDigest    string    `json:"runtime_image_digest,omitempty"`
	SysChrootCapability   bool      `json:"sys_chroot_capability"`
	SeccompCompatible     bool      `json:"seccomp_compatible"`
	GVisorSmokeStatus     string    `json:"gvisor_smoke_status"`
	GVisorSmokePassedAt   time.Time `json:"gvisor_smoke_passed_at,omitempty"`
	Failures              []string  `json:"failures,omitempty"`
	Ready                 bool      `json:"ready"`
}

type RootlessDoctor struct{ config RootlessDoctorConfig }

func NewRootlessDoctor(config RootlessDoctorConfig) *RootlessDoctor {
	if config.OperatingSystem == "" {
		config.OperatingSystem = goruntime.GOOS
	}
	if config.Architecture == "" {
		config.Architecture = goruntime.GOARCH
	}
	if config.Run == nil {
		config.Run = func(ctx context.Context, name string, arguments ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, arguments...).CombinedOutput()
		}
	}
	if config.CapabilityProbe == nil {
		config.CapabilityProbe = effectiveCapability
	}
	if config.SeccompProbe == nil {
		config.SeccompProbe = seccompUnconfined
	}
	return &RootlessDoctor{config: config}
}

func (d *RootlessDoctor) Inspect(ctx context.Context) RootlessReport {
	config := d.config
	report := RootlessReport{
		LinuxSupported: config.OperatingSystem == "linux", Architecture: config.Architecture,
		RunscPath: config.RunscPath, RootFS: config.RootFS, GVisorSmokeStatus: "not_run",
		SysChrootCapability: config.CapabilityProbe(18), SeccompCompatible: config.SeccompProbe(),
	}
	_, architectureError := config.Versions.Checksums(config.Architecture)
	report.ArchitectureSupported = architectureError == nil
	if info, err := os.Stat(config.RunscPath); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		report.RunscAvailable = true
		probeContext, cancel := context.WithTimeout(ctx, 3*time.Second)
		output, runErr := config.Run(probeContext, config.RunscPath, "--version")
		cancel()
		if runErr == nil {
			report.RunscVersion = strings.TrimSpace(string(output))
			report.RunscVersionSupported = versionAtLeast(gvisorRelease(report.RunscVersion), config.Versions.GVisor.Release)
		}
	}
	for _, relative := range []string{"usr/bin/deno", "opt/runtime/supervisor/main.ts", "opt/runtime/worker/bootstrap.ts", "opt/runtime/protocol.ts"} {
		info, err := os.Stat(filepath.Join(config.RootFS, filepath.FromSlash(relative)))
		if err != nil || !info.Mode().IsRegular() {
			report.RootFSAvailable = false
			break
		}
		report.RootFSAvailable = true
	}
	var record RootlessRecord
	if readJSON(config.RecordPath, &record) == nil && record.SchemaVersion == 1 && sha256Digest(record.ImageDigest) && sha256Digest(record.SourceHash) && record.DenoVersion == config.Versions.Deno.Version && versionAtLeast(record.RunscRelease, config.Versions.GVisor.Release) {
		report.RuntimeRecorded = true
		report.RuntimeImageDigest = record.ImageDigest
	}
	var smoke RootlessSmokeRecord
	if readJSON(config.SmokeRecordPath, &smoke) == nil && !smoke.PassedAt.IsZero() && smoke.Runtime == "runsc-rootless-systrap" && smoke.ImageDigest == report.RuntimeImageDigest {
		report.GVisorSmokeStatus = "passed"
		report.GVisorSmokePassedAt = smoke.PassedAt
	}
	checks := []struct {
		ok      bool
		failure string
	}{
		{report.LinuxSupported, "host operating system is not Linux"},
		{report.ArchitectureSupported, "host architecture is unsupported"},
		{report.RunscAvailable, "node-local runsc is unavailable"},
		{report.RunscVersionSupported, "node-local gVisor version is unsupported"},
		{report.RootFSAvailable, "portable runtime rootfs is unavailable"},
		{report.RuntimeRecorded, "portable runtime record is unavailable or stale"},
		{report.SysChrootCapability, "CAP_SYS_CHROOT is unavailable"},
		{report.SeccompCompatible, "outer seccomp profile is incompatible with gVisor systrap"},
		{report.GVisorSmokeStatus == "passed", "rootless gVisor smoke test has not passed for the current runtime"},
	}
	for _, check := range checks {
		if !check.ok {
			report.Failures = append(report.Failures, check.failure)
		}
	}
	report.Ready = len(report.Failures) == 0
	return report
}

type IsolationReport struct {
	ConfiguredMode  string          `json:"configured_mode"`
	SelectedMode    string          `json:"selected_mode"`
	SelectionReason string          `json:"selection_reason"`
	FullReady       bool            `json:"full_ready"`
	RootlessReady   bool            `json:"rootless_ready"`
	Capabilities    map[string]bool `json:"capabilities"`
	Limitations     []string        `json:"limitations,omitempty"`
}

func SelectMode(configured string, fullReady, rootlessReady bool) (Mode, string, error) {
	mode := Mode(strings.TrimSpace(configured))
	if mode == "" {
		mode = ModeAuto
	}
	switch mode {
	case ModeAuto:
		if fullReady {
			return ModeFull, "full containerd/CNI/cgroup isolation is available", nil
		}
		if rootlessReady {
			return ModeRootless, "full host isolation is unavailable; selected direct rootless gVisor", nil
		}
		return ModeUnavailable, "neither full nor rootless gVisor prerequisites are available", errors.New("no supported sandbox runtime mode is ready")
	case ModeFull:
		if !fullReady {
			return ModeUnavailable, "full mode was requested but its host prerequisites are unavailable", errors.New("configured full sandbox runtime is not ready")
		}
		return ModeFull, "full mode was explicitly configured", nil
	case ModeRootless:
		if !rootlessReady {
			return ModeUnavailable, "rootless mode was requested but its prerequisites are unavailable", errors.New("configured rootless sandbox runtime is not ready")
		}
		return ModeRootless, "rootless mode was explicitly configured", nil
	default:
		return ModeUnavailable, "configured mode is invalid", fmt.Errorf("unsupported sandbox runtime mode %q", configured)
	}
}

func NewIsolationReport(configured string, selected Mode, reason string, fullReady, rootlessReady bool) IsolationReport {
	report := IsolationReport{
		ConfiguredMode: configured, SelectedMode: string(selected), SelectionReason: reason, FullReady: fullReady, RootlessReady: rootlessReady,
		Capabilities: map[string]bool{"gvisor": selected == ModeFull || selected == ModeRootless, "filesystem_isolation": selected == ModeFull || selected == ModeRootless, "process_isolation": selected == ModeFull || selected == ModeRootless},
	}
	if selected == ModeFull {
		report.Capabilities["network_namespace"] = true
		report.Capabilities["network_firewall"] = true
		report.Capabilities["hard_resource_limits"] = true
		report.Capabilities["rootless"] = false
	}
	if selected == ModeRootless {
		report.Capabilities["network_namespace"] = false
		report.Capabilities["network_firewall"] = false
		report.Capabilities["hard_resource_limits"] = false
		report.Capabilities["rootless"] = true
		report.Limitations = []string{
			"uses host networking with loopback-only kernel control endpoints instead of a CNI network namespace",
			"Deno permissions restrict program egress, but no host nftables policy backs that restriction",
			"resource usage is observable through runsc and kernel-owned sandbox processes, but configured CPU, memory, and PID limits are not hard-enforced by cgroup v2",
		}
	}
	return report
}

func effectiveCapability(bit int) bool {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "CapEff:" {
			value, parseErr := strconv.ParseUint(fields[1], 16, 64)
			return parseErr == nil && bit >= 0 && bit < 64 && value&(uint64(1)<<bit) != 0
		}
	}
	return false
}

func seccompUnconfined() bool {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "Seccomp:" {
			return fields[1] == "0"
		}
	}
	return false
}
