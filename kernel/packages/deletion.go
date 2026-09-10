package packages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"the8020/kernel/deployment"
)

// DeletePackage publishes a removal through the same schema and catalog
// transaction as other source changes. Physical database data is retained.
func (s *Store) DeletePackage(ctx context.Context, packageID string) error {
	unlock, err := s.lockPackage(ctx, packageID)
	if err != nil {
		return err
	}
	defer unlock()
	if _, exists, err := s.index.Get(ctx, packageID); err != nil {
		return err
	} else if !exists {
		return os.ErrNotExist
	}
	path, exists, err := s.packageDestination(packageID)
	if err != nil {
		return err
	}
	if exists {
		if _, err := s.cleanRepositoryHead(ctx, path); err != nil {
			return err
		}
	}
	hook := s.schemaDeployment()
	transactionID, err := activationID()
	if err != nil {
		return err
	}
	if hook == nil {
		return errors.New("package activation is unavailable")
	}
	if _, err := os.Lstat(path + ".previous"); err == nil {
		return errors.New("previous package activation must be recovered first")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := hook.Prepare(ctx, transactionID, []deployment.Candidate{{PackageID: packageID, Root: path}}); err != nil {
		return err
	}
	if exists {
		if err := os.Rename(path, path+".previous"); err != nil {
			return errors.Join(err, hook.Complete(context.WithoutCancel(ctx), transactionID, false))
		}
		if err := syncPackageDirectory(filepath.Dir(path)); err != nil {
			if restoreErr := os.Rename(path+".previous", path); restoreErr != nil {
				return errors.Join(err, restoreErr, hook.Complete(context.WithoutCancel(ctx), transactionID, true))
			}
			return errors.Join(err, hook.Complete(context.WithoutCancel(ctx), transactionID, false))
		}
	}
	completionErr := hook.Complete(ctx, transactionID, true)
	if completionErr != nil {
		if _, indexed, err := s.index.Get(context.WithoutCancel(ctx), packageID); err != nil || indexed {
			return fmt.Errorf("complete package removal: %w", errors.Join(completionErr, err))
		}
	}
	if !exists {
		return completionErr
	}
	return errors.Join(completionErr, finalizePackageDirectory(path))
}
