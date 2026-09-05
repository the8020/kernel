// Package discovery indexes command descriptors from active packages.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/pelletier/go-toml/v2"

	"the8020/kernel/cbus/core"
	"the8020/kernel/deployment"
	programrunner "the8020/kernel/execution/programs"
	"the8020/kernel/execution/supervisor"
	workspacepackages "the8020/kernel/packages"
)

const commandManifestLimit = 1 << 20

var commandSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type PackageSource interface {
	ListPackageIndexes() ([]workspacepackages.PackageIndex, error)
	InspectPackageIndex(string) (workspacepackages.PackageIndex, error)
	ActivatedPackageCommit(context.Context, string) (string, error)
	PackagesRoot() string
}

type ProgramRunner interface {
	Run(context.Context, string, string, []any, map[string]string) (programrunner.Result, error)
}

type Registry interface {
	ReplacePackages([]core.Registration, []core.Diagnostic) error
	Catalog() core.Catalog
}

type Indexer struct {
	packages    PackageSource
	programs    ProgramRunner
	registry    Registry
	mu          sync.Mutex
	fragments   map[string]fragment
	diagnostics map[string]core.Diagnostic
}

type Report struct {
	Revision    string            `json:"revision"`
	Packages    []string          `json:"packages"`
	Commands    int               `json:"commands"`
	Diagnostics []core.Diagnostic `json:"diagnostics,omitempty"`
}

type commandManifest struct {
	Version         int                `toml:"version"`
	Command         string             `toml:"command"`
	Program         string             `toml:"program"`
	Summary         string             `toml:"summary"`
	Description     string             `toml:"description,omitempty"`
	Usage           string             `toml:"usage,omitempty"`
	MutatesState    bool               `toml:"mutates_state"`
	RestartBehavior string             `toml:"restart_behavior"`
	Examples        []commandExample   `toml:"examples,omitempty"`
	Secrets         []core.SecretInput `toml:"secrets,omitempty"`
}

type commandExample struct {
	Command string `toml:"command"`
}

type fragment struct {
	packageID     string
	registrations []core.Registration
}

func New(packages PackageSource, programs ProgramRunner, registry Registry) (*Indexer, error) {
	if packages == nil || programs == nil || registry == nil {
		return nil, errors.New("package source, program runner, and registry are required")
	}
	return &Indexer{packages: packages, programs: programs, registry: registry}, nil
}

// Reindex replaces selected package fragments, or all fragments when omitted.
// Filesystem discovery never reads unselected packages; collision validation
// still covers the complete cached catalog.
func (i *Indexer) Reindex(ctx context.Context, packageIDs ...string) (Report, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, id := range packageIDs {
		if _, err := workspacepackages.ParsePackageID(id); err != nil {
			return Report{}, err
		}
	}
	cached := map[string]fragment{}
	invalid := map[string]core.Diagnostic{}
	var entries []workspacepackages.PackageIndex
	if len(packageIDs) == 0 {
		var err error
		entries, err = i.packages.ListPackageIndexes()
		if err != nil {
			return Report{}, fmt.Errorf("list active packages: %w", err)
		}
	} else {
		for id, item := range i.fragments {
			cached[id] = item
		}
		for id, item := range i.diagnostics {
			invalid[id] = item
		}
		for _, id := range uniqueStrings(packageIDs) {
			entry, err := i.packages.InspectPackageIndex(id)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return Report{}, err
			}
			delete(cached, id)
			delete(invalid, id)
			if err == nil {
				entries = append(entries, entry)
			}
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if entry.State != "ready" || entry.ActiveCommit == "" {
			continue
		}
		commit, err := i.packages.ActivatedPackageCommit(ctx, entry.PackageID)
		if err != nil {
			invalid[entry.PackageID] = core.Diagnostic{PackageID: entry.PackageID, Message: err.Error()}
			continue
		}
		identity, err := workspacepackages.ParsePackageID(entry.PackageID)
		if err != nil {
			invalid[entry.PackageID] = core.Diagnostic{PackageID: entry.PackageID, Message: err.Error()}
			continue
		}
		item, err := i.discoverPackage(filepath.Join(i.packages.PackagesRoot(), identity.Namespace, identity.Repository), entry.PackageID, commit)
		if err != nil {
			invalid[entry.PackageID] = core.Diagnostic{PackageID: entry.PackageID, Message: err.Error()}
			continue
		}
		cached[entry.PackageID] = item
	}
	fragments := make([]fragment, 0, len(cached))
	for _, item := range cached {
		fragments = append(fragments, item)
	}
	sort.Slice(fragments, func(a, b int) bool { return fragments[a].packageID < fragments[b].packageID })
	registrations, validPackages, diagnostics := i.withoutCollisions(fragments)
	for _, diagnostic := range invalid {
		diagnostics = append(diagnostics, diagnostic)
	}
	sort.Slice(diagnostics, func(a, b int) bool {
		if diagnostics[a].PackageID != diagnostics[b].PackageID {
			return diagnostics[a].PackageID < diagnostics[b].PackageID
		}
		return diagnostics[a].Message < diagnostics[b].Message
	})
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if err := i.registry.ReplacePackages(registrations, diagnostics); err != nil {
		return Report{}, err
	}
	i.fragments, i.diagnostics = cached, invalid
	catalog := i.registry.Catalog()
	return Report{Revision: catalog.Revision, Packages: validPackages, Commands: len(registrations), Diagnostics: diagnostics}, nil
}

// ValidateCandidates validates command/program artifacts and visible-name
// collisions before an activation may switch the active package source.
func (i *Indexer) ValidateCandidates(ctx context.Context, candidates []deployment.Candidate) error {
	replacements := map[string]deployment.Candidate{}
	for _, candidate := range candidates {
		replacements[candidate.PackageID] = candidate
	}
	entries, err := i.packages.ListPackageIndexes()
	if err != nil {
		return err
	}
	fragments := []fragment{}
	for _, entry := range entries {
		if _, replaced := replacements[entry.PackageID]; replaced || entry.State != "ready" || entry.ActiveCommit == "" {
			continue
		}
		commit, err := i.packages.ActivatedPackageCommit(ctx, entry.PackageID)
		if err != nil {
			return err
		}
		identity, _ := workspacepackages.ParsePackageID(entry.PackageID)
		item, err := i.discoverPackage(filepath.Join(i.packages.PackagesRoot(), identity.Namespace, identity.Repository), entry.PackageID, commit)
		if err != nil {
			return fmt.Errorf("package %s commands: %w", entry.PackageID, err)
		}
		fragments = append(fragments, item)
	}
	for _, candidate := range candidates {
		item, err := i.discoverPackage(candidate.Root, candidate.PackageID, candidate.Commit)
		if err != nil {
			return fmt.Errorf("package %s commands: %w", candidate.PackageID, err)
		}
		fragments = append(fragments, item)
	}
	_, _, diagnostics := i.withoutCollisions(fragments)
	if len(diagnostics) > 0 {
		return errors.New(diagnostics[0].Message)
	}
	return nil
}

func (i *Indexer) discoverPackage(root, packageID, commit string) (fragment, error) {
	if _, err := workspacepackages.ParsePackageID(packageID); err != nil {
		return fragment{}, err
	}
	files, err := workspacepackages.DeclarationFiles(root, "cbus/commands")
	if err != nil {
		return fragment{}, err
	}
	result := fragment{packageID: packageID}
	declared := map[string]string{}
	for _, path := range files {
		filename := filepath.Base(path)
		manifest, err := readManifest(path)
		if err != nil {
			return fragment{}, fmt.Errorf("%s: %w", filename, err)
		}
		if previous, exists := declared[manifest.Command]; exists {
			return fragment{}, fmt.Errorf("command %q is declared by both %s and %s", manifest.Command, previous, filename)
		}
		declared[manifest.Command] = filename
		if _, err := workspacepackages.ValidateProgram(root, packageID, manifest.Program, commit); err != nil {
			return fragment{}, fmt.Errorf("%s references invalid program %q: %w", filename, manifest.Program, err)
		}
		examples := make([]string, len(manifest.Examples))
		for index, example := range manifest.Examples {
			examples[index] = example.Command
		}
		programID := packageID + "/" + manifest.Program
		command := core.Command{
			Version: manifest.Version, ID: opaqueID(packageID, commit, manifest.Command), Name: manifest.Command,
			Kind: core.CommandKindPackage, Path: []string{manifest.Command}, Summary: manifest.Summary,
			Description: manifest.Description, Usage: manifest.Usage,
			Secrets:      append([]core.SecretInput(nil), manifest.Secrets...),
			MutatesState: manifest.MutatesState, RestartBehavior: manifest.RestartBehavior,
			Examples: examples, Origin: core.CommandOrigin{PackageID: packageID, Commit: commit},
		}
		result.registrations = append(result.registrations, core.Registration{
			Command: command,
			Handler: func(ctx context.Context, request core.Request) (core.Execution, error) {
				arguments := make([]any, len(request.Argv))
				for index := range request.Argv {
					arguments[index] = request.Argv[index]
				}
				result, err := i.programs.Run(ctx, programID, commit, arguments, request.Secrets)
				if err != nil {
					if errors.Is(err, programrunner.ErrActiveCommitChanged) {
						return core.Execution{}, core.NewError(core.CodeStaleCatalog, err.Error())
					}
					var execution *supervisor.ResponseError
					if errors.As(err, &execution) && execution.Code != "" {
						return core.Execution{}, &core.Error{Code: execution.Code, Message: execution.Message, Details: execution.Details}
					}
					return core.Execution{}, core.NewError(core.CodeRuntimeOperation, err.Error())
				}
				output := make([]core.OutputEvent, len(result.Output))
				for index, event := range result.Output {
					output[index] = core.OutputEvent{Level: event.Level, Message: event.Message, Fields: event.Fields}
				}
				return core.Execution{Result: result.Value, Output: output}, nil
			},
		})
	}
	sort.Slice(result.registrations, func(a, b int) bool {
		return result.registrations[a].Command.Name < result.registrations[b].Command.Name
	})
	return result, nil
}

func readManifest(path string) (commandManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return commandManifest{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return commandManifest{}, err
	}
	if info.Size() > commandManifestLimit {
		return commandManifest{}, errors.New("command declaration exceeds manifest size limit")
	}
	var manifest commandManifest
	decoder := toml.NewDecoder(io.LimitReader(file, commandManifestLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return commandManifest{}, err
	}
	if manifest.Version != 1 {
		return commandManifest{}, errors.New("version must equal 1")
	}
	if manifest.Command == "" {
		return commandManifest{}, errors.New("command is required")
	}
	for _, segment := range strings.Split(manifest.Command, ".") {
		if !commandSegmentPattern.MatchString(segment) {
			return commandManifest{}, errors.New("command must be a dot-separated name with lowercase kebab-case segments")
		}
	}
	if manifest.Command == "kernel" || strings.HasPrefix(manifest.Command, "kernel.") {
		return commandManifest{}, fmt.Errorf("command name %q uses reserved kernel namespace", manifest.Command)
	}
	if err := workspacepackages.ValidateName(manifest.Program); err != nil || strings.Contains(manifest.Program, ".") {
		return commandManifest{}, errors.New("program must name one same-package program")
	}
	if strings.TrimSpace(manifest.Summary) == "" {
		return commandManifest{}, errors.New("summary is required")
	}
	if manifest.RestartBehavior == "" {
		return commandManifest{}, errors.New("restart_behavior is required")
	}
	seenSecrets := map[string]bool{}
	seenOptions := map[string]bool{}
	for _, secret := range manifest.Secrets {
		if !commandSegmentPattern.MatchString(secret.Name) {
			return commandManifest{}, fmt.Errorf("secure input name %q must be lowercase kebab-case", secret.Name)
		}
		if seenSecrets[secret.Name] {
			return commandManifest{}, fmt.Errorf("duplicate secure input %q", secret.Name)
		}
		seenSecrets[secret.Name] = true
		if secret.Required && secret.Prompt == "" {
			return commandManifest{}, fmt.Errorf("required secure input %q needs a prompt", secret.Name)
		}
		if secret.StdinOption != "" {
			if !commandSegmentPattern.MatchString(secret.StdinOption) || seenOptions[secret.StdinOption] {
				return commandManifest{}, fmt.Errorf("invalid or duplicate secure stdin option %q", secret.StdinOption)
			}
			seenOptions[secret.StdinOption] = true
		}
	}
	for _, example := range manifest.Examples {
		if strings.TrimSpace(example.Command) == "" {
			return commandManifest{}, errors.New("example command is required")
		}
	}
	return manifest, nil
}

func opaqueID(packageID, commit, command string) string {
	return "package:" + packageID + "@" + commit + ":" + command
}

func (i *Indexer) withoutCollisions(fragments []fragment) ([]core.Registration, []string, []core.Diagnostic) {
	owners := map[string][]string{}
	coreNames := map[string]bool{}
	for _, command := range i.registry.Catalog().Commands {
		if command.Kind != core.CommandKindPackage {
			coreNames[command.Name] = true
		}
	}
	for _, item := range fragments {
		for _, registration := range item.registrations {
			owners[registration.Command.Name] = append(owners[registration.Command.Name], item.packageID)
		}
	}
	invalid := map[string][]string{}
	for name, packages := range owners {
		unique := uniqueStrings(packages)
		if coreNames[name] {
			for _, packageID := range unique {
				invalid[packageID] = append(invalid[packageID], fmt.Sprintf("command %q duplicates a kernel command", name))
			}
		}
		if len(unique) > 1 {
			for _, packageID := range unique {
				invalid[packageID] = append(invalid[packageID], fmt.Sprintf("command %q is also supplied by %s", name, strings.Join(withoutString(unique, packageID), ", ")))
			}
		}
	}
	registrations := []core.Registration{}
	packages := []string{}
	diagnostics := []core.Diagnostic{}
	for _, item := range fragments {
		if messages := invalid[item.packageID]; len(messages) > 0 {
			for _, message := range uniqueStrings(messages) {
				diagnostics = append(diagnostics, core.Diagnostic{PackageID: item.packageID, Message: message})
			}
			continue
		}
		registrations = append(registrations, item.registrations...)
		packages = append(packages, item.packageID)
	}
	sort.Strings(packages)
	return registrations, packages, diagnostics
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func withoutString(values []string, omitted string) []string {
	result := []string{}
	for _, value := range values {
		if value != omitted {
			result = append(result, value)
		}
	}
	return result
}
