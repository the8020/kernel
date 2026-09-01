// Package runtime owns pinned runtime versions and host-readiness diagnostics.
package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type ComponentVersion struct {
	MinimumVersion     string `toml:"minimum_version"`
	RecommendedVersion string `toml:"recommended_version"`
	ArchiveSHA256AMD64 string `toml:"archive_sha256_amd64"`
	ArchiveSHA256ARM64 string `toml:"archive_sha256_arm64"`
}

type GVisorVersion struct {
	Release            string `toml:"release"`
	ArchiveSHA512AMD64 string `toml:"archive_sha512_amd64"`
	ArchiveSHA512ARM64 string `toml:"archive_sha512_arm64"`
	RunscSHA512AMD64   string `toml:"runsc_sha512_amd64"`
	RunscSHA512ARM64   string `toml:"runsc_sha512_arm64"`
	ShimSHA512AMD64    string `toml:"shim_sha512_amd64"`
	ShimSHA512ARM64    string `toml:"shim_sha512_arm64"`
}

type ArchiveVersion struct {
	Version            string `toml:"version"`
	ArchiveSHA256AMD64 string `toml:"archive_sha256_amd64"`
	ArchiveSHA256ARM64 string `toml:"archive_sha256_arm64"`
}

type DenoVersion struct {
	Version                 string `toml:"version"`
	ArchiveSHA256AMD64      string `toml:"archive_sha256_amd64"`
	ArchiveSHA256ARM64      string `toml:"archive_sha256_arm64"`
	BaseImage               string `toml:"base_image"`
	BaseImageDigest         string `toml:"base_image_digest"`
	BaseManifestDigestAMD64 string `toml:"base_manifest_digest_amd64"`
	BaseManifestDigestARM64 string `toml:"base_manifest_digest_arm64"`
}

type RuntimeImageVersion struct {
	Name string `toml:"name"`
}

type Versions struct {
	SchemaVersion          int                 `toml:"schema_version"`
	RuntimeProtocolVersion int                 `toml:"runtime_protocol_version"`
	RuntimeImageSchema     int                 `toml:"runtime_image_schema_version"`
	Containerd             ComponentVersion    `toml:"containerd"`
	GVisor                 GVisorVersion       `toml:"gvisor"`
	CNI                    ArchiveVersion      `toml:"cni"`
	BuildKit               ArchiveVersion      `toml:"buildkit"`
	Deno                   DenoVersion         `toml:"deno"`
	RuntimeImage           RuntimeImageVersion `toml:"runtime_image"`
	DevelopmentImage       RuntimeImageVersion `toml:"development_image"`
}

type ArtifactChecksums struct {
	Containerd    string
	GVisorArchive string
	Runsc         string
	RunscShim     string
	CNIArchive    string
	BuildKit      string
	DenoArchive   string
	DenoManifest  string
}

func LoadVersions(root string) (Versions, error) {
	return LoadVersionsFile(filepath.Join(root, "defaults", "config", "runtime", "versions.toml"))
}

// LoadVersionsFile loads one explicit system image/version definition.
func LoadVersionsFile(path string) (Versions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Versions{}, fmt.Errorf("read runtime version manifest: %w", err)
	}
	var versions Versions
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&versions); err != nil {
		return Versions{}, fmt.Errorf("parse runtime version manifest: %w", err)
	}
	if err := versions.Validate(); err != nil {
		return Versions{}, fmt.Errorf("validate runtime version manifest: %w", err)
	}
	return versions, nil
}

func (v Versions) Validate() error {
	if v.SchemaVersion != 1 || v.RuntimeProtocolVersion < 1 || v.RuntimeImageSchema < 1 {
		return errors.New("schema and runtime versions must be supported positive values")
	}
	for name, value := range map[string]string{
		"containerd minimum":     v.Containerd.MinimumVersion,
		"containerd recommended": v.Containerd.RecommendedVersion,
		"gVisor":                 v.GVisor.Release,
		"CNI":                    v.CNI.Version,
		"BuildKit":               v.BuildKit.Version,
		"Deno":                   v.Deno.Version,
	} {
		if !pinnedVersion(value) {
			return fmt.Errorf("%s version %q is not pinned", name, value)
		}
	}
	for name, value := range map[string]string{
		"gVisor archive amd64": v.GVisor.ArchiveSHA512AMD64,
		"gVisor archive arm64": v.GVisor.ArchiveSHA512ARM64,
		"runsc amd64":          v.GVisor.RunscSHA512AMD64,
		"runsc arm64":          v.GVisor.RunscSHA512ARM64,
		"runsc shim amd64":     v.GVisor.ShimSHA512AMD64,
		"runsc shim arm64":     v.GVisor.ShimSHA512ARM64,
	} {
		if !hexDigest(value, 128) {
			return fmt.Errorf("%s checksum is not SHA-512", name)
		}
	}
	for name, value := range map[string]string{
		"containerd amd64": v.Containerd.ArchiveSHA256AMD64,
		"containerd arm64": v.Containerd.ArchiveSHA256ARM64,
		"CNI amd64":        v.CNI.ArchiveSHA256AMD64,
		"CNI arm64":        v.CNI.ArchiveSHA256ARM64,
		"BuildKit amd64":   v.BuildKit.ArchiveSHA256AMD64,
		"BuildKit arm64":   v.BuildKit.ArchiveSHA256ARM64,
		"Deno amd64":       v.Deno.ArchiveSHA256AMD64,
		"Deno arm64":       v.Deno.ArchiveSHA256ARM64,
	} {
		if !hexDigest(value, 64) {
			return fmt.Errorf("%s checksum is not SHA-256", name)
		}
	}
	for name, value := range map[string]string{
		"Deno base image":     v.Deno.BaseImageDigest,
		"Deno amd64 manifest": v.Deno.BaseManifestDigestAMD64,
		"Deno arm64 manifest": v.Deno.BaseManifestDigestARM64,
	} {
		if !sha256Digest(value) {
			return fmt.Errorf("%s digest is invalid", name)
		}
	}
	if v.Deno.BaseImage == "" || strings.Contains(v.Deno.BaseImage, ":latest") {
		return errors.New("Deno base image must be explicitly versioned")
	}
	if strings.TrimSpace(v.RuntimeImage.Name) == "" || strings.Contains(v.RuntimeImage.Name, ":latest") {
		return errors.New("runtime image name is required and must be pinned")
	}
	if strings.TrimSpace(v.DevelopmentImage.Name) == "" || strings.Contains(v.DevelopmentImage.Name, ":latest") {
		return errors.New("development image name is required and must be pinned")
	}
	return nil
}

func (v Versions) Checksums(architecture string) (ArtifactChecksums, error) {
	switch architecture {
	case "amd64", "x86_64":
		return ArtifactChecksums{v.Containerd.ArchiveSHA256AMD64, v.GVisor.ArchiveSHA512AMD64, v.GVisor.RunscSHA512AMD64, v.GVisor.ShimSHA512AMD64, v.CNI.ArchiveSHA256AMD64, v.BuildKit.ArchiveSHA256AMD64, v.Deno.ArchiveSHA256AMD64, v.Deno.BaseManifestDigestAMD64}, nil
	case "arm64", "aarch64":
		return ArtifactChecksums{v.Containerd.ArchiveSHA256ARM64, v.GVisor.ArchiveSHA512ARM64, v.GVisor.RunscSHA512ARM64, v.GVisor.ShimSHA512ARM64, v.CNI.ArchiveSHA256ARM64, v.BuildKit.ArchiveSHA256ARM64, v.Deno.ArchiveSHA256ARM64, v.Deno.BaseManifestDigestARM64}, nil
	default:
		return ArtifactChecksums{}, fmt.Errorf("unsupported runtime architecture %q", architecture)
	}
}

func pinnedVersion(value string) bool {
	if value == "" || strings.EqualFold(value, "latest") || strings.ContainsAny(value, "*<>=~^") {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, "v"), ".") {
		if part == "" {
			return false
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func hexDigest(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func sha256Digest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && hexDigest(strings.TrimPrefix(value, "sha256:"), 64)
}
