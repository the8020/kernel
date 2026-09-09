//go:build workflowanalysis

// run.py activation overlays this owner into Manager.Activate. It is a
// disposable integration candidate, not an installed publication backend.
package development

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	platformconsole "the8020/kernel/console"
	"the8020/kernel/deployment"
	"the8020/kernel/identity"
	workspacepackages "the8020/kernel/packages"
	"the8020/kernel/sandbox/backend"
)

type analysisCapturedPath struct {
	Path, ID string
}

type analysisActivationPackage struct {
	PackageID, Worktree, Private, Shared, Previous, Published string
	Staged                                                    string
	Removed                                                   bool
	RemovalChecked                                            bool
	Captures                                                  []analysisCapturedPath
	Directories                                               []analysisCapturedPath
}

func analysisPackageGit(ctx context.Context, d *RunscDriver, sandbox Sandbox, id string, input io.Reader, args ...string) (string, error) {
	options := []string{"--git-dir=/workspace/git/private/" + id + "/.git", "--work-tree=/workspace/packages/" + id}
	return analysisNativeGit(ctx, d, sandbox, "/workspace", input, append(options, args...)...)
}

func (m *Manager) analysisSharedHead(id string) (string, error) {
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

func analysisEnsurePackageGit(ctx context.Context, d *analysisSparseDriver, sandbox Sandbox, id, head string, validate func() error) error {
	if head != "" {
		return d.initializeGitOwned(ctx, id, validate)
	}
	if _, err := os.Lstat(filepath.Join(d.storage, "git", id, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := d.ExecCommand(ctx, sandbox.SandboxID, []string{"/bin/mkdir", "-p", "/workspace/git/private/" + id}, nil, io.Discard); err != nil {
			return err
		}
		if _, err := analysisPackageGit(ctx, d.RunscDriver, sandbox, id, nil, "-c", "init.templateDir=", "init", "--initial-branch=main"); err != nil {
			return err
		}
		if _, err := analysisPackageGit(ctx, d.RunscDriver, sandbox, id, nil, "config", "remote.origin.url", "/workspace/git/shared/"+id); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(d.storage, "borrowed", id, "objects"), 0700); err != nil {
			return err
		}
		if err := d.ExecCommand(ctx, sandbox.SandboxID, []string{"/usr/bin/tee", "/workspace/git/private/" + id + "/.git/objects/info/alternates"}, strings.NewReader("/workspace/git/borrowed/"+id+"/objects\n"), io.Discard); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	return analysisGitReference(d.storage, id)
}

// Skip clean packages before initializing Git or claiming publication ownership.
// Capture later validates and applies ignore rules to the selected changes.
func analysisPackageChanged(ctx context.Context, d *analysisSparseDriver, id string) (bool, error) {
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

func analysisActivationEligible(d *analysisSparseDriver, id, head string) (bool, error) {
	if head != "" {
		return true, nil
	}
	manifest, err := os.Lstat(filepath.Join(d.storage, "upper", id, "package.toml"))
	if errors.Is(err, os.ErrNotExist) {
		// An established private repository can conflict with upstream package
		// deletion even when its untouched manifest has disappeared upstream.
		git, err := os.Lstat(filepath.Join(d.storage, "git", id, ".git"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil && git.IsDir(), err
	}
	if err != nil || !manifest.Mode().IsRegular() {
		return false, fmt.Errorf("invalid new package manifest %s: %v", id, err)
	}
	return true, nil
}

type analysisActivationAttempt struct {
	ID, Phase, TransactionID string
	Packages                 []analysisActivationPackage
}

func analysisNativeGit(ctx context.Context, d *RunscDriver, sandbox Sandbox, directory string, input io.Reader, args ...string) (string, error) {
	output := &boundedBuffer{limit: commandOutputLimit}
	err := d.ExecCommand(ctx, sandbox.SandboxID, append([]string{"/usr/bin/git", "-C", directory}, args...), input, output)
	if output.truncated {
		return "", errors.New("native Git response exceeded the probe limit")
	}
	if err != nil {
		return output.String(), fmt.Errorf("native Git: %w: %s", err, output.String())
	}
	return output.String(), nil
}

func analysisSaveAttempt(filename string, attempt *analysisActivationAttempt) error {
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
func (m *Manager) analysisRecoverSwitch(ctx context.Context, journal string, attempt *analysisActivationAttempt, hook deployment.SchemaHook) error {
	var switched []analysisActivationPackage
	// Refuse external edits before changing any source. Mid-reset dirty trees
	// still need a durable per-package publication intent before adoption.
	for _, item := range attempt.Packages {
		head, err := m.analysisSharedHead(item.PackageID)
		if err != nil || (head != item.Previous && head != item.Published) {
			return fmt.Errorf("cannot recover changed shared package %s: %v", item.PackageID, err)
		}
		if head == item.Published {
			switched = append(switched, item)
		}
	}
	if len(switched) == len(attempt.Packages) {
		attempt.Phase = "published"
		return analysisSaveAttempt(journal, attempt)
	}
	for _, item := range switched {
		if item.Previous == item.Published {
			continue
		}
		root := filepath.Join(m.config.PackagesRoot, item.PackageID)
		switch {
		case item.Previous == "":
			if err := analysisRenameSource(root, item.Staged); err != nil {
				return err
			}
		case item.Published == "":
			backup, err := gitOutput(root+".previous", "rev-parse", "HEAD")
			if err != nil || backup != item.Previous {
				return fmt.Errorf("invalid removal backup for %s: %v", item.PackageID, err)
			}
			if err := analysisRenameSource(root+".previous", root); err != nil {
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
	return analysisSaveAttempt(journal, attempt)
}

func analysisRenameSource(source, target string) error {
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

func (m *Manager) analysisCapturePackage(ctx context.Context, d *analysisSparseDriver, sandbox Sandbox, id, attemptID, head string, captureFiles bool) (analysisActivationPackage, error) {
	item := analysisActivationPackage{PackageID: id, Worktree: "/workspace/packages/.conflicts/" + attemptID + "/" + id}
	answer, err := d.exchange(ctx, "changes", id, "list")
	if err != nil {
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
			if _, err := os.Lstat(filepath.Join(d.storage, "upper", id)); errors.Is(err, os.ErrNotExist) {
				item.Removed = true
			} else if err != nil {
				return item, err
			}
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
		tracked, err := analysisPackageGit(ctx, d.RunscDriver, sandbox, id, nil, args...)
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
			item.Captures = append(item.Captures, analysisCapturedPath{Path: name})
			continue
		}
		captureID, err := randomHex(12)
		if err != nil {
			return item, err
		}
		capture := analysisCapturedPath{Path: name, ID: captureID}
		if _, err := d.exchange(ctx, "capture", id+"/"+name, capture.ID); err != nil {
			return item, err
		}
		item.Captures = append(item.Captures, capture)
	}
	for _, name := range directoryNames {
		publish := !excluded[name+"/"]
		if !publish {
			// ponytail: bounded by the 4,096-path probe limit; index ancestors
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
			item.Directories = append(item.Directories, analysisCapturedPath{Path: name})
			continue
		}
		captureID, err := randomHex(12)
		if err != nil {
			return item, err
		}
		if _, err := d.exchange(ctx, "capture-directory", path.Join(id, name), captureID); err != nil {
			return item, err
		}
		item.Directories = append(item.Directories, analysisCapturedPath{Path: name, ID: captureID})
	}
	// A child is acknowledged before a removed parent makes it visible again.
	sort.Slice(item.Directories, func(i, j int) bool { return len(item.Directories[i].Path) > len(item.Directories[j].Path) })
	return item, nil
}

func analysisPrepareGit(ctx context.Context, d *analysisSparseDriver, sandbox Sandbox, item *analysisActivationPackage, attemptID, shared, author, email, message string) error {
	repository := "/workspace/packages/" + item.PackageID
	git := func(input io.Reader, args ...string) (string, error) {
		return analysisPackageGit(ctx, d.RunscDriver, sandbox, item.PackageID, input, args...)
	}
	index := "/tmp/activation-" + attemptID + "-" + strings.ReplaceAll(item.PackageID, "/", "-") + ".index"
	indexGit := func(input io.Reader, args ...string) (string, error) {
		output := &boundedBuffer{limit: commandOutputLimit}
		argv := []string{"/usr/bin/env", "GIT_INDEX_FILE=" + index, "/usr/bin/git", "--git-dir=/workspace/git/private/" + item.PackageID + "/.git", "--work-tree=" + repository}
		err := d.ExecCommand(ctx, sandbox.SandboxID, append(argv, args...), input, output)
		if err != nil || output.truncated {
			return "", fmt.Errorf("prepare captured Git index: %v: %s", err, output.String())
		}
		return strings.TrimSpace(output.String()), nil
	}
	baseSource := shared
	if baseSource == "" {
		baseSource = "--empty"
	}
	if _, err := indexGit(nil, "read-tree", baseSource); err != nil {
		return err
	}
	var originals, private strings.Builder
	hashLength := len(shared)
	if hashLength == 0 {
		hashLength = 40
	}
	for _, capture := range item.Captures {
		for _, side := range []string{"base", "file"} {
			filename := filepath.Join(d.storage, "snapshots", capture.ID, side)
			if data, err := os.ReadFile(filename + "-reference"); err == nil {
				var ref analysisFileReference
				if err := json.Unmarshal(data, &ref); err != nil {
					return err
				}
				if _, err := analysisObjectID(ref.Blob); err != nil {
					return err
				}
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
				blob, err = analysisObjectID(blob)
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
	commitTree := func(tree, body string, parents ...string) (string, error) {
		args := []string{"-c", "user.name=" + author, "-c", "user.email=" + email, "commit-tree", tree}
		for _, parent := range parents {
			args = append(args, "-p", parent)
		}
		value, err := git(strings.NewReader(body), args...)
		if err != nil {
			return "", err
		}
		return analysisObjectID(value)
	}
	if _, err := indexGit(strings.NewReader(originals.String()), "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	baseTree, err := indexGit(nil, "write-tree")
	if err != nil {
		return err
	}
	base, err := commitTree(baseTree, "Observed per-path originals "+attemptID+"\n")
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
	if _, err := analysisNativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, strings.NewReader(patterns.String()), "sparse-checkout", "set", "--no-cone", "--stdin"); err != nil {
		return err
	}
	_, err = analysisNativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, nil, "read-tree", "--reset", "-u", "HEAD")
	return err
}

func analysisTransferCandidate(ctx context.Context, d *analysisSparseDriver, sandbox Sandbox, item *analysisActivationPackage, directory, target, commit string) error {
	ref := "refs/the8020/activation-export"
	if _, err := analysisPackageGit(ctx, d.RunscDriver, sandbox, item.PackageID, nil, "update-ref", ref, commit); err != nil {
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
func analysisRemovalAdditions(ctx context.Context, d *analysisSparseDriver, sandbox Sandbox, item *analysisActivationPackage) error {
	if !item.Removed || item.RemovalChecked || item.Previous == "" {
		return nil
	}
	entries, err := analysisPackageGit(ctx, d.RunscDriver, sandbox, item.PackageID, nil, "ls-tree", "-rz", item.Previous)
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
		if _, err := analysisNativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, strings.NewReader(patterns.String()), "sparse-checkout", "add", "--stdin"); err != nil {
			return err
		}
		if _, err := analysisNativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, strings.NewReader(index.String()), "update-index", "-z", "--index-info"); err != nil {
			return err
		}
		if _, err := analysisNativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, nil, append([]string{"checkout", "--ignore-skip-worktree-bits", "--theirs", "--"}, paths...)...); err != nil {
			return err
		}
	}
	item.RemovalChecked = true
	return nil
}

// This native validation view shares unchanged inodes. It does not copy their
// data, and validators receive it read-only. Lower hardlink mutation and
// concurrent shared-directory changes must be qualified before adoption.
func analysisValidationView(ctx context.Context, shared, previous, candidate, target string) error {
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
	changed, err := gitCommand(ctx, shared, nil, "diff", "--name-only", "-z", "--no-renames", previous, candidate)
	if err != nil {
		return err
	}
	oldEntries, err := gitCommand(ctx, shared, nil, "ls-tree", "-rz", previous)
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
		entry, err := gitCommand(ctx, shared, nil, "ls-tree", "-z", candidate, "--", name)
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
		command := exec.CommandContext(ctx, "git", "-C", shared, "cat-file", "blob", fields[2])
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
		sandbox.ActivationActive, sandbox.WritesPaused = false, false
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
	d, ok := m.driver.(*analysisSparseDriver)
	if !ok {
		return result, errors.New("activation prototype requires its qualified sparse driver")
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
	var attempt analysisActivationAttempt
	err = readJSON(journal, &attempt)
	if errors.Is(err, os.ErrNotExist) {
		attempt.ID, err = randomHex(12)
		if err != nil {
			return result, err
		}
		attempt.Phase = "captured"
		selected := map[string]bool{}
		for _, id := range options.SelectedPackages {
			if _, err := m.analysisSharedHead(id); err != nil {
				return result, err
			}
			selected[id] = true
		}
		ids := map[string]bool{}
		for _, root := range []string{m.config.PackagesRoot, filepath.Join(d.storage, "upper"), filepath.Join(d.storage, "git")} {
			for _, id := range packageDirectories(root) {
				ids[id] = true
			}
		}
		ordered := make([]string, 0, len(ids))
		for id := range ids {
			ordered = append(ordered, id)
		}
		sort.Strings(ordered)
		for _, id := range ordered {
			if len(selected) > 0 && !selected[id] {
				continue
			}
			item, err := func() (analysisActivationPackage, error) {
				if changed, err := analysisPackageChanged(ctx, d, id); err != nil || !changed {
					return analysisActivationPackage{}, err
				}
				release, err := workspacepackages.LockSources(ctx, m.config.PackagesRoot, []string{id})
				if err != nil {
					return analysisActivationPackage{}, err
				}
				defer release()
				head, err := m.analysisSharedHead(id)
				if err != nil {
					return analysisActivationPackage{}, err
				}
				if eligible, err := analysisActivationEligible(d, id, head); err != nil || !eligible {
					return analysisActivationPackage{}, err
				}
				if err := analysisEnsurePackageGit(ctx, d, sandbox, id, head, nil); err != nil {
					return analysisActivationPackage{}, err
				}
				item, err := m.analysisCapturePackage(ctx, d, sandbox, id, attempt.ID, head, true)
				if err != nil || len(item.Captures) == 0 && len(item.Directories) == 0 {
					return item, err
				}
				item.Previous = head
				message := activationCommitMessage(options.Description, sandbox.SandboxID, options.Metadata)
				err = analysisPrepareGit(ctx, d, sandbox, &item, attempt.ID, head, author, email, message)
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
		if err := analysisSaveAttempt(journal, &attempt); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	if len(options.SelectedPackages) > 0 {
		selected := map[string]bool{}
		for _, id := range options.SelectedPackages {
			selected[id] = true
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
		if err := analysisSaveAttempt(journal, &attempt); err != nil {
			return result, err
		}
	}
	if attempt.Phase == "switching" {
		if err := m.analysisRecoverSwitch(ctx, journal, &attempt, hook); err != nil {
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
			head, err := m.analysisSharedHead(item.PackageID)
			if err != nil {
				return result, err
			}
			if head != "" {
				if err := d.retainGitObjects(ctx, item.PackageID); err != nil {
					return result, err
				}
			}
			git := func(args ...string) (string, error) {
				return analysisNativeGit(ctx, d.RunscDriver, sandbox, item.Worktree, nil, args...)
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
			if removalErr := analysisRemovalAdditions(ctx, d, sandbox, item); removalErr != nil {
				return result, removalErr
			}
			if saveErr := analysisSaveAttempt(journal, &attempt); saveErr != nil {
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
			resolved, err = analysisObjectID(resolved)
			if err != nil {
				return result, err
			}
			shared := filepath.Join(d.shared, item.PackageID)
			target := shared
			if item.Previous == "" {
				item.Staged = filepath.Join(directory, "staged", attempt.ID, item.PackageID)
				if err := os.MkdirAll(item.Staged, 0700); err != nil {
					return result, err
				}
				if _, err := gitCommand(ctx, item.Staged, nil, "-c", "init.templateDir=", "init", "--initial-branch=main"); err != nil {
					return result, err
				}
				target = item.Staged
			}
			if err := analysisTransferCandidate(ctx, d, sandbox, item, directory, target, resolved); err != nil {
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
			}
			item.Published, err = gitOutputContext(ctx, target, gitIdentity(author, email), commitArgs...)
			if err != nil {
				return result, err
			}
			view := filepath.Join(stage, item.PackageID)
			if item.Previous == "" {
				if _, err := gitCommand(ctx, target, nil, "reset", "--hard", item.Published); err != nil {
					return result, err
				}
				view = target
			} else {
				if err := analysisValidationView(ctx, shared, item.Previous, item.Published, view); err != nil {
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
		if err := analysisSaveAttempt(journal, &attempt); err != nil {
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
			head, err := m.analysisSharedHead(item.PackageID)
			if err != nil || head != item.Previous {
				return result, fmt.Errorf("shared package changed during validation: %s (previous %q, current %q): %v", item.PackageID, item.Previous, head, err)
			}
		}
		attempt.Phase = "switching"
		if err := analysisSaveAttempt(journal, &attempt); err != nil {
			return result, err
		}
		for _, item := range attempt.Packages {
			root := filepath.Join(d.shared, item.PackageID)
			var err error
			switch {
			case item.Published == "":
				if item.Previous != "" {
					err = analysisRenameSource(root, root+".previous")
				}
			case item.Previous == "":
				err = analysisRenameSource(item.Staged, root)
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
		if err := analysisSaveAttempt(journal, &attempt); err != nil {
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
		for _, captures := range [][]analysisCapturedPath{item.Captures, item.Directories} {
			for _, capture := range captures {
				if _, err := d.exchange(ctx, "release", path.Join(item.PackageID, capture.Path), capture.ID); err != nil {
					return result, fmt.Errorf("release published capture: %w", err)
				}
			}
		}
	}
	if err := os.Remove(journal); err != nil {
		return result, err
	}
	sandbox.ConflictedPackages = nil
	result.Success, result.Status = true, "committed"
	return result, nil
}

type analysisActivationHook struct {
	prepare  func(context.Context, string, []deployment.Candidate) error
	complete func(context.Context, string, bool) error
}

func (h analysisActivationHook) Prepare(ctx context.Context, id string, candidates []deployment.Candidate) error {
	return h.prepare(ctx, id, candidates)
}
func (h analysisActivationHook) Complete(ctx context.Context, id string, activated bool) error {
	return h.complete(ctx, id, activated)
}

func TestWorkflowAnalysisActivation(t *testing.T) {
	m, sparse, sandbox, shared := analysisSparseRuntime(t, "activation", 4)
	d, ctx := sparse.RunscDriver, context.Background()
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	record := map[string]any{"full_workflow_qualified": false, "actual_schema_engine_qualified": false,
		"host_power_loss_qualified": false, "asset_bytes": 4 << 20, "observed_at": time.Now().UTC(),
		"go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"helper_samples_per_scenario":  1,
		"timing_boundary":              "real sandbox helper, HTTP/CBus/Manager, Git and checking schema hook; excludes actual schema engine",
		"sentry_setstat_error_overlay": true, "gvisor_sdk": os.Getenv("WORKFLOW_SPARSE_SDK")}
	if !t.Run("private_git_initialization_confines_live_paths", func(t *testing.T) {
		guard := &analysisSparseDriver{storage: t.TempDir(), shared: m.config.PackagesRoot}
		for _, name := range []string{"git", "upper"} {
			if err := os.MkdirAll(filepath.Join(guard.storage, name), 0700); err != nil {
				t.Fatal(err)
			}
		}
		outside := t.TempDir()
		alias := filepath.Join(guard.storage, "git/the8020")
		if err := os.Symlink(outside, alias); err != nil {
			t.Fatal(err)
		}
		if err := guard.initializeGit(ctx, "the8020/dev-core"); err == nil {
			t.Fatal("private Git initialization followed a path outside its root")
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
			t.Fatalf("Git initialization changed the outside directory: %v, %v", entries, err)
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := guard.initializeGit(ctx, "the8020/dev-core"); err != nil {
			t.Fatal(err)
		}
		if reference, err := os.ReadFile(filepath.Join(guard.storage, "upper/the8020/dev-core/.git")); err != nil || string(reference) != "gitdir: /workspace/git/private/the8020/dev-core/.git\n" {
			t.Fatalf("private Git initialization did not install its reference: %q: %v", reference, err)
		}
	}) {
		return
	}
	record["private_git_initialization_confines_live_paths"] = true
	var sdkFix map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("WORKFLOW_SPARSE_SDK_FIX")), &sdkFix); err != nil {
		t.Fatal(err)
	}
	record["sdk_setstat_fix"] = sdkFix
	hashes := map[string]string{}
	for _, name := range []string{"run.py", "runtime_test.go", "gofer_probe.go", "gofer_probe.py", "native_probe.go", "sparse_test.go", "sparse_activation_test.go", "activation-transaction.patch", "../model.go", "../manager.go", "../activation.go"} {
		body, err := os.ReadFile(filepath.Join("analysis", name))
		if err != nil {
			t.Fatal(err)
		}
		hashes[name] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	record["source_sha256"] = hashes
	guidanceTree, guidanceErr := gitOutput(filepath.Join(m.config.PackagesRoot, "the8020/dev-skills"), "rev-parse", "HEAD^{tree}")
	if guidanceErr != nil {
		t.Fatal(guidanceErr)
	}
	record["guidance_source_git_tree"] = guidanceTree
	var hostFS unix.Statfs_t
	if err := unix.Statfs(shared, &hostFS); err != nil {
		t.Fatal(err)
	}
	record["host_statfs_type"] = fmt.Sprintf("0x%x", hostFS.Type)
	commitShared := func(message string) {
		t.Helper()
		if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", message); err != nil {
			t.Fatal(err)
		}
	}
	activate := func(wantExit int) ActivationResult {
		t.Helper()
		started := time.Now()
		text := analysisExec(t, d, sandbox, "activate --json --message 'Native helper integration'; status=$?; test \"$status\" -eq "+strconv.Itoa(wantExit))
		var result ActivationResult
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			t.Fatalf("decode helper response %q: %v", text, err)
		}
		record["last_helper_ms"] = float64(time.Since(started).Microseconds()) / 1000
		return result
	}
	loadAttempt := func() analysisActivationAttempt {
		t.Helper()
		var attempt analysisActivationAttempt
		if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	// Include visible aliases, a host-only alias, and an alias hidden beneath
	// the native Git reference. Only the visible source names may be privatized.
	writeTestFile(t, filepath.Join(shared, "lower-a.txt"), "linked original\n")
	writeTestFile(t, filepath.Join(shared, "link-source.txt"), "linked original\n")
	writeTestFile(t, filepath.Join(shared, "rename-source.txt"), "first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "nested/keep.txt"), "untouched sibling\n")
	for _, name := range []string{"delete.txt", "nested/remove.txt"} {
		writeTestFile(t, filepath.Join(shared, name), "original deletion\n")
	}
	for _, alias := range []string{filepath.Join(shared, "lower-b.txt"), filepath.Join(shared, ".git/hidden-link"), filepath.Join(m.config.Root, "external-link")} {
		if err := os.Link(filepath.Join(shared, "lower-a.txt"), alias); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "add", "lower-a.txt", "lower-b.txt", "link-source.txt", "delete.txt", "nested/remove.txt", "rename-source.txt", "nested/keep.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Lower hardlink fixture")
	analysisExec(t, d, sandbox, "/root/metadata-client lower-links /workspace/packages/the8020/dev-core/lower-a.txt /workspace/packages/the8020/dev-core/lower-b.txt")
	analysisExec(t, d, sandbox, "/root/metadata-client link-lower /workspace/packages/the8020/dev-core/link-source.txt /workspace/packages/the8020/dev-core/link-created.txt")
	for _, filename := range []string{filepath.Join(shared, "lower-a.txt"), filepath.Join(shared, "lower-b.txt"), filepath.Join(shared, "link-source.txt"), filepath.Join(m.config.Root, "external-link")} {
		body, err := os.ReadFile(filename)
		if err != nil || string(body) != "linked original\n" {
			t.Fatalf("copy-up changed a shared/external alias: %s: %q: %v", filename, body, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sparse.storage, "upper/the8020/dev-core/.git/hidden-link")); !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("copy-up replaced the Git reference with a hidden lower alias: %v", err)
	}
	record["visible_lower_aliases_and_external_isolation"] = true
	record["native_link_from_lower_source"] = true
	analysisExec(t, d, sandbox, prefix+"for i in $(seq 1 8); do printf temporary >.label-$i.tmp; mv .label-$i.tmp same.txt; done")
	for i := 1; i <= 8; i++ {
		name := fmt.Sprintf("deleted/the8020/dev-core/.label-%d.tmp", i)
		if _, err := os.Stat(filepath.Join(sparse.storage, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ordinary atomic saves accumulated temporary deletion markers: %s: %v", name, err)
		}
	}
	record["temporary_saves_do_not_accumulate_deletion_markers"] = true
	events := make(chan string, 16)
	m.SetSchemaDeployment(analysisActivationHook{
		prepare: func(ctx context.Context, _ string, candidates []deployment.Candidate) error {
			if len(candidates) != 1 {
				return errors.New("expected one candidate")
			}
			candidate := candidates[0]
			if _, err := os.Stat(filepath.Join(candidate.Root, "rename-source.txt")); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("candidate retained the renamed source: %v", err)
			}
			for _, name := range []string{"delete.txt", "nested/remove.txt"} {
				if _, err := os.Stat(filepath.Join(candidate.Root, name)); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("candidate retained deleted path %s: %v", name, err)
				}
				if body, err := os.ReadFile(filepath.Join(shared, name)); err != nil || string(body) != "original deletion\n" {
					return fmt.Errorf("shared deletion preceded validation: %s: %v", name, err)
				}
			}
			for filename, want := range map[string]string{
				filepath.Join(shared, "same.txt"):                "shared label\n",
				filepath.Join(candidate.Root, "same.txt"):        "resolved label\n",
				filepath.Join(candidate.Root, "disjoint.txt"):    "shared newer first\n2\n3\n4\n5\n6\n7\nprivate last\n",
				filepath.Join(candidate.Root, "new.txt"):         "new before capture\n",
				filepath.Join(shared, "rename-source.txt"):       "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n",
				filepath.Join(candidate.Root, "renamed.txt"):     "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n",
				filepath.Join(candidate.Root, "nested/keep.txt"): "shared sibling update\n",
			} {
				body, err := os.ReadFile(filename)
				if err != nil || string(body) != want {
					return fmt.Errorf("schema-before-source %s: %q: %v", filename, body, err)
				}
			}
			for i := 0; i < 4; i++ {
				name := filepath.Join("assets", strconv.Itoa(i)+".bin")
				before, err := os.Stat(filepath.Join(shared, name))
				if err != nil {
					return err
				}
				linked, err := os.Stat(filepath.Join(candidate.Root, name))
				if err != nil || !os.SameFile(before, linked) {
					return fmt.Errorf("validation copied an asset: %s", name)
				}
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'later during schema\\n' >same.txt"); err != nil {
				return fmt.Errorf("agent could not edit during schema preparation: %w", err)
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf x >assets/0.bin"); err != nil {
				return fmt.Errorf("cannot start a new edit while validation holds a hardlink: %w", err)
			}
			for _, root := range []string{shared, candidate.Root} {
				info, err := os.Stat(filepath.Join(root, "assets/0.bin"))
				if err != nil || info.Size() != 1<<20 {
					return fmt.Errorf("new private asset edit changed source/validation: %v", err)
				}
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'recreated during validation\\n' >delete.txt"); err != nil {
				return fmt.Errorf("cannot recreate captured deletion during validation: %w", err)
			}
			var pending analysisActivationAttempt
			if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &pending); err != nil {
				return err
			}
			var originalCapture, duplicateID string
			for _, captured := range pending.Packages[0].Captures {
				if captured.Path == "new.txt" {
					originalCapture = captured.ID
				} else {
					duplicateID = captured.ID
				}
			}
			if _, err := sparse.exchange(ctx, "capture", "the8020/dev-core/new.txt", duplicateID); err == nil {
				return errors.New("duplicate snapshot capture unexpectedly succeeded")
			}
			registered, err := os.ReadFile(filepath.Join(sparse.storage, "snapshots/.pending/the8020/dev-core/new.txt"))
			if err != nil || string(registered) != originalCapture {
				return fmt.Errorf("failed recapture lost prior capture registration: %q: %v", registered, err)
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"rm new.txt"); err != nil {
				return fmt.Errorf("cannot delete a captured new file: %w", err)
			}
			events <- "prepare: complete view, old shared source, later private edit, new asset edit"
			return nil
		},
		complete: func(_ context.Context, _ string, activated bool) error {
			if !activated {
				return errors.New("unexpected schema rollback")
			}
			body, err := os.ReadFile(filepath.Join(shared, "same.txt"))
			if err != nil || string(body) != "resolved label\n" {
				return errors.New("schema completion preceded source publication")
			}
			events <- "complete: new source visible"
			return nil
		},
	})
	analysisExec(t, d, sandbox, prefix+"printf 'private label\\n' >same.txt; mkdir -p ignored; printf 'keep ignored\\n' >ignored/keep.txt; rm delete.txt nested/remove.txt; printf 'new before capture\\n' >new.txt")
	analysisExec(t, d, sandbox, prefix+"mv rename-source.txt renamed.txt")
	writeTestFile(t, filepath.Join(shared, "same.txt"), "shared label\n")
	writeTestFile(t, filepath.Join(shared, "disjoint.txt"), "shared first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "rename-source.txt"), "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "nested/keep.txt"), "shared sibling update\n")
	commitShared("First upstream update")
	if got := analysisExec(t, d, sandbox, prefix+"test -d nested; cat nested/keep.txt"); got != "shared sibling update\n" {
		t.Fatalf("nested deletion hid its live sibling: %q", got)
	}
	analysisExec(t, d, sandbox, prefix+"sed -i 's/^last$/private last/' disjoint.txt")
	writeTestFile(t, filepath.Join(shared, "disjoint.txt"), "shared newer first\n2\n3\n4\n5\n6\n7\nlast\n")
	commitShared("Second upstream update")
	first := activate(3)
	if first.Status != "conflicted" || len(first.Packages) != 1 || strings.Join(first.Packages[0].Conflicts, ",") != "same.txt" {
		t.Fatalf("expected native helper conflict: %+v", first)
	}
	record["first_helper_conflict"] = first
	record["conflict_helper_ms"] = record["last_helper_ms"]
	attempt := loadAttempt()
	for _, capture := range attempt.Packages[0].Captures {
		if _, err := sparse.exchange(ctx, "release", "the8020/dev-core/"+capture.Path, capture.ID); err == nil {
			t.Fatal("released an unacknowledged conflict capture")
		}
		if _, err := os.Stat(filepath.Join(sparse.storage, "snapshots", capture.ID)); err != nil {
			t.Fatal("rejected release removed a pending capture", err)
		}
	}
	worktree := attempt.Packages[0].Worktree
	resolution := "set -e; cd " + shellQuote(worktree) + "; "
	markers := analysisExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets")
	if !strings.Contains(markers, "|||||||") || !strings.HasSuffix(markers, "base\nprivate label\nshared label\n") {
		t.Fatalf("incorrect helper conflict originals: %q", markers)
	}
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	var err error
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysisExec(t, d, sandbox, resolution+"cat same.txt; git show :1:same.txt; git show :2:same.txt; git show :3:same.txt; test ! -e assets"); got != markers {
		t.Fatal("helper conflict did not survive recreation")
	}
	analysisExec(t, d, sandbox, prefix+"test lower-a.txt -ef lower-b.txt; test \"$(cat lower-b.txt)\" = 'private linked'")
	analysisExec(t, d, sandbox, prefix+"test link-source.txt -ef link-created.txt; test \"$(cat link-created.txt)\" = 'private linked'")
	record["private_lower_aliases_survive_recreation"] = true
	broker, err := platformconsole.New(platformconsole.Config{Authentication: sshProofAuthentication{}, Development: m})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	terminal, err := broker.CreateTerminal(ctx, "development", sandbox.SandboxID, backend.ConsoleOptions{
		Arguments: []string{"/bin/bash", "-l"}, WorkingDir: "/workspace/packages/the8020/dev-core", Terminal: true,
		Environment: []string{"TERM=xterm-256color", "HOME=/root", "PATH=" + developmentPath}, Size: backend.ConsoleSize{Columns: 90, Rows: 27},
	})
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("stty -echo; export PS1=''; printf '%s' $$ >/tmp/activation-tty-before; printf 'TTY-READY\\n'\n")); err != nil {
		t.Fatal(err)
	}
	sequence := retainedUntil(t, attachment, 0, "TTY-READY")
	attachment.Close()
	conflictUI := func(request map[string]any, wantFailure bool) map[string]any {
		t.Helper()
		request["worktree"] = worktree
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		response, err := m.Shell(ctx, sandbox.UserID, "printf %s "+shellQuote(string(body))+" | deno run --allow-read --allow-write --allow-run=/usr/bin/git --allow-env=DEVELOPMENT_USER_ID /workspace/scripts/activation-conflicts.ts")
		if wantFailure {
			if err == nil {
				t.Fatal("conflict editor accepted a stale write")
			}
			return nil
		}
		if err != nil {
			t.Fatalf("native conflict editor: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(response.Output), &result); err != nil {
			t.Fatalf("conflict editor response %q: %v", response.Output, err)
		}
		return result
	}
	opened := conflictUI(map[string]any{"action": "read", "path": "same.txt"}, false)
	if opened["original"] != "base\n" || opened["shared"] != "shared label\n" {
		t.Fatalf("editor lost Git stages: %+v", opened)
	}
	analysisExec(t, d, sandbox, resolution+"printf 'agent edit after UI opened\\n' >same.txt")
	conflictUI(map[string]any{"action": "save", "path": "same.txt", "version": opened["version"], "content": "stale browser edit\n"}, true)
	opened = conflictUI(map[string]any{"action": "read", "path": "same.txt"}, false)
	conflictUI(map[string]any{"action": "save", "path": "same.txt", "version": opened["version"], "content": "resolved label\n"}, false)
	conflictUI(map[string]any{"action": "finish"}, false)
	record["native_uui_adapter_shares_git_stages_rejects_stale_saves_and_resolves"] = true
	analysisExec(t, d, sandbox, prefix+"printf 'later primary save\\n' >same.txt")
	writeTestFile(t, filepath.Join(shared, "untouched.txt"), "shared during resolution\n")
	commitShared("Shared advancement during resolution")
	second := activate(0)
	if !second.Success || second.Status != "committed" || second.OverlayReset || second.OverlayResetPending {
		t.Fatalf("helper failed native retry/publication: %+v", second)
	}
	record["successful_helper"] = second
	record["successful_helper_ms"] = record["last_helper_ms"]
	if _, err := os.Stat(filepath.Join(shared, "rename-source.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication retained the renamed source: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(shared, "renamed.txt")); err != nil || string(body) != "shared rename first\n2\n3\n4\n5\n6\n7\nlast\n" {
		t.Fatalf("publication lost the shared edit of a renamed file: %q: %v", body, err)
	}
	analysisExec(t, d, sandbox, prefix+"test ! -e rename-source.txt; test \"$(head -n 1 renamed.txt)\" = 'shared rename first'")
	record["lower_rename_with_shared_edit_published"] = true
	if body, err := os.ReadFile(filepath.Join(shared, "new.txt")); err != nil || string(body) != "new before capture\n" {
		t.Fatalf("activation did not publish its captured new file: %q: %v", body, err)
	}
	analysisExec(t, d, sandbox, prefix+"test ! -e new.txt")
	record["later_deletion_of_captured_new_file_preserved"] = true
	record["failed_recapture_retains_prior_capture_registration"] = true
	for _, name := range []string{"delete.txt", "nested/remove.txt"} {
		if _, err := os.Stat(filepath.Join(shared, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("activation failed to publish deletion %s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(sparse.storage, "base/the8020/dev-core", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deletion retained an obsolete original for %s: %v", name, err)
		}
	}
	if got := analysisExec(t, d, sandbox, prefix+"test ! -e nested/remove.txt; cat delete.txt"); got != "recreated during validation\n" {
		t.Fatalf("publication lost a later recreation: %q", got)
	}
	writeTestFile(t, filepath.Join(shared, "nested/remove.txt"), "shared recreation\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "nested/remove.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Recreate a published deletion")
	if got := analysisExec(t, d, sandbox, prefix+"cat nested/remove.txt"); got != "shared recreation\n" {
		t.Fatalf("published deletion still hides later shared creation: %q", got)
	}
	record["deletion_publication_and_later_recreation"] = true
	record["nested_deletion_keeps_live_siblings"] = true
	if got := analysisExec(t, d, sandbox, prefix+"cat same.txt untouched.txt ignored/keep.txt disjoint.txt"); got != "later during schema\nshared during resolution\nkeep ignored\nshared newer first\n2\n3\n4\n5\n6\n7\nprivate last\n" {
		t.Fatalf("activation lost later, ignored, or merged work: %q", got)
	}
	attachment, err = terminal.Attach(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachment.Write([]byte("printf '%s' $$ >/tmp/activation-tty-after; printf 'TTY-SURVIVED\\n'\n")); err != nil {
		t.Fatal(err)
	}
	retainedUntil(t, attachment, sequence, "TTY-SURVIVED")
	attachment.Close()
	analysisExec(t, d, sandbox, "cmp /tmp/activation-tty-before /tmp/activation-tty-after")
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	for _, capture := range attempt.Packages[0].Captures {
		for _, payload := range []string{"file", "base"} {
			if _, err := os.Stat(filepath.Join(sparse.storage, "snapshots", capture.ID, payload)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("completed activation retained its capture payload", err)
			}
		}
	}
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	for _, capture := range attempt.Packages[0].Captures {
		name := "the8020/dev-core/" + capture.Path
		answer, err := sparse.exchange(ctx, "acknowledge", name, capture.ID)
		if err != nil || answer["status"] != "already_acknowledged" {
			t.Fatalf("recreated receipt lost acknowledgement: %v: %v", answer, err)
		}
		if _, err := sparse.exchange(ctx, "release", name, capture.ID); err != nil {
			t.Fatal("recreated release was not idempotent", err)
		}
		if _, err := sparse.exchange(ctx, "capture", "the8020/dev-core/same.txt", capture.ID); err == nil || !strings.Contains(err.Error(), "file exists") {
			t.Fatal("released capture ID was not reserved", err)
		}
	}
	record["capture_release_and_recreated_receipt_passed"] = true
	var schemaEvents []string
	for i := 0; i < 2; i++ {
		select {
		case event := <-events:
			schemaEvents = append(schemaEvents, event)
		case <-time.After(5 * time.Second):
			t.Fatal("missing schema boundary event")
		}
	}
	record["schema_boundary_events"] = schemaEvents
	record["edit_while_validation_holds_hardlink"] = true
	third := activate(3)
	if third.Status != "conflicted" {
		t.Fatalf("expected later-edit merge conflict: %+v", third)
	}
	attempt = loadAttempt()
	next := attempt.Packages[0].Worktree
	if got := analysisExec(t, d, sandbox, "git -C "+shellQuote(next)+" ls-files -- new.txt"); got != "" {
		t.Fatalf("next activation lost deletion of a captured new file: %q", got)
	}
	if got := analysisExec(t, d, sandbox, "git -C "+shellQuote(next)+" show HEAD:delete.txt; git -C "+shellQuote(next)+" ls-files -u -- delete.txt"); got != "recreated during validation\n" {
		t.Fatalf("recreated deletion did not merge as a new file: %q", got)
	}
	if got := analysisExec(t, d, sandbox, "git -C "+shellQuote(next)+" show :1:same.txt"); got != "private label\n" {
		t.Fatalf("next merge used an original the editor never saw: %q", got)
	}
	record["later_activation_uses_captured_private_original"] = true
	analysisExec(t, d, sandbox, "set -e; cd "+shellQuote(next)+"; printf 'resolved later label\\n' >same.txt; git add same.txt; git -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Resolve later label'")
	// Both failures retain this same native resolution and the original capture.
	// Validation must neither consume later edits nor publish stale candidates.
	preparations, rollbacks, completions := 0, 0, 0
	m.SetSchemaDeployment(analysisActivationHook{
		prepare: func(ctx context.Context, _ string, candidates []deployment.Candidate) error {
			preparations++
			if len(candidates) != 1 {
				return errors.New("expected one retry candidate")
			}
			if body, err := os.ReadFile(filepath.Join(candidates[0].Root, "same.txt")); err != nil || string(body) != "resolved later label\n" {
				return fmt.Errorf("retry lost native resolution: %q: %v", body, err)
			}
			switch preparations {
			case 1:
				if _, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'private after failed validation\\n' >same.txt"); err != nil {
					return err
				}
				return errors.New("fixture schema validation rejected the candidate")
			case 2:
				if err := os.WriteFile(filepath.Join(shared, "untouched.txt"), []byte("shared during validation\n"), 0644); err != nil {
					return err
				}
				_, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Shared changed during validation")
				return err
			case 3:
				if body, err := os.ReadFile(filepath.Join(candidates[0].Root, "untouched.txt")); err != nil || string(body) != "shared during validation\n" {
					return fmt.Errorf("retry omitted latest shared change: %q: %v", body, err)
				}
				return nil
			default:
				return errors.New("unexpected validation retry")
			}
		},
		complete: func(_ context.Context, _ string, activated bool) error {
			if activated {
				completions++
			} else {
				rollbacks++
			}
			return nil
		},
	})
	for failure := 1; failure <= 2; failure++ {
		failed := activate(3)
		if failed.Success || failed.Status != "failed" {
			t.Fatalf("expected validation/source failure %d: %+v", failure, failed)
		}
		pending := loadAttempt()
		if pending.ID != attempt.ID || pending.Phase != "preparing" {
			t.Fatalf("failure replaced or consumed the native attempt: %+v", pending)
		}
		if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "resolved label\n" {
			t.Fatalf("failure published candidate source: %q: %v", body, err)
		}
		if got := analysisExec(t, d, sandbox, prefix+"cat same.txt"); got != "private after failed validation\n" {
			t.Fatalf("failure consumed a later edit: %q", got)
		}
	}
	if preparations != 2 || rollbacks != 2 || completions != 0 {
		t.Fatalf("incorrect failure handshake: prepare=%d rollback=%d complete=%d", preparations, rollbacks, completions)
	}
	sparse.controlMu.Lock()
	sparse.loseReleaseReply = true
	sparse.controlMu.Unlock()
	cleanupFailed := activate(3)
	record["publication_release_reply_failure_ms"] = record["last_helper_ms"]
	if cleanupFailed.Success || loadAttempt().Phase != "published" || preparations != 3 || rollbacks != 3 || completions != 1 {
		t.Fatalf("release reply failure lost its published attempt: %+v", cleanupFailed)
	}
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	retried := activate(0)
	if !retried.Success || retried.OverlayReset || preparations != 3 || rollbacks != 3 || completions != 2 {
		t.Fatalf("failed to continue the retained activation: %+v; prepare=%d rollback=%d complete=%d", retried, preparations, rollbacks, completions)
	}
	record["release_reply_failure_recreated_retry_passed"] = true
	if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "resolved later label\n" {
		t.Fatalf("retry lost resolved content: %q: %v", body, err)
	}
	if got := analysisExec(t, d, sandbox, prefix+"cat same.txt untouched.txt"); got != "private after failed validation\nshared during validation\n" {
		t.Fatalf("retry lost later/private or shared work: %q", got)
	}
	record["validation_failure_retains_attempt_and_later_edits"] = true
	record["shared_advance_during_validation_rolls_back_and_retries"] = true
	record["release_reply_retry_helper_ms"] = record["last_helper_ms"]
	record["native_helper_loop_passed"] = true
	writeTestFile(t, filepath.Join(shared, "directory-intent/file.txt"), "directory original\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "directory-intent/file.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Directory activation fixture")
	analysisExec(t, d, sandbox, prefix+"rm -r directory-intent")
	changes := sparse.request(t, "changes", "the8020/dev-core", "directory-intent")
	if got := fmt.Sprint(changes["directories"]); got != "[the8020/dev-core/directory-intent]" {
		t.Fatalf("directory removal was missing from activation changes: %v", changes)
	}
	writeTestFile(t, filepath.Join(shared, "directory-intent/file.txt"), "shared directory edit\n")
	writeTestFile(t, filepath.Join(shared, "directory-intent/new.txt"), "new shared child\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "directory-intent"); err != nil {
		t.Fatal(err)
	}
	commitShared("Shared edit and addition under removed directory")
	currentLabel, err := os.ReadFile(filepath.Join(shared, "same.txt"))
	if err != nil {
		t.Fatal(err)
	}
	analysisExec(t, d, sandbox, prefix+"printf %s "+shellQuote(string(currentLabel))+" >same.txt")
	m.SetSchemaDeployment(analysisActivationHook{
		prepare: func(_ context.Context, _ string, candidates []deployment.Candidate) error {
			if len(candidates) != 1 {
				return errors.New("directory fixture expected one package")
			}
			if _, err := os.Stat(filepath.Join(candidates[0].Root, "directory-intent/file.txt")); !os.IsNotExist(err) {
				return fmt.Errorf("directory candidate retained the resolved deletion: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(candidates[0].Root, "directory-intent/new.txt")); err != nil || string(got) != "new shared child\n" {
				return fmt.Errorf("directory candidate lost the shared addition: %q: %v", got, err)
			}
			return nil
		},
		complete: func(context.Context, string, bool) error { return nil },
	})
	directoryConflict := activate(3)
	if directoryConflict.Status != "conflicted" || len(directoryConflict.Packages) != 1 || fmt.Sprint(directoryConflict.Packages[0].Conflicts) != "[directory-intent/file.txt]" {
		t.Fatalf("directory deletion did not produce the native modify/delete conflict: %+v", directoryConflict)
	}
	directoryAttempt := loadAttempt()
	directoryWorktree := directoryAttempt.Packages[0].Worktree
	if got := analysisExec(t, d, sandbox, "git -C "+shellQuote(directoryWorktree)+" show :1:directory-intent/file.txt; git -C "+shellQuote(directoryWorktree)+" show :3:directory-intent/file.txt"); got != "directory original\nshared directory edit\n" {
		t.Fatalf("directory conflict lost native stages: %q", got)
	}
	analysisExec(t, d, sandbox, "git -C "+shellQuote(directoryWorktree)+" rm directory-intent/file.txt; git -C "+shellQuote(directoryWorktree)+" -c user.name=Analysis -c user.email=analysis@example.test commit -qm 'Resolve directory deletion'")
	if result := activate(0); !result.Success {
		t.Fatalf("directory publication failed: %+v", result)
	}
	if got := analysisExec(t, d, sandbox, prefix+"test ! -e directory-intent/file.txt; cat directory-intent/new.txt"); got != "new shared child\n" {
		t.Fatalf("directory publication left the workspace stale: %q", got)
	}
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[]" {
		t.Fatalf("completed directory intent remains pending: %s", got)
	}
	record["directory_modify_delete_conflict_and_new_shared_child_published"] = true
	analysisExec(t, d, sandbox, prefix+"rm -r directory-intent")
	directoryPreparations := 0
	m.SetSchemaDeployment(analysisActivationHook{
		prepare: func(context.Context, string, []deployment.Candidate) error {
			directoryPreparations++
			if directoryPreparations == 1 {
				// This later create/remove cycle must remain pending even though
				// the directory is absent both before and after it.
				_, err := d.Exec(ctx, sandbox.SandboxID, prefix+"mkdir directory-intent; printf later >directory-intent/temporary; rm -r directory-intent")
				return err
			}
			var attempt analysisActivationAttempt
			if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil {
				return err
			}
			if len(attempt.Packages) != 1 || len(attempt.Packages[0].Captures) != 0 {
				return errors.New("directory-only fixture unexpectedly captured files")
			}
			if _, err := d.Exec(ctx, sandbox.SandboxID, "test ! -e "+shellQuote(attempt.Packages[0].Worktree+"/assets")); err != nil {
				return fmt.Errorf("directory-only worktree materialized assets: %w", err)
			}
			return nil
		},
		complete: func(context.Context, string, bool) error { return nil },
	})
	sparse.loseReleaseReply = true
	failedDirectory := activate(3)
	if failedDirectory.Success || !strings.Contains(failedDirectory.Error, "lost capture-release reply") {
		t.Fatalf("directory fixture did not interrupt after acknowledgement: %+v", failedDirectory)
	}
	pendingDirectory := loadAttempt()
	if pendingDirectory.Phase != "published" || len(pendingDirectory.Packages[0].Directories) != 1 {
		t.Fatalf("directory attempt was not retained: %+v", pendingDirectory)
	}
	oldDirectoryCapture := pendingDirectory.Packages[0].Directories[0]
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[the8020/dev-core/directory-intent]" {
		t.Fatalf("publication consumed a later directory operation: %s", got)
	}
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if result := activate(0); !result.Success || directoryPreparations != 1 {
		t.Fatalf("recreated directory retry repeated preparation: %+v, prepares=%d", result, directoryPreparations)
	}
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[the8020/dev-core/directory-intent]" {
		t.Fatalf("recreated acknowledgement consumed later directory intent: %s", got)
	}
	// Git versions files. A remaining directory-only intent needs no file
	// checkout; acknowledging this later generation restores live lower paths.
	if result := activate(0); !result.Success || directoryPreparations != 2 {
		t.Fatalf("directory-only activation failed: %+v, prepares=%d", result, directoryPreparations)
	}
	if got := fmt.Sprint(sparse.request(t, "changes", "the8020/dev-core", "directory-intent")["directories"]); got != "[]" {
		t.Fatalf("later directory generation was not acknowledged: %s", got)
	}
	if _, err := sparse.exchange(ctx, "acknowledge-directory", "the8020/dev-core/"+oldDirectoryCapture.Path, oldDirectoryCapture.ID); err == nil || !strings.Contains(err.Error(), "stale file handle") {
		t.Fatalf("stale directory acknowledgement was not rejected: %v", err)
	}
	writeTestFile(t, filepath.Join(shared, "directory-intent/after.txt"), "shared after directory publication\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "directory-intent/after.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Shared recreation after directory publication")
	if got := analysisExec(t, d, sandbox, prefix+"cat directory-intent/after.txt"); got != "shared after directory publication\n" {
		t.Fatalf("acknowledged directory stayed obsolete: %q", got)
	}
	record["directory_later_generation_recreated_retry_and_live_retirement"] = true
	writeTestFile(t, filepath.Join(shared, "ignored/shared/tracked.txt"), "newly tracked upstream\n")
	if _, err := gitCommand(ctx, shared, nil, "add", "-f", "ignored/shared/tracked.txt"); err != nil {
		t.Fatal(err)
	}
	commitShared("Track a new file under an ignored directory")
	analysisExec(t, d, sandbox, prefix+"rm -r ignored/shared")
	m.SetSchemaDeployment(analysisActivationHook{
		prepare:  func(context.Context, string, []deployment.Candidate) error { return nil },
		complete: func(context.Context, string, bool) error { return nil },
	})
	if result := activate(0); !result.Success || result.Status != "committed" {
		t.Fatalf("newly shared tracked file was treated as ignored: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(shared, "ignored/shared/tracked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracked deletion under an ignored directory was not published: %v", err)
	}
	record["new_shared_tracked_files_override_private_ignore_results"] = true
	analysisExec(t, d, sandbox, "deno eval "+shellQuote(`
const root = "/workspace/packages/the8020/workflow-new";
Deno.mkdirSync(root + "/programs/hello", {recursive: true});
Deno.writeTextFileSync(root + "/package.toml", "schema = 1\n");
Deno.writeTextFileSync(root + "/programs/hello/program.toml", "schema = 1\nentrypoint = \"main.ts\"\n");
Deno.writeTextFileSync(root + "/programs/hello/main.ts", "export default () => 'created';\n");
Deno.writeTextFileSync("/workspace/packages/the8020/dev-core/same.txt", "edited alongside new package\n");
`))
	created := activate(0)
	if !created.Success || len(created.Packages) != 2 {
		data, _ := json.MarshalIndent(created, "", "  ")
		if err := os.WriteFile("analysis/sparse-package-creation-failure.json", append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("ordinary package creation was omitted from the activation batch: %+v", created)
	}
	if body, err := os.ReadFile(filepath.Join(m.config.PackagesRoot, "the8020/workflow-new/programs/hello/main.ts")); err != nil || string(body) != "export default () => 'created';\n" {
		t.Fatalf("new package was not published: %q: %v", body, err)
	}
	record["ordinary_new_package_and_existing_edit_published_together"] = true
	analysisExec(t, d, sandbox, "rm -r /workspace/packages/the8020/workflow-new")
	newRoot := filepath.Join(m.config.PackagesRoot, "the8020/workflow-new")
	writeTestFile(t, filepath.Join(newRoot, "programs/hello/main.ts"), "export default () => 'upstream edit';\n")
	writeTestFile(t, filepath.Join(newRoot, "upstream-new.txt"), "added while package was removed\n")
	if _, err := gitCommand(ctx, newRoot, nil, "add", "upstream-new.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, newRoot, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Concurrent shared edit"); err != nil {
		t.Fatal(err)
	}
	deleted := activate(3)
	if deleted.Status != "conflicted" || len(deleted.Packages) != 1 || len(deleted.Packages[0].Conflicts) != 2 {
		t.Fatalf("package deletion lost concurrent edits: %+v", deleted)
	}
	deletionAttempt := loadAttempt()
	deletionWorktree := deletionAttempt.Packages[0].Worktree
	analysisExec(t, d, sandbox, "git -C "+shellQuote(deletionWorktree)+" rm programs/hello/main.ts upstream-new.txt && git -C "+shellQuote(deletionWorktree)+" -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Resolve package removal'")
	if result := activate(0); !result.Success {
		t.Fatalf("resolved package deletion: %+v", result)
	}
	if _, err := os.Stat(newRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed package still exists: %v", err)
	}
	record["ordinary_package_removal_conflicts_and_native_git_resolution"] = true
	upstreamID := "the8020/workflow-upstream-removed"
	analysisExec(t, d, sandbox, "mkdir -p /workspace/packages/"+upstreamID+"; printf 'schema = 1\\n' >/workspace/packages/"+upstreamID+"/package.toml; printf 'original\\n' >/workspace/packages/"+upstreamID+"/label.txt")
	activate(0)
	analysisExec(t, d, sandbox, "printf 'private after upstream deletion\\n' >/workspace/packages/"+upstreamID+"/label.txt")
	if err := os.RemoveAll(filepath.Join(m.config.PackagesRoot, upstreamID)); err != nil {
		t.Fatal(err)
	}
	removedUpstream := activate(3)
	if len(removedUpstream.Packages) != 1 || len(removedUpstream.Packages[0].Conflicts) != 1 || removedUpstream.Packages[0].Conflicts[0] != "label.txt" {
		t.Fatalf("upstream removal hid private work: %+v", removedUpstream)
	}
	resolutionRoot := removedUpstream.Packages[0].ConflictWorktree
	analysisExec(t, d, sandbox, "git -C "+shellQuote(resolutionRoot)+" rm label.txt && git -C "+shellQuote(resolutionRoot)+" -c user.name=Fixture -c user.email=fixture@example.test commit -qm 'Accept upstream removal'")
	if result := activate(0); !result.Success {
		t.Fatalf("accept upstream removal: %+v", result)
	}
	record["upstream_package_deletion_preserves_private_conflict_and_resolution"] = true
	if err := os.MkdirAll(filepath.Join(shared, "symbolic"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"remove", "from", "changed"} {
		if err := os.Symlink("/tmp/original-link-target", filepath.Join(shared, "symbolic", name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitCommand(ctx, shared, nil, "add", "symbolic"); err != nil {
		t.Fatal(err)
	}
	commitShared("Shared symlink fixtures")
	analysisExec(t, d, sandbox, prefix+"rm symbolic/remove && mv symbolic/from symbolic/moved && ln -sfn /tmp/private-link-target symbolic/changed")
	if err := os.Remove(filepath.Join(shared, "symbolic/changed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/shared-link-target", filepath.Join(shared, "symbolic/changed")); err != nil {
		t.Fatal(err)
	}
	commitShared("Concurrent shared symlink edit")
	symlinkConflict := activate(3)
	if len(symlinkConflict.Packages) != 1 || len(symlinkConflict.Packages[0].Conflicts) != 1 || symlinkConflict.Packages[0].Conflicts[0] != "symbolic/changed" {
		t.Fatalf("symlink changes did not retain a native conflict: %+v", symlinkConflict)
	}
	worktree = symlinkConflict.Packages[0].ConflictWorktree
	linkConflict := conflictUI(map[string]any{"action": "read", "path": "symbolic/changed"}, false)
	if linkConflict["binary"] != true || linkConflict["hasShared"] != true || linkConflict["original"] != "/tmp/original-link-target" || linkConflict["private"] != "/tmp/private-link-target" || linkConflict["shared"] != "/tmp/shared-link-target" {
		t.Fatalf("conflict helper did not expose the link targets and side choices: %+v", linkConflict)
	}
	conflictUI(map[string]any{"action": "shared", "path": "symbolic/changed", "version": linkConflict["version"]}, false)
	conflictUI(map[string]any{"action": "finish"}, false)
	if result := activate(0); !result.Success {
		t.Fatalf("symlink resolution publication: %+v", result)
	}
	for _, name := range []string{"remove", "from"} {
		if _, err := os.Lstat(filepath.Join(shared, "symbolic", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("symlink deletion was not published: %s: %v", name, err)
		}
	}
	for name, target := range map[string]string{"moved": "/tmp/original-link-target", "changed": "/tmp/shared-link-target"} {
		if value, err := os.Readlink(filepath.Join(shared, "symbolic", name)); err != nil || value != target {
			t.Fatalf("published symlink %s = %q: %v", name, value, err)
		}
	}
	record["native_symlink_delete_rename_conflict_and_publication_without_target_reads"] = true
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("analysis/sparse-activation-results.json", append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("PASS real helper conflict/resolution/retry, per-path originals, lower aliases, validation-time edits, schema handshake, ignored files, and retained PTY; full transaction recovery remains unqualified")
}

func TestWorkflowAnalysisRename(t *testing.T) {
	m, sparse, sandbox, shared := analysisSparseRuntime(t, "rename", 4)
	m.SetSchemaDeployment(analysisActivationHook{
		prepare:  func(context.Context, string, []deployment.Candidate) error { return nil },
		complete: func(context.Context, string, bool) error { return nil },
	})
	d, ctx := sparse.RunscDriver, context.Background()
	prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
	commitShared := func(message string) {
		t.Helper()
		if _, err := gitCommand(ctx, shared, nil, "add", "-A"); err != nil {
			t.Fatal(err)
		}
		if _, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-m", message); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"assets/nested/merge.txt": "first\n2\n3\n4\n5\n6\n7\nlast\n",
		"assets/conflict.txt":     "original title\n",
		"assets/annotated.txt":    "original heading\nalpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\n",
		"assets/removed.txt":      "remove me\n",
		"assets/untouched.txt":    "original unchanged text\n",
		"assets/after-rename.txt": "original before rename\n",
	} {
		writeTestFile(t, filepath.Join(shared, name), body)
	}
	commitShared("Directory rename inputs")
	analysisExec(t, d, sandbox, prefix+"printf 'private title\n' >assets/conflict.txt; sed -i 's/original heading/private heading/' assets/annotated.txt; sed -i 's/^last$/private last/' assets/nested/merge.txt; printf 'new file\n' >assets/new.txt; rm assets/removed.txt; mkdir -p empty/nested target; printf occupied >target/keep")
	started := time.Now()
	analysisExec(t, d, sandbox, prefix+"exec 9<assets/untouched.txt; cd assets/nested; mv /workspace/packages/the8020/dev-core/assets /workspace/packages/the8020/dev-core/images; test \"$(pwd -P)\" = /workspace/packages/the8020/dev-core/images/nested; test \"$(cat <&9)\" = 'original unchanged text'; cd ../..; mv images artwork; mv artwork/0.bin artwork/icon.bin; mv empty renamed-empty; test -d renamed-empty/nested; test ! -e assets; test ! -e images; test ! -e empty; test ! -e artwork/removed.txt; test \"$(cat artwork/new.txt)\" = 'new file'; test \"$(cat artwork/conflict.txt)\" = 'private title'; if mv -T artwork target 2>/dev/null; then exit 1; fi; test -f artwork/icon.bin; test -f target/keep")
	renameMS := float64(time.Since(started).Microseconds()) / 1000
	analysisExec(t, d, sandbox, prefix+"printf 'edited after rename\n' >artwork/after-rename.txt")
	asset, err := os.ReadFile(filepath.Join(shared, "assets/0.bin"))
	if err != nil {
		t.Fatal(err)
	}
	wantAsset := fmt.Sprintf("%x", sha256.Sum256(asset))
	if got := strings.Fields(analysisExec(t, d, sandbox, prefix+"sha256sum artwork/icon.bin")); len(got) == 0 || got[0] != wantAsset {
		t.Fatalf("renamed asset contents: %v", got)
	}
	noAssetCopies := func() {
		t.Helper()
		if err := filepath.WalkDir(sparse.storage, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(sparse.storage, name)
			if err != nil {
				return err
			}
			if relative == "lower" || relative == "borrowed" {
				return filepath.SkipDir
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Size() >= 1<<20 {
					return fmt.Errorf("rename copied asset-sized content into %s (%d bytes)", relative, info.Size())
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	noAssetCopies()
	// A failed metadata checkpoint must restore both the moved private subtree
	// and an existing destination, without removing the rollback backup first.
	failurePath := filepath.Join(sparse.storage, "snapshots/.references-new")
	if err := os.Mkdir(failurePath, 0700); err != nil {
		t.Fatal(err)
	}
	analysisExec(t, d, sandbox, prefix+"mkdir empty-target; if mv -T artwork empty-target 2>/dev/null; then exit 1; fi; test -f artwork/icon.bin; test \"$(cat artwork/new.txt)\" = 'new file'; test -d empty-target; test ! -e empty-target/icon.bin")
	if err := os.Remove(failurePath); err != nil {
		t.Fatal(err)
	}
	// References must remain usable after the underlying path changes, and after
	// an ordinary runtime recreation. Neither operation copies the asset set.
	writeTestFile(t, filepath.Join(shared, "assets/untouched.txt"), "upstream changed text\n")
	commitShared("Update referenced text")
	analysisExec(t, d, sandbox, prefix+"test \"$(cat artwork/untouched.txt)\" = 'original unchanged text'")
	if err := d.Kill(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, sandbox.SandboxID); err != nil {
		t.Fatal(err)
	}
	sandbox, err = m.Start(ctx, sandbox.UserID)
	if err != nil {
		t.Fatal(err)
	}
	analysisExec(t, d, sandbox, prefix+"test -d renamed-empty/nested; test -f artwork/icon.bin; test \"$(cat artwork/untouched.txt)\" = 'original unchanged text'; rm artwork/3.bin; test ! -e artwork/3.bin; if rmdir artwork 2>/dev/null; then exit 1; fi")
	noAssetCopies()
	writeTestFile(t, filepath.Join(shared, "assets/conflict.txt"), "shared title\n")
	writeTestFile(t, filepath.Join(shared, "assets/nested/merge.txt"), "shared first\n2\n3\n4\n5\n6\n7\nlast\n")
	writeTestFile(t, filepath.Join(shared, "assets/annotated.txt"), "shared heading\nalpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\n")
	commitShared("Concurrent edits at original paths")
	activate := func(exit int) ActivationResult {
		t.Helper()
		body := analysisExec(t, d, sandbox, "activate --json --message 'Directory rename'; status=$?; test \"$status\" -eq "+strconv.Itoa(exit))
		var result ActivationResult
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatalf("decode activation: %q: %v", body, err)
		}
		return result
	}
	first := activate(3)
	if first.Status != "conflicted" || len(first.Packages) != 1 {
		t.Fatalf("expected shared native conflict: %+v", first)
	}
	var attempt analysisActivationAttempt
	if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &attempt); err != nil {
		t.Fatal(err)
	}
	worktree := attempt.Packages[0].Worktree
	analysisExec(t, d, sandbox, "test ! -e "+shellQuote(worktree+"/artwork/icon.bin"))
	conflicts := analysisExec(t, d, sandbox, "git -C "+shellQuote(worktree)+" ls-files -u")
	if !strings.Contains(conflicts, "assets/conflict.txt") || !strings.Contains(conflicts, "artwork/annotated.txt") {
		t.Fatalf("missing renamed conflict index: %s", conflicts)
	}
	analysisExec(t, d, sandbox, "set -e; cd "+shellQuote(worktree)+"; test \"$(head -c 7 artwork/annotated.txt)\" = '<<<<<<<'; printf 'resolved title\n' >artwork/conflict.txt; printf 'resolved heading\nalpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\n' >artwork/annotated.txt; git rm assets/conflict.txt; git add artwork/conflict.txt artwork/annotated.txt; git -c user.name=Fixture -c user.email=fixture@example.test commit -m 'Resolve renamed file'")
	noAssetCopies()
	m.SetSchemaDeployment(analysisActivationHook{
		prepare: func(ctx context.Context, _ string, candidates []deployment.Candidate) error {
			if len(candidates) != 1 {
				return errors.New("expected one renamed candidate")
			}
			candidate := candidates[0].Root
			for _, pair := range [][2]string{{"assets/0.bin", "artwork/icon.bin"}, {"assets/1.bin", "artwork/1.bin"}, {"assets/2.bin", "artwork/2.bin"}} {
				a, err := os.Stat(filepath.Join(shared, pair[0]))
				if err != nil {
					return err
				}
				b, err := os.Stat(filepath.Join(candidate, pair[1]))
				if err != nil || !os.SameFile(a, b) {
					return fmt.Errorf("candidate copied renamed asset %s: %v", pair[1], err)
				}
			}
			_, err := d.Exec(ctx, sandbox.SandboxID, prefix+"printf 'later private edit\n' >artwork/nested/merge.txt; printf alive >/tmp/rename-process-marker")
			return err
		}, complete: func(context.Context, string, bool) error { return nil },
	})
	started = time.Now()
	last := activate(0)
	activationMS := float64(time.Since(started).Microseconds()) / 1000
	if !last.Success {
		t.Fatalf("rename publication: %+v", last)
	}
	for name, want := range map[string]string{
		"artwork/conflict.txt":     "resolved title\n",
		"artwork/nested/merge.txt": "shared first\n2\n3\n4\n5\n6\n7\nprivate last\n",
		"artwork/untouched.txt":    "upstream changed text\n",
		"artwork/new.txt":          "new file\n",
		"artwork/after-rename.txt": "edited after rename\n",
	} {
		body, err := os.ReadFile(filepath.Join(shared, name))
		if err != nil || string(body) != want {
			t.Fatalf("published %s: %q, %v", name, body, err)
		}
	}
	for _, name := range []string{"assets/0.bin", "artwork/3.bin", "artwork/removed.txt"} {
		if _, err := os.Stat(filepath.Join(shared, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted path %s remains: %v", name, err)
		}
	}
	analysisExec(t, d, sandbox, prefix+"test \"$(cat artwork/nested/merge.txt)\" = 'later private edit'; test \"$(cat /tmp/rename-process-marker)\" = alive; test ! -e artwork/3.bin; test -f artwork/icon.bin")
	noAssetCopies()
	record := map[string]any{"observed_at": time.Now().UTC(), "asset_bytes": 4 << 20, "native_file_and_directory_renames": true,
		"mixed_private_new_deleted_files": true, "working_directory_and_open_file_preserved": true, "recreation": true,
		"native_git_conflict_resolution_and_activation": true, "later_edits_preserved": true, "private_asset_copies": 0,
		"validation_reuses_moved_asset_inodes": true, "rename_command_ms": renameMS, "resolved_activation_ms": activationMS,
		"failed_metadata_checkpoint_restores_rename": true, "reference_copy_up_on_edit": true,
		"timing_boundary": "one native shell rename batch / one resolved helper activation, including transport; checking hook, no real schema engine"}
	hashes := map[string]string{}
	for _, name := range []string{"gofer_probe.go", "sparse_test.go", "sparse_activation_test.go", "run.py"} {
		body, err := os.ReadFile(filepath.Join("analysis", name))
		if err != nil {
			t.Fatal(err)
		}
		hashes[name] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	record["source_sha256"] = hashes
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("analysis/rename-results.json", append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("rename batch %.1f ms; resolved activation %.1f ms; zero private asset copies", renameMS, activationMS)
}
