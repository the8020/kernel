package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type checkoutRecoveryKey struct{}

func WithCheckoutRecovery(ctx context.Context) context.Context {
	if _, routed := ctx.Value(gitTransportKey{}).(GitTransport); routed {
		return ctx
	}
	return context.WithValue(ctx, checkoutRecoveryKey{}, true)
}

func validateRecoveryGit(root string) error {
	git := filepath.Join(root, ".git")
	return filepath.WalkDir(git, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || path == git && !entry.IsDir() || path == filepath.Join(git, "objects", "info", "alternates") || path == filepath.Join(git, "commondir") || path == filepath.Join(git, "worktrees") {
			return fmt.Errorf("checkout recovery requires self-contained Git metadata; inspect %s", path)
		}
		return nil
	})
}

func (s *Store) saveCheckout(source string, id Identity) (string, error) {
	root := filepath.Join(s.workspaceRoot, "checkout-recovery")
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create private checkout recovery directory: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("checkout recovery requires a private directory at %s", root)
	}
	dir, err := os.MkdirTemp(root, id.Namespace+"-"+id.Repository+"-")
	if err != nil {
		return "", fmt.Errorf("create recovery copy: %w", err)
	}
	dst := filepath.Join(dir, "repository")
	pending := filepath.Join(dir, "saving")
	if err := copyRepository(source, pending); err != nil {
		return "", fmt.Errorf("save checkout before sync: %w", errors.Join(err, os.RemoveAll(dir)))
	}
	if err := os.Rename(pending, dst); err != nil {
		return "", fmt.Errorf("finish recovery copy: %w", errors.Join(err, os.RemoveAll(dir)))
	}
	return dst, nil
}
