package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"the8020/kernel/database"
	"the8020/kernel/deployment"
)

func writeNodeCommit(ctx context.Context, tx *sql.Tx, node, id, commit string) error {
	if commit == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM `+nodePackagesTable+` WHERE "nodeId"=$1 AND "packageId"=$2`, node, id)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+nodePackagesTable+` ("nodeId","packageId","activeCommit") VALUES ($1,$2,$3)
		ON CONFLICT ("nodeId","packageId") DO UPDATE SET "activeCommit"=excluded."activeCommit", "ready"=true`, node, id, commit)
	return err
}

func bumpPackageRevision(ctx context.Context, tx *sql.Tx, domain string, now any) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO "the8020__system__revisions" ("domain","revision","updatedAt") VALUES ($1,1,$2)
	ON CONFLICT ("domain") DO UPDATE SET "revision"="the8020__system__revisions"."revision"+1,"updatedAt"=excluded."updatedAt"`, domain, now)
	return err
}

// Register clean local sources against completed system activations. A new container
// can carry earlier accepted commits during a rolling kernel upgrade.
func (s *Store) registerNode(ctx context.Context, commits map[string]string) error {
	db := s.index.(*DatabasePackageIndexStore).database
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var unsettled error
	known, err := tx.QueryContext(ctx, `SELECT "packageId" FROM `+nodePackagesTable+` WHERE "nodeId"=$1`, s.nodeID)
	if err != nil {
		return err
	}
	var missing []string
	for known.Next() {
		var id string
		if err := known.Scan(&id); err != nil {
			known.Close()
			return err
		}
		if commits[id] == "" {
			missing = append(missing, id)
		}
	}
	if err := errors.Join(known.Err(), known.Close()); err != nil {
		return err
	}
	for _, id := range missing {
		if err := writeNodeCommit(ctx, tx, s.nodeID, id, ""); err != nil {
			return err
		}
	}
	for id, commit := range commits {
		var active, local string
		var ready bool
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(p."activeCommit",''), COALESCE(n."activeCommit",''), COALESCE(n."ready",false) FROM `+packagesTable+` p LEFT JOIN `+nodePackagesTable+` n ON n."packageId"=p."packageId" AND n."nodeId"=$2 WHERE p."packageId"=$1`, id, s.nodeID).Scan(&active, &local, &ready)
		if errors.Is(err, sql.ErrNoRows) {
			unsettled = fmt.Errorf("unindexed package %s on node %s", id, s.nodeID)
			continue
		}
		if err != nil {
			return err
		}
		if local == commit && ready {
			continue
		}
		accepted := active == commit
		if !accepted {
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+activationPackagesTable+` p JOIN `+activationsTable+` a ON a."activationId"=p."activationId" WHERE p."packageId"=$1 AND p."candidateCommit"=$2 AND a."stage" IN ('published','complete'))`, id, commit).Scan(&accepted)
			if err != nil {
				return err
			}
		}
		if err := writeNodeCommit(ctx, tx, s.nodeID, id, commit); err != nil {
			return err
		}
		if !accepted {
			if _, err := tx.ExecContext(ctx, `UPDATE `+nodePackagesTable+` SET "ready"=false WHERE "nodeId"=$1 AND "packageId"=$2`, s.nodeID, id); err != nil {
				return err
			}
			unsettled = fmt.Errorf("package %s at %s needs node sync before serving", id, commit)
		}
	}
	return errors.Join(tx.Commit(), unsettled)
}

// Catch up to an already published schema/package version without replaying
// system hooks. The database lock only spans the recheck and local rename.
func (s *Store) installPublished(ctx context.Context, stage stagedSynchronization) (bool, error) {
	c, ok := s.schemaDeployment().(*ActivationCoordinator)
	if !ok || s.nodeID == "" {
		return false, nil
	}
	ctx, release, err := c.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if selected, ok := ctx.Value(nodeSelectionKey{}).(NodeSelection); ok {
		if err := s.checkSelection(ctx, selected); err != nil {
			return false, err
		}
	}
	entry, found, err := s.index.Get(ctx, stage.PackageID)
	if err != nil {
		return false, err
	}
	selected, _ := ctx.Value(nodeSelectionKey{}).(NodeSelection)
	if !selected.Restore && (!found || entry.State != "ready" || entry.ActiveCommit != stage.Commit) {
		return false, nil
	}
	var pending bool
	err = c.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+activationsTable+` a JOIN `+activationPackagesTable+` p ON p."activationId"=a."activationId" WHERE a."stage" IN `+unfinishedActivationStages+` AND p."packageId"=$1)`, stage.PackageID).Scan(&pending)
	if err != nil {
		return false, err
	}
	if pending {
		return false, fmt.Errorf("%s has an unfinished activation", stage.PackageID)
	}
	tx, err := c.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = writeNodeCommit(ctx, tx, s.nodeID, stage.PackageID, stage.Commit); err != nil {
		return false, err
	}
	if err = bumpPackageRevision(ctx, tx, "packages:"+s.nodeID, database.EncodeTime(c.database, time.Now())); err != nil {
		return false, err
	}
	if stage.exists {
		err = unix.Renameat2(unix.AT_FDCWD, stage.root, unix.AT_FDCWD, stage.destination, unix.RENAME_EXCHANGE)
	} else {
		err = os.Rename(stage.root, stage.destination)
	}
	if err != nil {
		return false, err
	}
	if err = syncPackageDirectory(filepath.Dir(stage.destination)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) validateNodeInstalled(ctx context.Context, commits map[string]string) error {
	if err := s.registerNode(ctx, commits); err != nil {
		return err
	}
	if err := s.pinInstalledTags(ctx, commits); err != nil {
		return err
	}
	_, published, err := s.runtimeIndex.Published(ctx)
	if err != nil {
		return err
	}
	for id, wanted := range published {
		if commits[id] != wanted {
			return fmt.Errorf("package %s checkout %q does not match this node's commit %s", id, commits[id], wanted)
		}
	}
	if len(published) != len(commits) {
		return errors.New("node package inventory changed during validation")
	}
	return nil
}

// Resolve legacy tags only when the installed Git tree confirms the active commit.
func (s *Store) pinInstalledTags(ctx context.Context, commits map[string]string) error {
	db := s.index.(*DatabasePackageIndexStore).database
	rows, err := db.QueryContext(ctx, `SELECT "packageId", "requestedTag", "activeCommit", "revision"
		FROM `+packagesTable+` WHERE "state"='ready' AND "requestedTag" IS NOT NULL AND "activeCommit" IS NOT NULL`)
	if err != nil {
		return err
	}
	type legacyTag struct {
		id, tag, commit string
		revision        int64
	}
	var candidates []legacyTag
	for rows.Next() {
		var entry legacyTag
		if err := rows.Scan(&entry.id, &entry.tag, &entry.commit, &entry.revision); err != nil {
			rows.Close()
			return err
		}
		if commits[entry.id] == entry.commit && safeGitTag(entry.tag) {
			candidates = append(candidates, entry)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, entry := range candidates {
		commit, err := s.gitValue(ctx, s.packagePath(entry.id), "rev-parse", "--verify", "refs/tags/"+entry.tag+"^{commit}")
		if err != nil || commit != entry.commit {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE `+packagesTable+` SET "requestedCommit"=$1, "requestedTag"=NULL,
			"revision"="revision"+1, "updatedAt"=$2 WHERE "packageId"=$3 AND "revision"=$4`,
			entry.commit, database.EncodeTime(db, time.Now()), entry.id, entry.revision); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Store) NodeInstallations() bool { return s.nodeID != "" }

type NodeSelection = deployment.NodeSelection

func validateNodeSelection(r NodeSelection) error {
	if _, err := ParsePackageID(r.PackageID); err != nil {
		return err
	}
	if (r.Commit != "" && !fullCommit(r.Commit)) || (r.Before != "" && !fullCommit(r.Before)) || (r.Before == "" && r.Commit == "") {
		return errors.New("node synchronization requires full commits")
	}
	if r.Commit == "" {
		return nil
	}
	_, _, _, err := normalizePackageSource(r.Source)
	return err
}

type nodeSelectionKey struct{}

func (s *Store) SyncNodePackage(ctx context.Context, r NodeSelection) error {
	if !s.NodeInstallations() {
		return errors.New("install the node package schema and restart this node before node synchronization")
	}
	if r.CheckOnly {
		if _, err := ParsePackageID(r.PackageID); err != nil {
			return err
		}
		release, err := s.lockPackage(ctx, r.PackageID)
		if err == nil {
			release()
		}
		return err
	}
	if err := validateNodeSelection(r); err != nil {
		return err
	}
	if r.Commit == "" {
		return s.removeRetired(ctx, r)
	}
	ctx, err := WithGitTransport(ctx, GitTransport{r.Source, r.Username, r.Secret})
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, nodeSelectionKey{}, r)
	_, err = s.synchronizePackage(ctx, r.PackageID, "", "", nil)
	return err
}

func (s *Store) checkSelection(ctx context.Context, r NodeSelection) error {
	if r.Restore {
		var accepted bool
		db := s.index.(*DatabasePackageIndexStore).database
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+activationPackagesTable+` p JOIN `+activationsTable+` a ON a."activationId"=p."activationId" WHERE p."packageId"=$1 AND p."candidateCommit"=$2 AND a."stage" IN ('published','complete'))`, r.PackageID, r.Commit).Scan(&accepted); err != nil {
			return err
		}
		if !accepted {
			return fmt.Errorf("%s has no published version %s", r.PackageID, r.Commit)
		}
		return nil
	}

	entry, found, err := s.index.Get(ctx, r.PackageID)
	if err != nil {
		return err
	}
	want := entry.Commit
	if want == "" {
		want = entry.ActiveCommit
	}
	if !found || want != r.Commit {
		return fmt.Errorf("desired version changed for %s; refresh the selection", r.PackageID)
	}
	return nil
}

// A peer already retired the shared package. Remove only this reviewed clean checkout.
func (s *Store) removeRetired(ctx context.Context, r NodeSelection) error {
	unlock, err := s.lockPackage(ctx, r.PackageID)
	if err != nil {
		return err
	}
	defer unlock()
	c := s.schemaDeployment().(*ActivationCoordinator)
	ctx, release, err := c.database.AcquireDeploymentLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	var state string
	if err = c.database.QueryRowContext(ctx, `SELECT "state" FROM `+packagesTable+` WHERE "packageId"=$1`, r.PackageID).Scan(&state); err != nil {
		return err
	}
	if state != "retired" && !r.Restore {
		return fmt.Errorf("%s is no longer retired", r.PackageID)
	}
	path, exists, err := s.packageDestination(r.PackageID)
	if err != nil {
		return err
	}
	if exists {
		head, err := s.cleanRepositoryHead(ctx, path)
		if err != nil {
			return err
		}
		if head != r.Before {
			return fmt.Errorf("%s checkout changed since review", r.PackageID)
		}
		// Rename before deleting, so interruption never leaves a partial live checkout.
		tmp, err := os.MkdirTemp(filepath.Dir(path), ".retired-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		if err = os.Rename(path, filepath.Join(tmp, "repository")); err != nil {
			return err
		}
		if err = syncPackageDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	tx, err := c.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = writeNodeCommit(ctx, tx, s.nodeID, r.PackageID, ""); err != nil {
		return err
	}
	if err = bumpPackageRevision(ctx, tx, "packages:"+s.nodeID, database.EncodeTime(c.database, time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}
