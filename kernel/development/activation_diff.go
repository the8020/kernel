package development

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

const activationDiffLimit = 128 << 10

func validatePreviewFile(options ActivationOptions) error {
	if options.PreviewFile == "" {
		return nil
	}
	if len(options.SelectedPackages) != 1 || !validRelative(options.PreviewFile) || filepath.ToSlash(filepath.Clean(options.PreviewFile)) != options.PreviewFile {
		return errors.New("file preview requires one package and a relative file path")
	}
	return nil
}

func (m *Manager) previewFileDiff(ctx context.Context, sandbox Sandbox, change packageChanges, name string) (*ActivationFileDiff, error) {
	output := &boundedBuffer{limit: activationDiffLimit}
	command := sandboxCachedIndexCommand(change.PackageID, change.Base, "--literal-pathspecs", "diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", change.Base, "--", name)
	if err := m.driver.ExecCommand(ctx, sandbox.SandboxID, []string{"/bin/sh", "-c", command}, nil, output); err != nil {
		return nil, err
	}
	return activationDiffOutput(output.RawString()), nil
}

func activationDiffOutput(output string) *ActivationFileDiff {
	result := &ActivationFileDiff{}
	// Only the hunks go to the editor; Git headers can contain host paths.
	if _, hunks, ok := strings.Cut(output, "\n@@"); ok {
		result.Text = "@@" + hunks
	} else if strings.Contains(output, "Binary files ") {
		result.Notice = "Binary file changed. A text diff is unavailable."
	} else {
		result.Notice = "No text changes. Only the file's presence or metadata changed."
	}
	if len(result.Text) > activationDiffLimit {
		result.Text = result.Text[:strings.LastIndex(result.Text[:activationDiffLimit], "\n")+1]
		result.Notice = "Diff truncated. Use the terminal to review the complete change."
	} else if strings.HasSuffix(result.Text, "\n[output truncated]") {
		result.Text = strings.TrimSuffix(result.Text, "\n[output truncated]")
		result.Notice = "Diff truncated. Use the terminal to review the complete change."
	}
	return result
}
