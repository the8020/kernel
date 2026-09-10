package development

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"the8020/kernel/deployment"
	"the8020/kernel/identity"
	workspacepackages "the8020/kernel/packages"
)

type capturedPath struct {
	Path, ID      string
	BaseReference *fileReference `json:",omitempty"`
	FileReference *fileReference `json:",omitempty"`
}

type activationPackage struct {
	PackageID, Worktree, Private, Shared, Previous, Published string
	Origin, OriginHead, Parent                                string
	Staged                                                    string
	Removed                                                   bool
	RemovalChecked                                            bool
	Captures                                                  []capturedPath
	Directories                                               []capturedPath
}

func packageGit(ctx context.Context, d *RunscDriver, sandbox Sandbox, id string, input io.Reader, args ...string) (string, error) {
	// These are object/index operations; the package worktree may be deleted.
	options := []string{"--git-dir=/workspace/git/private/" + id + "/.git", "--work-tree=/workspace"}
	return nativeGit(ctx, d, sandbox, "/workspace", input, append(options, args...)...)
}

func (m *Manager) sharedHead(id string) (string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || !safePackageSegment(parts[0]) || !safePackageSegment(parts[1]) {
		return "", errors.New("package ID must be <namespace>/<repository>")
	}
	for _, name := range []string{parts[0], id} {
		info, err := os.Lstat(filepath.Join(m.config.PackagesRoot, name))
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", errors.New("package source must be an ordinary directory")
		}
	}
	// Native candidate mounts may create an empty destination directory. It
	// contains no published package. Never admit a nonempty uninitialized root.
	directory, err := os.Open(filepath.Join(m.config.PackagesRoot, id))
	if err != nil {
		return "", err
	}
	_, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	if errors.Is(readErr, io.EOF) && closeErr == nil {
		return "", nil
	}
	if readErr != nil || closeErr != nil {
		return "", errors.Join(readErr, closeErr)
	}
	repository, err := m.inspectRepository(id)
	if err != nil || !repository.Clean || !repository.ActivationReady {
		return "", fmt.Errorf("shared package is not clean and ready: %s: %v", id, err)
	}
	return repository.Head, nil
}

func ensurePackageGit(ctx context.Context, d *workspace, sandbox Sandbox, id, head string, validate func() error) error {
	if head != "" {
		return d.initializeGitOwned(ctx, id, validate)
	}
	upper, err := os.OpenRoot(filepath.Join(d.storage, "upper"))
	if err != nil {
		return err
	}
	defer upper.Close()
	var source string
	existingGit := false
	if file, err := upper.OpenFile(id+"/.git", os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0); err == nil {
		info, statErr := file.Stat()
		if statErr != nil {
			file.Close()
			return statErr
		}
		if info.Mode().IsRegular() {
			body, readErr := io.ReadAll(io.LimitReader(file, 4097))
			file.Close()
			if readErr != nil {
				return readErr
			}
			if !strings.HasPrefix(string(body), "gitdir: /workspace/git/private/") || !strings.HasSuffix(string(body), "/.git\n") {
				return fmt.Errorf("invalid private Git reference for %s", id)
			}
			source = strings.TrimSuffix(strings.TrimPrefix(string(body), "gitdir: /workspace/git/private/"), "/.git\n")
			if _, err := workspacepackages.ParsePackageID(source); err != nil {
				return fmt.Errorf("invalid private Git reference for %s", id)
			}
		} else {
			existingGit = info.IsDir()
			file.Close()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(d.storage, "git", id, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := d.ExecCommand(ctx, sandbox.SandboxID, []string{"/bin/mkdir", "-p", "/workspace/git/private/" + id}, nil, io.Discard); err != nil {
			return err
		}
		if source != "" && source != id || existingGit {
			// Clone metadata inside the sandbox; the existing host filesystem owner
			// links immutable objects because native cross-mount linkat is unavailable.
			repository := "/workspace/git/private/" + source
			if existingGit {
				repository = "/workspace/packages/" + id
			}
			if _, err := nativeGit(ctx, d.RunscDriver, sandbox, "/workspace", nil,
				"-c", "init.templateDir=", "clone", "--shared", "--no-checkout", "--",
				repository, "/workspace/git/private/"+id); err != nil {
				return err
			}
		} else {
			// Ordinary package creation requires no manual Git initialization.
			if _, err := nativeGit(ctx, d.RunscDriver, sandbox, "/workspace", nil,
				"-c", "init.templateDir=", "init", "--initial-branch=main",
				"--separate-git-dir=/workspace/git/private/"+id+"/.git", "/workspace/packages/"+id); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if source != "" && source != id || existingGit {
		if err := linkPrivateObjects(d, id, source, existingGit); err != nil {
			return err
		}
		repository := "/workspace/git/private/" + source
		if existingGit {
			repository = "/workspace/packages/" + id
		}
		// Repeat safely after interrupted preparation. Retain the source's existing
		// alternates, not its mutable location; its own objects are linked above.
		alternates := "/workspace/git/private/" + id + "/.git/objects/info/alternates"
		command := "set -eu; objects=$(git -C " + shellQuote(repository) + " count-objects -v); printf '%s\\n' \"$objects\" | sed -n 's/^alternate: //p' >" + shellQuote(alternates)
		if err := d.ExecStream(ctx, sandbox.SandboxID, command, nil, io.Discard); err != nil {
			return err
		}
		git := "git --git-dir=" + shellQuote("/workspace/git/private/"+id+"/.git")
		if err := d.ExecStream(ctx, sandbox.SandboxID, "if "+git+" rev-parse --verify --quiet HEAD >/dev/null; then "+git+" read-tree HEAD; else test \"$?\" -eq 1; fi", nil, io.Discard); err != nil {
			return err
		}
	}
	if _, err := packageGit(ctx, d.RunscDriver, sandbox, id, nil, "config", "remote.origin.url", "/workspace/git/shared/"+id); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(d.storage, "borrowed", id, "objects"), 0700); err != nil {
		return err
	}
	if err := addAlternates(ctx, d, sandbox, id, []string{id}); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	if source != "" && source != id {
		_, err := d.exchangeControl(ctx, map[string]any{"action": "bind-git", "path": id, "id": "bind", "moves": map[string]string{id: source}})
		return err
	}
	return gitReference(d.storage, id)
}

func linkPrivateObjects(d *workspace, destination, source string, inWorktree bool) error {
	owner := "git"
	if inWorktree {
		owner, source = "upper", destination
	}
	root, err := os.OpenRoot(filepath.Join(d.storage, owner))
	if err != nil {
		return err
	}
	defer root.Close()
	from, err := root.OpenRoot(source + "/.git/objects")
	if err != nil {
		return err
	}
	defer from.Close()
	private, err := os.OpenRoot(filepath.Join(d.storage, "git"))
	if err != nil {
		return err
	}
	defer private.Close()
	to, err := private.OpenRoot(destination + "/.git/objects")
	if err != nil {
		return err
	}
	defer to.Close()
	entries := 0
	return fs.WalkDir(from.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		// ponytail: the same 100,000-entry ceiling as shared object retention.
		if entries > 100000 {
			return errors.New("private Git retention exceeds 100,000 entries")
		}
		if name == "info" {
			return fs.SkipDir
		}
		if entry.IsDir() {
			return to.MkdirAll(name, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("private Git objects must be regular files")
		}
		a, err := from.Open(path.Dir(name))
		if err != nil {
			return err
		}
		defer a.Close()
		b, err := to.Open(path.Dir(name))
		if err != nil {
			return err
		}
		defer b.Close()
		err = unix.Linkat(int(a.Fd()), path.Base(name), int(b.Fd()), path.Base(name), 0)
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return err
	})
}

func addAlternates(ctx context.Context, d *workspace, sandbox Sandbox, id string, sources []string) error {
	if len(sources) == 0 {
		return nil
	}
	var entries strings.Builder
	for _, source := range sources {
		if _, err := workspacepackages.ParsePackageID(source); err != nil {
			return err
		}
		entries.WriteString("/workspace/git/borrowed/" + source + "/objects\n")
	}
	name := "/workspace/git/private/" + id + "/.git/objects/info/alternates"
	// Replace the file so even a native local clone's linked metadata is private.
	command := "set -eu; target=" + shellQuote(name) + "; temporary=$(mktemp \"$target.XXXXXX\"); trap 'rm -f -- \"$temporary\"' EXIT; { if [ -f \"$target\" ]; then cat -- \"$target\"; fi; cat; } | sort -u >\"$temporary\"; mv -- \"$temporary\" \"$target\""
	return d.ExecStream(ctx, sandbox.SandboxID, command, strings.NewReader(entries.String()), io.Discard)
}

// Skip clean packages before initializing Git or claiming publication ownership.
// Capture later validates and applies ignore rules to the selected changes.
func packageChanged(ctx context.Context, d *workspace, id string) (bool, error) {
	answer, err := d.exchange(ctx, "changes", id, "list")
	if err != nil {
		return false, err
	}
	paths, pathsOK := answer["paths"].([]any)
	directories, directoriesOK := answer["directories"].([]any)
	if !pathsOK || !directoriesOK {
		return false, errors.New("filesystem returned invalid package changes")
	}
	for _, value := range paths {
		name, ok := value.(string)
		if !ok {
			return false, errors.New("filesystem returned an invalid changed path")
		}
		if name != id+"/.git" && !strings.HasPrefix(name, id+"/.git/") {
			return true, nil
		}
	}
	return len(directories) != 0, nil
}

func activationEligible(ctx context.Context, d *workspace, id, head string) (bool, error) {
	if head != "" {
		return true, nil
	}
	answer, err := d.exchange(ctx, "changes", id, "list")
	if err != nil {
		return false, err
	}
	// Retained per-path originals still own conflicts after upstream removal,
	// including packages whose private Git HEAD has never been committed.
	if answer["manifest"] == true || answer["originals"] == true {
		return true, nil
	}
	if answer["exists"] == true {
		return false, fmt.Errorf("package %s needs a regular package.toml before activation", id)
	}
	// Retained originals still own conflicts with an upstream package deletion.
	git, err := os.Lstat(filepath.Join(d.storage, "git", id, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil && git.IsDir(), err
}

// Read package-level namespace changes once; source contents remain lazy.
type packageState struct {
	Moves      map[string]string
	Namespaces []string
}

func packageSelection(ctx context.Context, d *workspace, selection []string) ([]string, packageState, error) {
	var state packageState
	answer, err := d.exchange(ctx, "package-state", "packages", "list")
	if err != nil {
		return nil, state, err
	}
	body, err := json.Marshal(answer)
	if err != nil {
		return nil, state, err
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, state, err
	}
	ids := map[string]bool{}
	for _, root := range []string{d.shared, filepath.Join(d.storage, "upper"), filepath.Join(d.storage, "git"), filepath.Join(d.storage, "deleted")} {
		for _, id := range packageDirectories(root) {
			ids[id] = true
		}
	}
	for destination, source := range state.Moves {
		for _, id := range []string{destination, source} {
			if _, err := workspacepackages.ParsePackageID(id); err != nil {
				return nil, state, fmt.Errorf("cannot activate renamed package %q: %w", id, err)
			}
			ids[id] = true
		}
	}
	selected := map[string]bool{}
	for _, id := range selection {
		if _, err := workspacepackages.ParsePackageID(id); err != nil {
			return nil, state, err
		}
		selected[id] = true
	}
	for _, namespace := range state.Namespaces {
		if !safePackageSegment(namespace) {
			return nil, state, errors.New("invalid removed namespace")
		}
	}
	if len(selection) != 0 {
		// A move's source and destination publish together, including namespaces.
		for changed := true; changed; {
			before := len(selected)
			for destination, source := range state.Moves {
				if selected[destination] || selected[source] {
					selected[destination], selected[source] = true, true
				}
			}
			for _, namespace := range state.Namespaces {
				include := false
				for id := range selected {
					include = include || strings.HasPrefix(id, namespace+"/")
				}
				if include {
					for id := range ids {
						if strings.HasPrefix(id, namespace+"/") {
							selected[id] = true
						}
					}
				}
			}
			changed = len(selected) != before
		}
	}
	ordered := []string{}
	for id := range ids {
		if len(selection) == 0 || selected[id] {
			ordered = append(ordered, id)
		}
	}
	sort.Strings(ordered)
	return ordered, state, nil
}

type activationAttempt struct {
	ID, Phase, TransactionID string
	Packages                 []activationPackage
	Moves                    map[string]string
	Namespaces               []capturedPath
}

func nativeGit(ctx context.Context, d *RunscDriver, sandbox Sandbox, directory string, input io.Reader, args ...string) (string, error) {
	output := &boundedBuffer{limit: commandOutputLimit}
	err := d.ExecCommand(ctx, sandbox.SandboxID, append([]string{"/usr/bin/git", "-C", directory}, args...), input, output)
	if output.truncated {
		return "", errors.New("native Git response exceeded its output limit")
	}
	if err != nil {
		return output.String(), fmt.Errorf("native Git: %w: %s", err, output.String())
	}
	return output.String(), nil
}

func saveAttempt(filename string, attempt *activationAttempt) error {
	body, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	if err := writeAtomic(filename, body, 0600); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// The caller owns LockSources throughout this recovery and its completion.
func (m *Manager) recoverSwitch(ctx context.Context, journal string, attempt *activationAttempt, hook deployment.SchemaHook) error {
	var switched []activationPackage
	// Refuse external edits before changing any source. Mid-reset dirty trees
	// still need a durable per-package publication intent before adoption.
	for _, item := range attempt.Packages {
		head, err := m.sharedHead(item.PackageID)
		if err != nil || (head != item.Previous && head != item.Published) {
			return fmt.Errorf("cannot recover changed shared package %s: %v", item.PackageID, err)
		}
		if head == item.Published {
			switched = append(switched, item)
		}
	}
	if len(switched) == len(attempt.Packages) {
		attempt.Phase = "published"
		return saveAttempt(journal, attempt)
	}
	for _, item := range switched {
		if item.Previous == item.Published {
			continue
		}
		root := filepath.Join(m.config.PackagesRoot, item.PackageID)
		switch {
		case item.Previous == "":
			if err := renameSource(root, item.Staged); err != nil {
				return err
			}
		case item.Published == "":
			backup, err := gitOutput(root+".previous", "rev-parse", "HEAD")
			if err != nil || backup != item.Previous {
				return fmt.Errorf("invalid removal backup for %s: %v", item.PackageID, err)
			}
			if err := renameSource(root+".previous", root); err != nil {
				return err
			}
		default:
			if _, err := gitCommand(ctx, root, nil, "reset", "--hard", item.Previous); err != nil {
				return err
			}
		}
	}
	if err := hook.Complete(ctx, attempt.TransactionID, false); err != nil {
		return err
	}
	attempt.Phase = "captured"
	return saveAttempt(journal, attempt)
}

func renameSource(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	// POSIX directory rename also replaces an empty mountpoint atomically;
	// a nonempty destination cannot be overwritten.
	if err := unix.Renameat(unix.AT_FDCWD, source, unix.AT_FDCWD, target); err != nil {
		return err
	}
	for _, name := range []string{filepath.Dir(source), filepath.Dir(target)} {
		directory, err := os.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) capturePackage(ctx context.Context, d *workspace, sandbox Sandbox, id, attemptID, head string, captureFiles bool) (activationPackage, error) {
	item := activationPackage{PackageID: id, Worktree: "/workspace/packages/.conflicts/" + attemptID + "/" + id}
	answer, err := d.exchange(ctx, "changes", id, "list")
	if err != nil {
		return item, err
	}
	var references struct {
		Files, Bases map[string]*fileReference
	}
	encoded, err := json.Marshal(answer["references"])
	if err != nil {
		return item, err
	}
	if err := json.Unmarshal(encoded, &references); err != nil {
		return item, err
	}
	directories, ok := answer["directories"].([]any)
	if !ok {
		return item, errors.New("filesystem returned invalid directory changes")
	}
	var directoryNames []string
	for _, value := range directories {
		name, ok := value.(string)
		if !ok || !validRelative(name) || path.Clean(name) != name || (name != id && !strings.HasPrefix(name, id+"/")) {
			return item, errors.New("filesystem returned an invalid directory path")
		}
		if name == id {
			item.Removed = answer["exists"] == false
			directoryNames = append(directoryNames, "")
			continue
		}
		directoryNames = append(directoryNames, strings.TrimPrefix(name, id+"/"))
	}
	raw, ok := answer["paths"].([]any)
	if !ok {
		return item, errors.New("filesystem returned invalid changed paths")
	}
	var names []string
	for _, value := range raw {
		name, ok := value.(string)
		if !ok || !strings.HasPrefix(name, id+"/") || !validRelative(name) || path.Clean(name) != name {
			return item, errors.New("filesystem returned an invalid package path")
		}
		name = strings.TrimPrefix(name, id+"/")
		if name == ".git" || strings.HasPrefix(name, ".git/") {
			continue // Native repository metadata is never activated as package source.
		}
		if strings.ContainsAny(name, "\r\n") {
			return item, fmt.Errorf("unsupported activation path %q", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 && len(directoryNames) == 0 {
		return item, nil
	}
	if head != "" {
		if err := d.retainGitObjects(ctx, id); err != nil {
			return item, err
		}
	}
	// Native check-ignore respects the private index: tracked paths retain
	// their ordinary semantics even when an ignore rule also matches them.
	ignored := &boundedBuffer{limit: commandOutputLimit}
	command := "git --git-dir=" + shellQuote("/workspace/git/private/"+id+"/.git") + " --work-tree=" + shellQuote("/workspace/packages/"+id) + " check-ignore -z --stdin; status=$?; test \"$status\" -le 1"
	queries := append([]string(nil), names...)
	for _, name := range directoryNames {
		queries = append(queries, name+"/")
	}
	if !item.Removed {
		if err := d.ExecStream(ctx, sandbox.SandboxID, command, strings.NewReader(strings.Join(queries, "\x00")+"\x00"), ignored); err != nil || ignored.truncated {
			return item, fmt.Errorf("inspect ignored paths: %v", err)
		}
	}
	excluded := map[string]bool{}
	for _, name := range strings.Split(ignored.String(), "\x00") {
		excluded[name] = true
	}
	// Untouched source follows shared updates even while the developer keeps an
	// older private index. Files newly tracked upstream must not become ignored.
	var ignoredFiles []string
	for _, name := range names {
		if excluded[name] {
			ignoredFiles = append(ignoredFiles, name)
		}
	}
	if len(ignoredFiles) != 0 && head != "" {
		args := append([]string{"--literal-pathspecs", "ls-tree", "--name-only", "-z", head, "--"}, ignoredFiles...)
		tracked, err := packageGit(ctx, d.RunscDriver, sandbox, id, nil, args...)
		if err != nil {
			return item, err
		}
		for _, name := range strings.Split(tracked, "\x00") {
			delete(excluded, name)
		}
	}
	for _, name := range names {
		if excluded[name] {
			continue
		}
		if !captureFiles {
			item.Captures = append(item.Captures, capturedPath{Path: name, BaseReference: references.Bases[id+"/"+name], FileReference: references.Files[id+"/"+name]})
			continue
		}
		captureID, err := randomHex(12)
		if err != nil {
			return item, err
		}
		capture := capturedPath{Path: name, ID: captureID}
		if _, err := d.exchange(ctx, "capture", id+"/"+name, capture.ID); err != nil {
			return item, err
		}
		item.Captures = append(item.Captures, capture)
	}
	for _, name := range directoryNames {
		publish := !excluded[name+"/"]
		if !publish {
			// ponytail: bounded by the 4,096 changed-path limit; index ancestors
			// only if bulk ignored-directory removals make this scan expensive.
			for _, capture := range item.Captures {
				if strings.HasPrefix(capture.Path, name+"/") {
					publish = true
					break
				}
			}
		}
		if !publish {
			continue
		}
		if !captureFiles {
			item.Directories = append(item.Directories, capturedPath{Path: name})
			continue
		}
		captureID, err := randomHex(12)
		if err != nil {
			return item, err
		}
		if _, err := d.exchange(ctx, "capture-directory", path.Join(id, name), captureID); err != nil {
			return item, err
		}
		item.Directories = append(item.Directories, capturedPath{Path: name, ID: captureID})
	}
	// A child is acknowledged before a removed parent makes it visible again.
	sort.Slice(item.Directories, func(i, j int) bool { return len(item.Directories[i].Path) > len(item.Directories[j].Path) })
	return item, nil
}

func (m *Manager) Preview(ctx context.Context, user string, options ActivationOptions) (ActivationPreview, error) {
	if err := validatePreviewFile(options); err != nil {
		return ActivationPreview{}, err
	}
	unlock := m.lockUser(user)
	defer unlock()
	result := ActivationPreview{Packages: []ActivationPackagePreview{}}
	sandbox, err := m.loadSandbox(user)
	if err != nil {
		return result, err
	}
	d, ok := m.workspaceFor(sandbox.SandboxID)
	if !ok {
		return result, errors.New("development sandbox is not running")
	}
	ordered, _, err := packageSelection(ctx, d, nil)
	if err != nil {
		return result, err
	}
	for _, id := range ordered {
		if len(options.SelectedPackages) != 0 && !slices.Contains(options.SelectedPackages, id) {
			continue
		}
		err := func() error {
			if changed, err := packageChanged(ctx, d, id); err != nil || !changed {
				return err
			}
			validate, release, err := workspacepackages.ObserveSources(ctx, d.shared, []string{id})
			if err != nil {
				return err
			}
			defer release()
			head, err := m.sharedHead(id)
			if err != nil {
				return err
			}
			if eligible, err := activationEligible(ctx, d, id, head); err != nil || !eligible {
				return err
			}
			if err := ensurePackageGit(ctx, d, sandbox, id, head, validate); err != nil {
				return err
			}
			item, err := m.capturePackage(ctx, d, sandbox, id, "", head, false)
			if err != nil {
				return err
			}
			if err := validate(); err != nil {
				return err
			}
			if len(item.Captures) == 0 && len(item.Directories) == 0 {
				return nil
			}
			preview := ActivationPackagePreview{PackageID: id, Change: "modified", Selected: true, SharedCommit: head, ActivationReady: true, Files: []ActivationFile{}, ChangedFiles: len(item.Captures)}
			if head == "" {
				preview.Change = "added"
			}
			if item.Removed {
				preview.Change = "deleted"
			}
			if preview.ChangedFiles == 0 {
				preview.ChangedFiles = len(item.Directories)
			}
			// ponytail: line counts cover changed regular files up to 1 MiB;
			// larger files still appear in the changed-file count.
			for _, capture := range item.Captures {
				change := "modified"
				paths := []string{}
				countLines := true
				for _, side := range []string{"base", "upper"} {
					ref := capture.BaseReference
					if side == "upper" {
						ref = capture.FileReference
					}
					filename := filepath.Join(d.storage, side, id, capture.Path)
					info, err := os.Lstat(filename)
					if errors.Is(err, os.ErrNotExist) && ref != nil {
						countLines = false
						continue
					}
					if errors.Is(err, os.ErrNotExist) {
						paths = append(paths, os.DevNull)
						if side == "base" {
							change = "added"
						} else {
							change = "deleted"
						}
						continue
					}
					if err != nil {
						return err
					}
					if !info.Mode().IsRegular() || info.Size() > 1<<20 {
						countLines = false
					}
					paths = append(paths, filename)
				}
				if countLines {
					stats, err := gitCommand(ctx, d.storage, nil, "diff", "--no-index", "--numstat", "--", paths[0], paths[1])
					var exit *exec.ExitError
					if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
						return err
					}
					values := strings.Fields(stats)
					if len(values) >= 2 {
						added, _ := strconv.Atoi(values[0])
						removed, _ := strconv.Atoi(values[1])
						preview.AddedRows += added
						preview.RemovedRows += removed
					}
				}
				file := ActivationFile{Path: capture.Path, Change: change}
				if options.PreviewFile == file.Path {
					file.Diff, err = previewFileDiff(ctx, d, id, capture)
					if err != nil {
						return err
					}
				}
				preview.Files = append(preview.Files, file)
			}
			result.Packages = append(result.Packages, preview)
			return nil
		}()
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func previewFileDiff(ctx context.Context, d *workspace, id string, capture capturedPath) (*ActivationFileDiff, error) {
	root, err := os.OpenRoot(d.storage)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	temporary, err := os.MkdirTemp(d.storage, ".preview-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	paths := []string{}
	for index, side := range []string{"base", "upper"} {
		ref := capture.BaseReference
		if index == 1 {
			ref = capture.FileReference
		}
		name := filepath.Join(side, id, capture.Path)
		file, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		var body []byte
		if errors.Is(err, os.ErrNotExist) && ref != nil {
			if _, err := workspacepackages.ParsePackageID(ref.Package); err != nil {
				return nil, err
			}
			if _, err := objectID(ref.Blob); err != nil {
				return nil, err
			}
			if ref.Mode&unix.S_IFMT != unix.S_IFREG {
				return &ActivationFileDiff{Notice: "This file type has no text preview. Review it in the terminal."}, nil
			}
			gitDir := "--git-dir=" + filepath.Join(d.storage, "borrowed", ref.Package)
			sizeText, err := gitCommand(ctx, d.storage, nil, gitDir, "cat-file", "-s", ref.Blob)
			if err != nil {
				return nil, err
			}
			size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
			if err != nil || size < 0 {
				return nil, errors.New("invalid referenced blob size")
			}
			if size > 48<<10 {
				return &ActivationFileDiff{Notice: "File exceeds the 48 KiB text preview limit. Review it in the terminal."}, nil
			}
			output := &boundedBuffer{limit: 48 << 10}
			command := exec.CommandContext(ctx, "git", gitDir, "cat-file", "blob", ref.Blob)
			command.Stdout = output
			if err := command.Run(); err != nil {
				return nil, err
			}
			if output.truncated {
				return &ActivationFileDiff{Notice: "File exceeds the 48 KiB text preview limit. Review it in the terminal."}, nil
			}
			body = []byte(output.RawString())
		} else if errors.Is(err, os.ErrNotExist) {
			paths = append(paths, os.DevNull)
			continue
		} else if errors.Is(err, unix.ELOOP) {
			return &ActivationFileDiff{Notice: "This file type has no text preview. Review it in the terminal."}, nil
		} else if err != nil {
			return nil, err
		} else {
			info, statErr := file.Stat()
			if statErr != nil {
				file.Close()
				return nil, statErr
			}
			if !info.Mode().IsRegular() || info.Size() > 48<<10 {
				file.Close()
				return &ActivationFileDiff{Notice: "Large or non-text file. Review it in the terminal (text preview limit: 48 KiB)."}, nil
			}
			body, err = io.ReadAll(io.LimitReader(file, (48<<10)+1))
			file.Close()
			if err != nil {
				return nil, err
			}
			if len(body) > 48<<10 {
				return &ActivationFileDiff{Notice: "File exceeds the 48 KiB text preview limit. Review it in the terminal."}, nil
			}
		}
		if bytes.ContainsRune(body, 0) || !utf8.Valid(body) {
			return &ActivationFileDiff{Notice: "Binary file changed. A text diff is unavailable."}, nil
		}
		filename := filepath.Join(temporary, side)
		if err := os.WriteFile(filename, body, 0600); err != nil {
			return nil, err
		}
		paths = append(paths, filename)
	}
	output, err := gitCommand(ctx, temporary, nil, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--no-color", "--", paths[0], paths[1])
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return nil, err
	}
	return activationDiffOutput(output), nil
}

func prepareGit(ctx context.Context, d *workspace, sandbox Sandbox, item *activationPackage, attemptID, shared, author, email, message string) error {
	git := func(input io.Reader, args ...string) (string, error) {
		return packageGit(ctx, d.RunscDriver, sandbox, item.PackageID, input, args...)
	}
	index := "/tmp/activation-" + attemptID + "-" + strings.ReplaceAll(item.PackageID, "/", "-") + ".index"
	indexGit := func(input io.Reader, args ...string) (string, error) {
		output := &boundedBuffer{limit: commandOutputLimit}
		argv := []string{"/usr/bin/env", "GIT_INDEX_FILE=" + index, "/usr/bin/git", "--git-dir=/workspace/git/private/" + item.PackageID + "/.git", "--work-tree=/workspace"}
		err := d.ExecCommand(ctx, sandbox.SandboxID, append(argv, args...), input, output)
		if err != nil || output.truncated {
			return "", fmt.Errorf("prepare captured Git index: %v: %s", err, output.String())
		}
		return strings.TrimSpace(output.String()), nil
	}
	baseSource := shared
	if baseSource == "" {
		baseSource = "--empty"
		output := &boundedBuffer{limit: 128}
		command := "git --git-dir=" + shellQuote("/workspace/git/private/"+item.PackageID+"/.git") + " rev-parse --verify --quiet HEAD; status=$?; test \"$status\" -le 1"
		if err := d.ExecStream(ctx, sandbox.SandboxID, command, nil, output); err != nil || output.truncated {
			return fmt.Errorf("inspect new package history: %v", err)
		}
		if output.String() != "" {
			var err error
			item.Parent, err = objectID(output.String())
			if err != nil {
				return err
			}
		}
	}
	if _, err := indexGit(nil, "read-tree", baseSource); err != nil {
		return err
	}
	var originals, private strings.Builder
	referencePackages := map[string]bool{}
	hashLength := len(shared)
	if hashLength == 0 {
		hashLength = 40
	}
	for _, capture := range item.Captures {
		for _, side := range []string{"base", "file"} {
			filename := filepath.Join(d.storage, "snapshots", capture.ID, side)
			if data, err := os.ReadFile(filename + "-reference"); err == nil {
				var ref fileReference
				if err := json.Unmarshal(data, &ref); err != nil {
					return err
				}
				if _, err := objectID(ref.Blob); err != nil {
					return err
				}
				referencePackages[ref.Package] = true
				mode := "100644"
				if ref.Mode&unix.S_IFMT == unix.S_IFLNK {
					mode = "120000"
				} else if ref.Mode&0111 != 0 {
					mode = "100755"
				}
				entry := mode + " " + ref.Blob + "\t" + capture.Path + "\x00"
				if side == "base" {
					originals.WriteString(entry)
				} else {
					private.WriteString(entry)
				}
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			info, err := os.Lstat(filename)
			entry := "0 " + strings.Repeat("0", hashLength) + "\t" + capture.Path + "\x00"
			if err == nil {
				mode := "100644"
				var payload io.ReadCloser
				if info.Mode()&os.ModeSymlink != 0 {
					target, err := os.Readlink(filename)
					if err != nil {
						return err
					}
					payload, mode = io.NopCloser(strings.NewReader(target)), "120000"
				} else if info.Mode().IsRegular() {
					payload, err = os.Open(filename)
					if err != nil {
						return err
					}
					if info.Mode()&0111 != 0 {
						mode = "100755"
					}
				} else {
					return fmt.Errorf("invalid captured file %s", capture.Path)
				}
				blob, hashErr := git(payload, "hash-object", "-w", "--stdin")
				closeErr := payload.Close()
				if hashErr != nil || closeErr != nil {
					return errors.Join(hashErr, closeErr)
				}
				blob, err = objectID(blob)
				if err != nil {
					return err
				}
				entry = mode + " " + blob + "\t" + capture.Path + "\x00"
			} else {
				if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if side == "file" {
					if _, err := os.Stat(filepath.Join(d.storage, "snapshots", capture.ID, "deleted")); err != nil {
						return err
					}
				}
			}
			if side == "base" {
				originals.WriteString(entry)
			} else {
				private.WriteString(entry)
			}
		}
	}
	sources := []string{}
	for id := range referencePackages {
		sources = append(sources, id)
	}
	sort.Strings(sources)
	if err := addAlternates(ctx, d, sandbox, item.PackageID, sources); err != nil {
		return err
	}
	commitTree := func(tree, body string, parents ...string) (string, error) {
		args := []string{"-c", "user.name=" + author, "-c", "user.email=" + email, "commit-tree", tree}
		for _, parent := range parents {
			if parent != "" {
				args = append(args, "-p", parent)
			}
		}
		value, err := git(strings.NewReader(body), args...)
		if err != nil {
			return "", err
		}
		return objectID(value)
	}
	if _, err := indexGit(strings.NewReader(originals.String()), "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	baseTree, err := indexGit(nil, "write-tree")
	if err != nil {
		return err
	}
	base, err := commitTree(baseTree, "Observed per-path originals "+attemptID+"\n", item.Parent)
	if err != nil {
		return err
	}
	if item.Removed {
		if _, err := indexGit(nil, "read-tree", "--empty"); err != nil {
			return err
		}
	}
	if _, err := indexGit(strings.NewReader(private.String()), "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	privateTree, err := indexGit(nil, "write-tree")
	if err != nil {
		return err
	}
	item.Private, err = commitTree(privateTree, message, base)
	if err != nil {
		return err
	}
	sharedTree, err := git(nil, "mktree")
	parents := []string{base}
	if shared != "" {
		sharedTree, err = git(nil, "rev-parse", shared+"^{tree}")
		parents = append(parents, shared)
	}
	if err != nil {
		return err
	}
	// Both comparison commits descend from the actual per-path originals.
	// Attaching the real shared commit also permits ordinary subsequent merges.
	item.Shared, err = commitTree(strings.TrimSpace(sharedTree), "Shared comparison "+attemptID+"\n", parents...)
	if err != nil {
		return err
	}
	if _, err := git(nil, "worktree", "add", "--detach", "--no-checkout", item.Worktree, item.Private); err != nil {
		return err
	}
	var patterns strings.Builder
	escape := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[")
	for _, capture := range item.Captures {
		// Unedited moves need only index entries. Git materializes conflicts
		// itself, including conflicts outside these sparse checkout patterns.
		if _, err := os.Stat(filepath.Join(d.storage, "snapshots", capture.ID, "file-reference")); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		patterns.WriteString("/" + escape.Replace(capture.Path) + "\n")
	}
	if _, err := nativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, strings.NewReader(patterns.String()), "sparse-checkout", "set", "--no-cone", "--stdin"); err != nil {
		return err
	}
	_, err = nativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, nil, "read-tree", "--reset", "-u", "HEAD")
	return err
}

func transferCandidate(ctx context.Context, d *workspace, sandbox Sandbox, item *activationPackage, directory, target, commit string) error {
	ref := "refs/the8020/activation-export"
	if _, err := packageGit(ctx, d.RunscDriver, sandbox, item.PackageID, nil, "update-ref", ref, commit); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, "candidate-*.bundle")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	args := []string{"/usr/bin/git", "--git-dir=/workspace/git/private/" + item.PackageID + "/.git", "bundle", "create", "-", ref}
	if item.Previous != "" {
		args = append(args, "^"+item.Previous)
	} else if item.OriginHead != "" {
		args = append(args, "^"+item.OriginHead)
	}
	err = d.ExecCommand(ctx, sandbox.SandboxID, args, nil, file)
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	if _, err := gitCommand(ctx, target, nil, "-c", "fetch.fsckObjects=true", "fetch", file.Name(), ref); err != nil {
		return err
	}
	head, err := gitOutput(target, "rev-parse", "FETCH_HEAD")
	if err != nil || head != commit {
		return errors.New("native candidate transfer changed the commit")
	}
	return nil
}

// Git tracks files, not package roots. An upstream addition made after the
// package was removed needs an explicit added-by-them index entry; silently
// including it in the deletion's base would discard an unobserved file.
func removalAdditions(ctx context.Context, d *workspace, sandbox Sandbox, item *activationPackage) error {
	if !item.Removed || item.RemovalChecked || item.Previous == "" {
		return nil
	}
	entries, err := packageGit(ctx, d.RunscDriver, sandbox, item.PackageID, nil, "ls-tree", "-rz", item.Previous)
	if err != nil {
		return err
	}
	observed := map[string]bool{}
	for _, capture := range item.Captures {
		observed[capture.Path] = true
	}
	var index strings.Builder
	var paths []string
	for _, entry := range strings.Split(strings.TrimSuffix(entries, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "\t", 2)
		if len(parts) != 2 {
			return errors.New("invalid removal tree entry")
		}
		if observed[parts[1]] {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) != 3 || fields[1] != "blob" {
			return errors.New("unsupported removal tree entry")
		}
		index.WriteString("0 " + strings.Repeat("0", len(fields[2])) + "\t" + parts[1] + "\x00" + fields[0] + " " + fields[2] + " 3\t" + parts[1] + "\x00")
		paths = append(paths, parts[1])
	}
	if index.Len() != 0 {
		escape := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[")
		var patterns strings.Builder
		for _, name := range paths {
			patterns.WriteString("/" + escape.Replace(name) + "\n")
		}
		if _, err := nativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, strings.NewReader(patterns.String()), "sparse-checkout", "add", "--stdin"); err != nil {
			return err
		}
		if _, err := nativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, strings.NewReader(index.String()), "update-index", "-z", "--index-info"); err != nil {
			return err
		}
		if _, err := nativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, nil, append([]string{"checkout", "--ignore-skip-worktree-bits", "--theirs", "--"}, paths...)...); err != nil {
			return err
		}
	}
	item.RemovalChecked = true
	return nil
}

// This native validation view shares unchanged inodes. It does not copy their
// data, and validators receive it read-only. Lower hardlink mutation and
// concurrent shared-directory changes must be qualified before adoption.
func validationView(ctx context.Context, shared, gitRoot, previous, candidate, target string) error {
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	if err := filepath.WalkDir(shared, func(source string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(shared, source)
		if err != nil || relative == ".git" {
			return filepath.SkipDir
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(source)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported candidate entry %s", relative)
		}
		return os.Link(source, destination)
	}); err != nil {
		return err
	}
	changed, err := gitCommand(ctx, gitRoot, nil, "diff", "--name-only", "-z", "--no-renames", previous, candidate)
	if err != nil {
		return err
	}
	oldEntries, err := gitCommand(ctx, gitRoot, nil, "ls-tree", "-rz", previous)
	if err != nil {
		return err
	}
	unchanged := map[string]string{}
	for _, entry := range strings.Split(oldEntries, "\x00") {
		header, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if ok && len(fields) == 3 && fields[1] == "blob" {
			unchanged[fields[0]+" "+fields[2]] = name
		}
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
		if name == "" {
			continue
		}
		if !validRelative(name) || path.Clean(name) != name || name == ".git" || strings.HasPrefix(name, ".git/") {
			return errors.New("invalid changed candidate path")
		}
		if err := root.RemoveAll(name); err != nil {
			return err
		}
		entry, err := gitCommand(ctx, gitRoot, nil, "ls-tree", "-z", candidate, "--", name)
		if err != nil {
			return err
		}
		if entry == "" {
			continue
		}
		fields := strings.Fields(strings.SplitN(entry, "\t", 2)[0])
		if len(fields) != 3 || fields[1] != "blob" {
			return errors.New("unsupported candidate object type")
		}
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		if previousName, ok := unchanged[fields[0]+" "+fields[2]]; ok && fields[0] != "120000" {
			if err := os.Link(filepath.Join(shared, previousName), filepath.Join(target, name)); err != nil {
				return err
			}
			continue
		}
		command := exec.CommandContext(ctx, "git", "-C", gitRoot, "cat-file", "blob", fields[2])
		diagnostics := &boundedBuffer{limit: commandOutputLimit}
		command.Stderr = diagnostics
		if fields[0] == "120000" {
			blob := &boundedBuffer{limit: 4096}
			command.Stdout = blob
			if err := command.Run(); err != nil || blob.truncated {
				return fmt.Errorf("read candidate symlink: %v: %s", err, diagnostics.String())
			}
			if err := root.Symlink(blob.String(), name); err != nil {
				return err
			}
		} else {
			mode := os.FileMode(0644)
			if fields[0] == "100755" {
				mode = 0755
			} else if fields[0] != "100644" {
				return errors.New("unsupported candidate file mode")
			}
			file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			command.Stdout = file
			if err := errors.Join(command.Run(), file.Sync(), file.Close()); err != nil {
				return fmt.Errorf("write candidate blob: %w: %s", err, diagnostics.String())
			}
		}
	}
	return nil
}

func (m *Manager) Activate(ctx context.Context, userID string, options ActivationOptions) (result ActivationResult, returnErr error) {
	unlock := m.lockUser(userID)
	defer unlock()
	sandbox, err := m.loadSandbox(userID)
	if err != nil {
		return result, err
	}
	result = ActivationResult{Status: "failed", Packages: []ActivationPackageResult{}}
	defer func() {
		if returnErr != nil {
			result.Error = returnErr.Error()
		}
		sandbox.ActivationActive = false
		sandbox.State = StateReady
		if result.Status == "conflicted" {
			sandbox.State = StateConflicted
		}
		sandbox.LastActivationAt, sandbox.UpdatedAt = time.Now().UTC(), time.Now().UTC()
		sandbox.LastActivationResult, sandbox.LastActivationStatus = &result, result.Status
		returnErr = errors.Join(returnErr, m.saveSandbox(sandbox))
		if returnErr != nil {
			result.Error = returnErr.Error()
		}
	}()
	if strings.TrimSpace(options.Description) == "" {
		return result, errors.New("activation description is required")
	}
	d, ok := m.workspaceFor(sandbox.SandboxID)
	if !ok {
		return result, errors.New("development workspace is not running")
	}
	if _, active := m.owned.Load(sandbox.SandboxID); !active {
		return result, errors.New("development sandbox is not running")
	}
	sandbox.State, sandbox.ActivationActive = StateActivating, true
	if err := m.saveSandbox(sandbox); err != nil {
		return result, err
	}
	author, email := m.activationAuthor(sandbox, options)
	directory := filepath.Join(m.sandboxRoot(sandbox), "activation")
	journal := filepath.Join(directory, "active.json")
	var attempt activationAttempt
	err = readJSON(journal, &attempt)
	if errors.Is(err, os.ErrNotExist) {
		attempt.ID, err = randomHex(12)
		if err != nil {
			return result, err
		}
		attempt.Phase = "captured"
		ordered, state, err := packageSelection(ctx, d, options.SelectedPackages)
		if err != nil {
			return result, err
		}
		attempt.Moves = map[string]string{}
		included := map[string]bool{}
		for _, id := range ordered {
			included[id] = true
		}
		for destination, source := range state.Moves {
			if included[destination] && included[source] {
				attempt.Moves[destination] = source
			}
		}
		for _, namespace := range state.Namespaces {
			selected := false
			for _, id := range ordered {
				selected = selected || strings.HasPrefix(id, namespace+"/")
			}
			if !selected {
				continue
			}
			captureID, err := randomHex(12)
			if err != nil {
				return result, err
			}
			if _, err := d.exchange(ctx, "capture-directory", namespace, captureID); err != nil {
				return result, err
			}
			attempt.Namespaces = append(attempt.Namespaces, capturedPath{Path: namespace, ID: captureID})
		}
		for _, id := range ordered {
			item, err := func() (activationPackage, error) {
				if changed, err := packageChanged(ctx, d, id); err != nil || !changed {
					return activationPackage{}, err
				}
				release, err := workspacepackages.LockSources(ctx, m.config.PackagesRoot, []string{id})
				if err != nil {
					return activationPackage{}, err
				}
				defer release()
				head, err := m.sharedHead(id)
				if err != nil {
					return activationPackage{}, err
				}
				if eligible, err := activationEligible(ctx, d, id, head); err != nil || !eligible {
					return activationPackage{}, err
				}
				if err := ensurePackageGit(ctx, d, sandbox, id, head, nil); err != nil {
					return activationPackage{}, err
				}
				item, err := m.capturePackage(ctx, d, sandbox, id, attempt.ID, head, true)
				if err != nil || len(item.Captures) == 0 && len(item.Directories) == 0 {
					return item, err
				}
				item.Previous = head
				if head == "" {
					item.Origin = state.Moves[id]
				}
				message := activationCommitMessage(options.Description, sandbox.SandboxID, options.Metadata)
				err = prepareGit(ctx, d, sandbox, &item, attempt.ID, head, author, email, message)
				return item, err
			}()
			if err != nil {
				return result, err
			}
			if len(item.Captures) != 0 || len(item.Directories) != 0 {
				attempt.Packages = append(attempt.Packages, item)
			}
		}
		if len(attempt.Packages) == 0 {
			result.Success, result.Status = true, "not-committed"
			return result, nil
		}
		if err := saveAttempt(journal, &attempt); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	if len(options.SelectedPackages) > 0 {
		ordered, _, err := packageSelection(ctx, d, options.SelectedPackages)
		if err != nil {
			return result, err
		}
		selected := map[string]bool{}
		for _, id := range ordered {
			selected[id] = true
		}
		for destination, source := range attempt.Moves {
			if selected[destination] || selected[source] {
				selected[destination], selected[source] = true, true
			}
		}
		for _, item := range attempt.Packages {
			if !selected[item.PackageID] {
				return result, errors.New("resolve the pending activation before changing its package selection")
			}
		}
	}
	packageIDs := make([]string, 0, len(attempt.Packages))
	for _, item := range attempt.Packages {
		packageIDs = append(packageIDs, item.PackageID)
	}
	releaseSources, err := workspacepackages.LockSources(ctx, m.config.PackagesRoot, packageIDs)
	if err != nil {
		return result, err
	}
	defer releaseSources()
	hook := m.schemaDeployment()
	if hook == nil {
		return result, errors.New("schema coordinator is required")
	}
	if attempt.Phase == "preparing" {
		if err := hook.Complete(ctx, attempt.TransactionID, false); err != nil {
			return result, err
		}
		attempt.Phase, attempt.TransactionID = "captured", ""
		if err := saveAttempt(journal, &attempt); err != nil {
			return result, err
		}
	}
	if attempt.Phase == "switching" {
		if err := m.recoverSwitch(ctx, journal, &attempt, hook); err != nil {
			return result, err
		}
	}
	if attempt.Phase != "captured" && attempt.Phase != "published" {
		return result, fmt.Errorf("unsupported activation recovery phase %q", attempt.Phase)
	}
	if attempt.Phase == "captured" {
		var candidates []deployment.Candidate
		stage, err := os.MkdirTemp(directory, "validation-")
		if err != nil {
			return result, err
		}
		defer os.RemoveAll(stage)
		for index := range attempt.Packages {
			item := &attempt.Packages[index]
			head, err := m.sharedHead(item.PackageID)
			if err != nil {
				return result, err
			}
			if head != "" {
				if err := d.retainGitObjects(ctx, item.PackageID); err != nil {
					return result, err
				}
			}
			git := func(args ...string) (string, error) {
				return nativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, nil, args...)
			}
			packageResult := ActivationPackageResult{PackageID: item.PackageID, Status: "failed", CommitMessage: options.Description}
			for _, comparison := range []string{item.Shared, item.Private} {
				if comparison == item.Shared {
					_, err = git("-c", "user.name="+author, "-c", "user.email="+email, "-c", "merge.conflictStyle=diff3", "merge", "--no-edit", comparison)
				} else if err == nil {
					_, err = git("merge-base", "--is-ancestor", comparison, "HEAD")
				}
				if err != nil {
					break
				}
			}
			if err == nil {
				_, err = git("diff", "--quiet", "HEAD")
			}
			item.Previous = head
			if err == nil && head != "" {
				_, err = git("-c", "user.name="+author, "-c", "user.email="+email, "-c", "merge.conflictStyle=diff3", "merge", "--allow-unrelated-histories", "--no-edit", head)
			}
			if removalErr := removalAdditions(ctx, d, sandbox, item); removalErr != nil {
				return result, removalErr
			}
			if saveErr := saveAttempt(journal, &attempt); saveErr != nil {
				return result, saveErr
			}
			if err == nil {
				_, err = git("diff", "--quiet", "HEAD")
			}
			if err != nil {
				paths, listErr := git("diff", "--name-only", "--diff-filter=U", "-z")
				if listErr != nil {
					return result, errors.Join(err, listErr)
				}
				if paths != "" {
					packageResult.Conflicts = strings.Split(strings.TrimSuffix(paths, "\x00"), "\x00")
				}
				packageResult.Status, result.Status = "conflicted", "conflicted"
				packageResult.ConflictWorktree = item.Worktree
				packageResult.Error = "Resolve and commit with ordinary Git in " + item.Worktree + ", then rerun activate. " + err.Error()
				result.Packages = append(result.Packages, packageResult)
				return result, err
			}
			resolved, err := git("rev-parse", "HEAD")
			if err != nil {
				return result, err
			}
			resolved, err = objectID(resolved)
			if err != nil {
				return result, err
			}
			shared := filepath.Join(d.shared, item.PackageID)
			target := shared
			if item.Previous == "" {
				item.Staged = filepath.Join(directory, "staged", attempt.ID, item.PackageID)
				if err := os.RemoveAll(item.Staged); err != nil {
					return result, err
				}
				if err := os.MkdirAll(item.Staged, 0700); err != nil {
					return result, err
				}
				if item.Origin != "" {
					item.OriginHead, err = m.sharedHead(item.Origin)
					if err != nil {
						return result, err
					}
				}
				if item.OriginHead != "" {
					if _, err := gitCommand(ctx, directory, nil, "-c", "init.templateDir=", "clone", "--local", "--no-checkout", "--", filepath.Join(d.shared, item.Origin), item.Staged); err != nil {
						return result, err
					}
					if _, err := gitCommand(ctx, item.Staged, nil, "remote", "remove", "origin"); err != nil {
						return result, err
					}
				} else if _, err := gitCommand(ctx, item.Staged, nil, "-c", "init.templateDir=", "init", "--initial-branch=main"); err != nil {
					return result, err
				}
				target = item.Staged
			}
			if err := transferCandidate(ctx, d, sandbox, item, directory, target, resolved); err != nil {
				return result, err
			}
			treeEntries, err := git("ls-tree", "HEAD")
			if err != nil {
				return result, err
			}
			if treeEntries == "" && (item.Removed || item.Previous == "") {
				item.Published = ""
				candidates = append(candidates, deployment.Candidate{PackageID: item.PackageID, Root: shared})
				continue
			}
			message := options.Description
			if override := strings.TrimSpace(options.PackageMessages[item.PackageID]); override != "" {
				message = override
			}
			messageFile := filepath.Join(stage, "message")
			if err := os.WriteFile(messageFile, []byte(activationCommitMessage(message, sandbox.SandboxID, options.Metadata)), 0600); err != nil {
				return result, err
			}
			commitArgs := []string{"commit-tree", resolved + "^{tree}", "-F", messageFile}
			if item.Previous != "" {
				commitArgs = append(commitArgs, "-p", item.Previous)
			} else {
				if item.OriginHead != "" {
					commitArgs = append(commitArgs, "-p", item.OriginHead)
				}
				if item.Parent != "" && item.Parent != item.OriginHead {
					commitArgs = append(commitArgs, "-p", item.Parent)
				}
			}
			item.Published, err = gitOutputContext(ctx, target, gitIdentity(author, email), commitArgs...)
			if err != nil {
				return result, err
			}
			view := filepath.Join(stage, item.PackageID)
			if item.Previous == "" {
				reset := "--hard"
				if item.OriginHead != "" {
					if err := validationView(ctx, filepath.Join(d.shared, item.Origin), target, item.OriginHead, item.Published, target); err != nil {
						return result, err
					}
					reset = "--mixed"
				}
				if _, err := gitCommand(ctx, target, nil, "reset", reset, item.Published); err != nil {
					return result, err
				}
				view = target
			} else {
				if err := validationView(ctx, shared, shared, item.Previous, item.Published, view); err != nil {
					return result, err
				}
			}
			candidates = append(candidates, deployment.Candidate{PackageID: item.PackageID, Root: view, Commit: item.Published})
		}
		attempt.TransactionID, err = identity.New("act")
		if err != nil {
			return result, err
		}
		attempt.Phase = "preparing"
		if err := saveAttempt(journal, &attempt); err != nil {
			return result, err
		}
		if err := hook.Prepare(ctx, attempt.TransactionID, candidates); err != nil {
			return result, fmt.Errorf("prepare schema and hooks: %w", err)
		}
		prepared := true
		defer func() {
			if prepared {
				returnErr = errors.Join(returnErr, hook.Complete(context.WithoutCancel(ctx), attempt.TransactionID, false))
			}
		}()
		sort.Slice(attempt.Packages, func(i, j int) bool { return attempt.Packages[i].PackageID < attempt.Packages[j].PackageID })
		for _, item := range attempt.Packages {
			head, err := m.sharedHead(item.PackageID)
			if err != nil || head != item.Previous {
				return result, fmt.Errorf("shared package changed during validation: %s (previous %q, current %q): %v", item.PackageID, item.Previous, head, err)
			}
		}
		attempt.Phase = "switching"
		if err := saveAttempt(journal, &attempt); err != nil {
			return result, err
		}
		for _, item := range attempt.Packages {
			root := filepath.Join(d.shared, item.PackageID)
			var err error
			switch {
			case item.Published == "":
				if item.Previous != "" {
					err = renameSource(root, root+".previous")
				}
			case item.Previous == "":
				err = renameSource(item.Staged, root)
			default:
				_, err = gitCommand(ctx, root, nil, "reset", "--hard", item.Published)
			}
			if err != nil {
				prepared = false // Partial switches require explicit recovery, not false rollback claims.
				return result, err
			}
		}
		prepared = false
		attempt.Phase = "published"
		if err := saveAttempt(journal, &attempt); err != nil {
			return result, err
		}
	}
	if err := hook.Complete(ctx, attempt.TransactionID, true); err != nil {
		result.Status = "committed-finalization-failed"
		return result, err
	}
	for _, item := range attempt.Packages {
		for _, capture := range item.Captures {
			if _, err := d.exchange(ctx, "acknowledge", item.PackageID+"/"+capture.Path, capture.ID); err != nil {
				return result, err
			}
		}
		for _, capture := range item.Directories {
			if _, err := d.exchange(ctx, "acknowledge-directory", path.Join(item.PackageID, capture.Path), capture.ID); err != nil {
				return result, err
			}
		}
		result.Packages = append(result.Packages, ActivationPackageResult{PackageID: item.PackageID, Status: "committed", PreviousHead: item.Previous, ResultingHead: item.Published, CommitMessage: options.Description})
	}
	for _, item := range attempt.Packages {
		for _, captures := range [][]capturedPath{item.Captures, item.Directories} {
			for _, capture := range captures {
				if _, err := d.exchange(ctx, "release", path.Join(item.PackageID, capture.Path), capture.ID); err != nil {
					return result, fmt.Errorf("release published capture: %w", err)
				}
			}
		}
	}
	for _, capture := range attempt.Namespaces {
		if _, err := d.exchange(ctx, "acknowledge-directory", capture.Path, capture.ID); err != nil {
			return result, err
		}
		if _, err := d.exchange(ctx, "release", capture.Path, capture.ID); err != nil {
			return result, err
		}
	}
	if _, err := d.exchangeControl(ctx, map[string]any{"action": "acknowledge-moves", "path": "packages", "id": "acknowledge", "moves": attempt.Moves}); err != nil {
		return result, err
	}
	if err := os.Remove(journal); err != nil {
		return result, err
	}
	sandbox.ConflictedPackages = nil
	result.Success, result.Status = true, "committed"
	return result, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (m *Manager) activationAuthor(sandbox Sandbox, options ActivationOptions) (string, string) {
	name, email := strings.TrimSpace(options.AuthorName), strings.TrimSpace(options.AuthorEmail)
	if name == "" {
		name = sandbox.UserID
	}
	if email == "" {
		email = sandbox.UserID + "@development.local"
	}
	return name, email
}

func activationCommitMessage(message, sandboxID string, metadata map[string]string) string {
	values := map[string]string{"sandbox": sandboxID}
	for key, value := range metadata {
		values["metadata_"+sanitizeFooterKey(key)] = strings.ReplaceAll(value, "\n", " ")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var appendix strings.Builder
	appendix.WriteString("# 80|20 activation metadata\n[the8020.activation]\n")
	for _, key := range keys {
		appendix.WriteString(strconv.Quote(key))
		appendix.WriteString(" = ")
		appendix.WriteString(strconv.Quote(values[key]))
		appendix.WriteByte('\n')
	}
	return strings.TrimSpace(message) + "\n\n" + appendix.String()
}

func sanitizeFooterKey(value string) string {
	parts := strings.FieldsFunc(value, func(character rune) bool {
		return (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9')
	})
	if len(parts) == 0 {
		return "metadata"
	}
	return strings.ToLower(strings.Join(parts, "_"))
}
