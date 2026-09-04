// Package discovery indexes command descriptors from active packages.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"the8020/kernel/cbus/core"
	"the8020/kernel/deployment"
	programrunner "the8020/kernel/execution/programs"
	workspacepackages "the8020/kernel/packages"
)

const commandManifestLimit = 1 << 20

var commandSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type PackageSource interface {
	ListPackageIndexes() ([]workspacepackages.PackageIndex, error)
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
	packages PackageSource
	programs ProgramRunner
	registry Registry
}

type Report struct {
	Revision    string            `json:"revision"`
	Packages    []string          `json:"packages"`
	Commands    int               `json:"commands"`
	Diagnostics []core.Diagnostic `json:"diagnostics,omitempty"`
}

type commandManifest struct {
	Version         int                `toml:"version"`
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

// Reindex independently validates every ready package, then publishes one
// complete immutable registry snapshot.
func (i *Indexer) Reindex(ctx context.Context) (Report, error) {
	entries, err := i.packages.ListPackageIndexes()
	if err != nil {
		return Report{}, fmt.Errorf("list active packages: %w", err)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].PackageID < entries[b].PackageID })
	fragments := make([]fragment, 0, len(entries))
	diagnostics := []core.Diagnostic{}
	for _, entry := range entries {
		if entry.State != "ready" || entry.ActiveCommit == "" {
			continue
		}
		commit, err := i.packages.ActivatedPackageCommit(ctx, entry.PackageID)
		if err != nil {
			diagnostics = append(diagnostics, core.Diagnostic{PackageID: entry.PackageID, Message: err.Error()})
			continue
		}
		identity, err := workspacepackages.ParsePackageID(entry.PackageID)
		if err != nil {
			diagnostics = append(diagnostics, core.Diagnostic{PackageID: entry.PackageID, Message: err.Error()})
			continue
		}
		root := filepath.Join(i.packages.PackagesRoot(), identity.Namespace, identity.Repository)
		item, err := i.discoverPackage(root, entry.PackageID, commit)
		if err != nil {
			diagnostics = append(diagnostics, core.Diagnostic{PackageID: entry.PackageID, Message: err.Error()})
			continue
		}
		fragments = append(fragments, item)
	}
	registrations, validPackages, duplicateDiagnostics := i.withoutCollisions(fragments)
	diagnostics = append(diagnostics, duplicateDiagnostics...)
	sort.Slice(diagnostics, func(a, b int) bool {
		if diagnostics[a].PackageID != diagnostics[b].PackageID {
			return diagnostics[a].PackageID < diagnostics[b].PackageID
		}
		return diagnostics[a].Message < diagnostics[b].Message
	})
	if err := i.registry.ReplacePackages(registrations, diagnostics); err != nil {
		return Report{}, err
	}
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
	identity, err := workspacepackages.ParsePackageID(packageID)
	if err != nil {
		return fragment{}, err
	}
	commandRoot := filepath.Join(root, "cbus", "commands")
	info, err := os.Lstat(commandRoot)
	if errors.Is(err, os.ErrNotExist) {
		return fragment{packageID: packageID}, nil
	}
	if err != nil {
		return fragment{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fragment{}, errors.New("cbus/commands must be a real directory")
	}
	result := fragment{packageID: packageID}
	err = filepath.WalkDir(commandRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("command path %s is a symlink", path)
		}
		if entry.IsDir() || entry.Name() != "command.toml" {
			return nil
		}
		relative, err := filepath.Rel(commandRoot, filepath.Dir(path))
		if err != nil || relative == "." {
			return errors.New("command.toml must be inside a command path directory")
		}
		segments := strings.Split(filepath.ToSlash(relative), "/")
		for _, segment := range segments {
			if !commandSegmentPattern.MatchString(segment) {
				return fmt.Errorf("command path segment %q must be lowercase kebab-case", segment)
			}
		}
		manifest, err := readManifest(path)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.ToSlash(relative), err)
		}
		if _, err := workspacepackages.ValidateProgram(root, packageID, manifest.Program, commit); err != nil {
			return fmt.Errorf("%s references invalid program %q: %w", filepath.ToSlash(relative), manifest.Program, err)
		}
		nameParts := append([]string(nil), segments...)
		if identity.Namespace == "the8020" {
			nameParts = append([]string{identity.Repository}, nameParts...)
		} else {
			nameParts = append([]string{identity.Namespace, identity.Repository}, nameParts...)
		}
		name := strings.Join(nameParts, ".")
		if name == "kernel" || strings.HasPrefix(name, "kernel.") {
			return fmt.Errorf("command name %q uses reserved kernel namespace", name)
		}
		examples := make([]string, len(manifest.Examples))
		for index, example := range manifest.Examples {
			examples[index] = example.Command
		}
		programID := packageID + "/" + manifest.Program
		command := core.Command{
			Version: manifest.Version, ID: opaqueID(packageID, commit, relative), Name: name,
			Kind: core.CommandKindPackage, Path: []string{name}, Summary: manifest.Summary,
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
					var execution *programrunner.ExecutionError
					if errors.As(err, &execution) {
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
		return nil
	})
	if err != nil {
		return fragment{}, err
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
	var manifest commandManifest
	decoder := toml.NewDecoder(io.LimitReader(file, commandManifestLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return commandManifest{}, err
	}
	if manifest.Version != 1 {
		return commandManifest{}, errors.New("version must equal 1")
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

func opaqueID(packageID, commit, relative string) string {
	return "package:" + packageID + "@" + commit + ":" + filepath.ToSlash(relative)
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
