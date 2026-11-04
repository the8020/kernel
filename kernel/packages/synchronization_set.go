package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"the8020/kernel/database"
	"the8020/kernel/deployment"
	idgen "the8020/kernel/identity"
)

const synchronizationsTable = `"the8020__packages__synchronizations"`

// SynchronizationSet is one reviewed operation. Before is the observed installed
// commit, or empty for an absent source. It is checked while holding source locks.
type SynchronizationSet struct {
	ActivationID string                     `json:"activation_id"`
	Packages     []SynchronizationSelection `json:"packages"`
}

type SynchronizationSelection struct {
	PackageID string `json:"package_id"`
	Source    string `json:"source"`
	Commit    string `json:"commit"`
	Tag       string `json:"tag"`
	Before    string `json:"before"`
}

type sourceConfiguration struct {
	Source string `json:"source"`
	Commit string `json:"commit"`
	Tag    string `json:"tag"`
	Secret string `json:"secret"`
	Local  bool   `json:"local"`
}

type synchronizationIntent struct {
	PackageID string              `json:"package_id"`
	Desired   sourceConfiguration `json:"desired"`
	Previous  sourceConfiguration `json:"previous"`
}

type synchronizationKey struct{}

func configuration(entry PackageIndex) sourceConfiguration {
	return sourceConfiguration{entry.Source, entry.Commit, entry.Tag, entry.Secret, entry.Local}
}

// SynchronizationSetAvailable also checks the application-owned additive schema;
// old installations can still use ordinary synchronization to install that schema.
func (s *Store) SynchronizationSetAvailable(ctx context.Context) error {
	c, ok := s.schemaDeployment().(*ActivationCoordinator)
	if !ok {
		return errors.New("staged-set synchronization requires an available activation coordinator")
	}
	exists, err := database.RelationExists(ctx, c.database, "the8020__packages__synchronizations")
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("staged-set synchronization requires the updated packages schema")
	}
	return nil
}

// SynchronizeSet stages every exact selection before a single native activation.
// Credentials are invocation-scoped and never enter the durable intent.
func (s *Store) SynchronizeSet(ctx context.Context, input, token, username string) (results []PackageSynchronization, err error) {
	if len(input) > 256*1024 {
		return nil, errors.New("staged-set input exceeds 256 KiB")
	}
	var request SynchronizationSet
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("invalid staged-set input: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("staged-set input must contain one JSON object")
	}
	if !idgen.Is(request.ActivationID, "act") || len(request.Packages) == 0 || len(request.Packages) > 256 {
		return nil, errors.New("staged-set synchronization requires a fresh act- ID and 1..256 packages")
	}
	options := deployment.Options(ctx)
	if options.Preview || ((len(options.ForceColumns) > 0 || options.ApprovedPlan != "") && len(request.Packages) != 1) {
		return nil, errors.New("staged-set force requires one explicit package; use ordinary synchronization for preview")
	}
	if err := s.SynchronizationSetAvailable(ctx); err != nil {
		return nil, err
	}
	c := s.schemaDeployment().(*ActivationCoordinator)
	ids, entries := make([]string, len(request.Packages)), make([]PackageIndex, len(request.Packages))
	seen := map[string]bool{}
	for i, selection := range request.Packages {
		identity, err := ParsePackageID(selection.PackageID)
		if err != nil || seen[selection.PackageID] {
			return nil, fmt.Errorf("invalid or duplicate selected package %q", selection.PackageID)
		}
		entry := PackageIndex{Author: identity.Namespace, Repository: identity.Repository, Source: selection.Source, Commit: selection.Commit, Tag: selection.Tag}
		if err := validatePackageIndex(&entry); err != nil {
			return nil, err
		}
		if entry.PackageID != selection.PackageID || (entry.Commit == "" && entry.Tag == "") ||
			(entry.Commit != "" && !fullCommit(entry.Commit)) || (selection.Before != "" && !fullCommit(selection.Before)) {
			return nil, fmt.Errorf("%s requires an exact full commit or explicit tag and a full baseline commit", selection.PackageID)
		}
		ids[i], entries[i], seen[selection.PackageID] = selection.PackageID, entry, true
	}
	unlock, err := LockSources(ctx, s.packagesRoot, ids)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ctx, releaseActivation, err := c.database.AcquireActivationLock(ctx, request.ActivationID)
	if err != nil {
		return nil, err
	}
	defer releaseActivation()
	var existing int
	err = c.database.QueryRowContext(ctx, `SELECT 1 FROM `+activationsTable+` WHERE "activationId" = $1`, request.ActivationID).Scan(&existing)
	if err == nil {
		return nil, errors.New("activation identity already exists; inspect/resume it instead of replaying synchronization")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	intents := make([]synchronizationIntent, len(entries))
	for i, entry := range entries {
		// Include retired records: retirement does not authorize replacing a custom source.
		previous, found, err := c.synchronizationConfiguration(ctx, entry.PackageID)
		if err != nil {
			return nil, err
		}
		if found && (previous.Local || previous.Secret != "" || (previous.Source != "" && previous.Source != entry.Source)) {
			return nil, fmt.Errorf("%s has a local or custom source configuration; review it before deployment", entry.PackageID)
		}
		current, exists, err := s.index.Get(ctx, entry.PackageID)
		if err != nil {
			return nil, err
		}
		path, installed, err := s.packageDestination(entry.PackageID)
		if err != nil {
			return nil, err
		}
		actual := ""
		if installed {
			files, readErr := os.ReadDir(path)
			if readErr != nil {
				return nil, readErr
			}
			if len(files) > 0 {
				actual, err = s.cleanRepositoryHead(ctx, path)
				if err != nil {
					return nil, err
				}
			}
		}
		if actual != request.Packages[i].Before || (actual != "" && (!exists || current.State != "ready" || current.ActiveCommit != actual)) || (actual == "" && current.ActiveCommit != "") {
			return nil, fmt.Errorf("%s changed from the reviewed baseline or has an unindexed/unready source", entry.PackageID)
		}
		intents[i] = synchronizationIntent{entry.PackageID, configuration(entry), previous}
	}
	encoded, err := json.Marshal(intents)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 512*1024 {
		return nil, errors.New("synchronization intent exceeds 512 KiB")
	}
	stages := make([]stagedSynchronization, 0, len(entries))
	candidates := make([]deployment.Candidate, 0, len(entries))
	retain, prepared := false, false
	defer func() {
		if err != nil && prepared && !retain {
			cleanup := context.WithoutCancel(ctx)
			var stage string
			checkErr := c.database.QueryRowContext(cleanup, `SELECT "stage" FROM `+activationsTable+` WHERE "activationId" = $1`, request.ActivationID).Scan(&stage)
			if checkErr == nil {
				err = errors.Join(err, c.Complete(cleanup, request.ActivationID, false))
				forward, checkErr := c.ForwardRequired(cleanup, request.ActivationID)
				retain = forward || checkErr != nil
			} else if !errors.Is(checkErr, sql.ErrNoRows) {
				retain = true
				err = errors.Join(err, checkErr)
			}
		}
		if !retain {
			for _, stage := range stages {
				err = errors.Join(err, os.RemoveAll(stage.stageRoot))
			}
		}
		if err != nil {
			err = fmt.Errorf("activation %s: %s", request.ActivationID, redactGitCredential(err.Error(), token, username))
		}
	}()
	for _, entry := range entries {
		stage, err := s.stagePackageIndex(ctx, entry, token, username, nil)
		if err != nil {
			return results, fmt.Errorf("stage %s: %w", entry.PackageID, err)
		}
		stages = append(stages, stage)
		results = append(results, stage.PackageSynchronization)
		candidates = append(candidates, deployment.Candidate{PackageID: entry.PackageID, Root: stage.root, Commit: stage.Commit})
	}
	ctx = context.WithValue(ctx, synchronizationKey{}, intents)
	prepared = true
	if err := c.Prepare(ctx, request.ActivationID, candidates); err != nil {
		return results, err
	}
	for _, stage := range stages {
		switched, switchErr := replacePackageDirectory(stage.destination, stage.root)
		retain = retain || switched
		if switchErr != nil {
			return results, switchErr
		}
	}
	if err := c.Complete(ctx, request.ActivationID, true); err != nil {
		return results, err
	}
	retain = false
	for i := range results {
		results[i].Success, results[i].Changed, results[i].Cloned = true, true, !stages[i].exists
	}
	return results, nil
}

func fullCommit(value string) bool {
	return (len(value) == 40 || len(value) == 64) && strings.Trim(value, "0123456789abcdef") == ""
}

func (c *ActivationCoordinator) synchronizationConfiguration(ctx context.Context, id string) (sourceConfiguration, bool, error) {
	var value sourceConfiguration
	err := c.database.QueryRowContext(ctx, `SELECT COALESCE("source", ''), COALESCE("requestedCommit", ''), COALESCE("requestedTag", ''), COALESCE("secretName", ''), "local" FROM `+packagesTable+` WHERE "packageId" = $1`, id).
		Scan(&value.Source, &value.Commit, &value.Tag, &value.Secret, &value.Local)
	if errors.Is(err, sql.ErrNoRows) {
		return value, false, nil
	}
	return value, err == nil, err
}

func (c *ActivationCoordinator) synchronizationIntent(ctx context.Context, id string) ([]synchronizationIntent, error) {
	exists, err := database.RelationExists(ctx, c.database, "the8020__packages__synchronizations")
	if err != nil || !exists {
		return nil, err
	}
	var encoded []byte
	err = c.database.QueryRowContext(ctx, `SELECT "packages" FROM `+synchronizationsTable+` WHERE "activationId" = $1`, id).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []synchronizationIntent
	if len(encoded) > 512*1024 {
		return nil, errors.New("synchronization intent exceeds 512 KiB")
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	if len(result) == 0 || len(result) > 256 {
		return nil, errors.New("invalid synchronization intent membership")
	}
	return result, nil
}

func (c *ActivationCoordinator) verifySynchronization(ctx context.Context, id string) error {
	intents, err := c.synchronizationIntent(ctx, id)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		current, found, err := c.synchronizationConfiguration(ctx, intent.PackageID)
		if err != nil {
			return err
		}
		if !found || current != intent.Previous {
			return fmt.Errorf("%s source configuration changed during activation %s; restore the reviewed configuration before recovery", intent.PackageID, id)
		}
	}
	return nil
}

// ResumeSynchronization cannot take over development's separate source journal.
func (s *Store) ResumeSynchronization(ctx context.Context, id string) (ActivationInspection, error) {
	if !idgen.Is(id, "act") {
		return ActivationInspection{}, errors.New("invalid activation identity")
	}
	if err := s.SynchronizationSetAvailable(ctx); err != nil {
		return ActivationInspection{}, err
	}
	c := s.schemaDeployment().(*ActivationCoordinator)
	intents, err := c.synchronizationIntent(ctx, id)
	if err != nil {
		return ActivationInspection{}, err
	}
	if len(intents) == 0 {
		return ActivationInspection{}, errors.New("this activation is not owned by staged-set synchronization; use its original owner recovery")
	}
	if err := c.recoverActivation(ctx, id); err != nil {
		return ActivationInspection{}, err
	}
	return c.InspectActivation(ctx, id)
}
