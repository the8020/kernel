package packages

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/deployment"
)

type committedSchema struct {
	candidates       []deployment.Candidate
	prepares, aborts int
}

func (s *committedSchema) Prepare(_ context.Context, _ string, candidates []deployment.Candidate) error {
	s.prepares++
	s.candidates = candidates
	return nil
}
func (s *committedSchema) Complete(_ context.Context, _ string, activated bool) error {
	if !activated {
		s.aborts++
		return errors.New("committed schema cannot roll back")
	}
	return nil
}
func (s *committedSchema) ForwardRequired(context.Context, string) (bool, error) {
	return s.prepares > 0, nil
}
func (s *committedSchema) RecoveryCandidates(context.Context, string) ([]deployment.Candidate, error) {
	return s.candidates, nil
}

func TestCommittedSchemaRecoveryKeepsCandidateAndDoesNotReplay(t *testing.T) {
	for _, scenario := range []string{"pre-hook", "post-hook", "post-hook-drift", "missing-stage", "pre-hook-drift", "missing-stage-git"} {
		t.Run(scenario, func(t *testing.T) {
			root, store, db := activationStore(t)
			active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", true)
			candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
			if scenario == "missing-stage-git" {
				stageRoot, err := os.MkdirTemp(filepath.Dir(active), ".orders-sync-")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(candidate, filepath.Join(stageRoot, "repository")); err != nil {
					t.Fatal(err)
				}
				candidate = filepath.Join(stageRoot, "repository")
			}
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			commit := func(path, message string) string {
				runTestGit(t, git, path, "init", "-q")
				runTestGit(t, git, path, "config", "user.name", "Fixture")
				runTestGit(t, git, path, "config", "user.email", "fixture@example.test")
				runTestGit(t, git, path, "add", ".")
				runTestGit(t, git, path, "commit", "-q", "-m", message)
				return runTestGit(t, git, path, "rev-parse", "HEAD")
			}
			old := commit(active, "old")
			writeFile(t, filepath.Join(candidate, "candidate.txt"), "new")
			next := commit(candidate, "new")
			putActivePackage(t, store, "acme/orders", old)
			events := []string{}
			failure := "acme/orders/pre-activate"
			if strings.HasPrefix(scenario, "post-hook") {
				failure = "acme/orders/post-activate"
			}
			runner := &activationJobRecorder{events: &events, failures: map[string]int{failure + ":acme/orders": 1}, calls: map[string]int{}}
			schema := &committedSchema{}
			indexes := 0
			config := ActivationCoordinatorConfig{Database: db, Packages: store, Schema: schema, Jobs: runner, Reindex: func(context.Context, []string) error {
				indexes++
				if indexes == 1 {
					return errors.New("index unavailable")
				}
				return nil
			}}
			owner, err := NewActivationCoordinator(config)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			const id = "act-forward001"
			err = owner.Prepare(ctx, id, []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: next}})
			if strings.HasPrefix(scenario, "post-hook") {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := replacePackageDirectory(active, candidate); err != nil {
					t.Fatal(err)
				}
				err = owner.Complete(ctx, id, true)
			}
			if err == nil {
				t.Fatal("hook failure disappeared")
			}
			if schema.aborts != 0 {
				t.Fatal("committed schema was rolled back")
			}
			if scenario == "post-hook-drift" {
				writeFile(t, filepath.Join(active, "candidate.txt"), "uncommitted change")
				if err := owner.Recover(ctx); err == nil {
					t.Fatal("recovery accepted a dirty candidate source")
				}
				if runner.calls["acme/orders/post-activate:acme/orders"] != 1 {
					t.Fatal("recovery retried a hook against a dirty candidate")
				}
				assertActivationState(t, db, "code_switched")
				runTestGit(t, git, active, "checkout", "--", "candidate.txt")
				writeFile(t, filepath.Join(active, "candidate.txt"), "different commit")
				runTestGit(t, git, active, "add", "candidate.txt")
				runTestGit(t, git, active, "commit", "-q", "-m", "different candidate")
				if err := owner.Recover(ctx); err == nil || !strings.Contains(err.Error(), "candidate acme/orders changed") {
					t.Fatalf("recovery accepted another candidate: %v", err)
				}
				if runner.calls["acme/orders/post-activate:acme/orders"] != 1 {
					t.Fatal("recovery retried a hook against another candidate")
				}
				runTestGit(t, git, active, "reset", "--hard", next)
			}
			if scenario == "pre-hook-drift" {
				writeFile(t, filepath.Join(candidate, "candidate.txt"), "uncommitted change")
				if err := owner.Recover(ctx); err == nil {
					t.Fatal("recovery accepted a dirty staged candidate")
				}
				if runner.calls["acme/orders/pre-activate:acme/orders"] != 1 {
					t.Fatal("recovery retried a hook against a dirty staged candidate")
				}
				runTestGit(t, git, candidate, "checkout", "--", "candidate.txt")
				writeFile(t, filepath.Join(candidate, "candidate.txt"), "different commit")
				runTestGit(t, git, candidate, "add", "candidate.txt")
				runTestGit(t, git, candidate, "commit", "-q", "-m", "different candidate")
				if err := owner.Recover(ctx); err == nil {
					t.Fatal("recovery accepted a different staged candidate")
				}
				if runner.calls["acme/orders/pre-activate:acme/orders"] != 1 {
					t.Fatal("recovery retried a hook against a different staged candidate")
				}
				runTestGit(t, git, candidate, "reset", "--hard", next)
			}
			if scenario == "missing-stage" {
				if err := os.Rename(candidate, candidate+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := owner.Recover(ctx); err == nil {
					t.Fatal("missing candidate was silently replaced")
				}
				if runner.calls["acme/orders/pre-activate:acme/orders"] != 1 {
					t.Fatal("recovery retried a hook without the staged candidate")
				}
				if head := runTestGit(t, git, active, "rev-parse", "HEAD"); head != old {
					t.Fatal("missing candidate changed source")
				}
				if err := os.Rename(candidate+"-retained", candidate); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "missing-stage-git" {
				if err := os.Rename(filepath.Join(candidate, ".git"), candidate+"-git"); err != nil {
					t.Fatal(err)
				}
				if err := owner.Recover(ctx); err == nil || !strings.Contains(err.Error(), "staged Git metadata is missing") {
					t.Fatalf("recovery accepted missing Git metadata: %v", err)
				}
				if runner.calls["acme/orders/pre-activate:acme/orders"] != 1 {
					t.Fatal("recovery retried a hook without candidate identity")
				}
				if err := os.Rename(candidate+"-git", filepath.Join(candidate, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			owner, err = NewActivationCoordinator(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Recover(ctx); err == nil || !strings.Contains(err.Error(), "index unavailable") {
				t.Fatalf("recovery did not reach publication: %v", err)
			}
			if err := owner.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err := owner.Complete(ctx, id, true); err != nil {
				t.Fatal(err)
			}
			if schema.prepares != 1 || schema.aborts != 0 || indexes != 2 {
				t.Fatalf("replayed schema/abort/index: %+v %d", schema, indexes)
			}
			if head := runTestGit(t, git, active, "rev-parse", "HEAD"); head != next {
				t.Fatal("wrong candidate published")
			}
			if runner.calls["acme/orders/pre-activate:acme/orders"] != 1 && strings.HasPrefix(scenario, "post-hook") {
				t.Fatal("successful pre-hook replayed")
			}
			assertActivationState(t, db, "complete")
		})
	}
}

func TestFirstActivationRecoveryAcceptsOnlyEmptyMountPlaceholder(t *testing.T) {
	root, store, db := activationStore(t)
	candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	runTestGit(t, git, candidate, "init", "-q")
	runTestGit(t, git, candidate, "add", ".")
	runTestGit(t, git, candidate, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-q", "-m", "candidate")
	commit := runTestGit(t, git, candidate, "rev-parse", "HEAD")
	events := []string{}
	runner := &activationJobRecorder{events: &events, failures: map[string]int{"acme/orders/pre-activate:acme/orders": 1}, calls: map[string]int{}}
	schema := &committedSchema{}
	config := ActivationCoordinatorConfig{Database: db, Packages: store, Schema: schema, Jobs: runner}
	owner, err := NewActivationCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Prepare(t.Context(), "act-firstempty", []deployment.Candidate{{PackageID: "acme/orders", Root: candidate, Commit: commit}}); err == nil {
		t.Fatal("pre-hook failure disappeared")
	}
	destination := filepath.Join(root, "packages/acme/orders")
	writeFile(t, filepath.Join(destination, "private-work"), "keep")
	owner, err = NewActivationCoordinator(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Recover(t.Context()); err == nil {
		t.Fatal("recovery discarded unexpected source")
	}
	if raw, err := os.ReadFile(filepath.Join(destination, "private-work")); err != nil || string(raw) != "keep" {
		t.Fatal("private work changed", err)
	}
	if err := os.Remove(filepath.Join(destination, "private-work")); err != nil {
		t.Fatal(err)
	}
	if err := owner.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if schema.prepares != 1 || schema.aborts != 0 {
		t.Fatalf("replayed committed schema: %+v", schema)
	}
	if runTestGit(t, git, destination, "rev-parse", "HEAD") != commit {
		t.Fatal("candidate was not installed")
	}
	assertActivationState(t, db, "complete")
}

func TestCommittedSchemaRecoveryFinishesPartialStagedSet(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	root, store, db := activationStore(t)
	var candidates []deployment.Candidate
	for _, id := range []string{"acme/a", "acme/b"} {
		active := writeActivationPackage(t, filepath.Join(root, "packages"), id, true)
		runTestGit(t, git, active, "init", "-q")
		runTestGit(t, git, active, "add", ".")
		runTestGit(t, git, active, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-q", "-m", "old")
		putActivePackage(t, store, id, runTestGit(t, git, active, "rev-parse", "HEAD"))
		stage := filepath.Join(t.TempDir(), "repository")
		runTestGit(t, git, "", "clone", "-q", active, stage)
		writeFile(t, filepath.Join(stage, "candidate.txt"), id)
		runTestGit(t, git, stage, "add", ".")
		runTestGit(t, git, stage, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-q", "-m", "candidate")
		candidates = append(candidates, deployment.Candidate{PackageID: id, Root: stage, Commit: runTestGit(t, git, stage, "rev-parse", "HEAD")})
	}
	events := []string{}
	runner := &activationJobRecorder{events: &events, failures: map[string]int{}, calls: map[string]int{}}
	schema := &committedSchema{}
	owner, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Prepare(t.Context(), "act-forwardset", candidates); err != nil {
		t.Fatal(err)
	}
	if _, err := replacePackageDirectory(store.packagePath(candidates[0].PackageID), candidates[0].Root); err != nil {
		t.Fatal(err)
	}
	// The previous checkout may contain the candidate commit without containing
	// the candidate files that schema and hooks must validate.
	runTestGit(t, git, store.packagePath(candidates[1].PackageID), "fetch", "-q", candidates[1].Root, "HEAD")
	if err := os.Rename(candidates[1].Root, candidates[1].Root+"-saved"); err != nil {
		t.Fatal(err)
	}
	if err := owner.Recover(t.Context()); err == nil {
		t.Fatal("recovery accepted a missing unswitched staged candidate")
	} else {
		t.Logf("missing candidate: %v", err)
	}
	if runner.calls["acme/a/post-activate:acme/a"] != 0 || runner.calls["acme/b/post-activate:acme/b"] != 0 {
		t.Fatalf("hooks ran without the candidate: %v", runner.calls)
	}
	if err := os.Rename(candidates[1].Root+"-saved", candidates[1].Root); err != nil {
		t.Fatal(err)
	}
	if err := owner.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if got := runTestGit(t, git, store.packagePath(candidate.PackageID), "rev-parse", "HEAD"); got != candidate.Commit {
			t.Fatalf("%s at %s, want %s", candidate.PackageID, got, candidate.Commit)
		}
		if runner.calls[candidate.PackageID+"/pre-activate:"+candidate.PackageID] != 1 {
			t.Fatalf("pre-hook replayed for %s: %v", candidate.PackageID, runner.calls)
		}
	}
	if schema.prepares != 1 || schema.aborts != 0 {
		t.Fatalf("committed schema replayed or aborted: %+v", schema)
	}
	assertActivationState(t, db, "complete")
}
