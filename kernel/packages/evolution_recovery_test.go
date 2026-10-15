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
	for _, scenario := range []string{"pre-hook", "post-hook", "missing-stage"} {
		t.Run(scenario, func(t *testing.T) {
			root, store, db := activationStore(t)
			active := writeActivationPackage(t, filepath.Join(root, "packages"), "acme/orders", true)
			candidate := writeActivationPackage(t, t.TempDir(), "acme/orders", true)
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
			if scenario == "post-hook" {
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
			if scenario == "post-hook" {
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
			if scenario == "missing-stage" {
				if err := os.Rename(candidate, candidate+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := owner.Recover(ctx); err == nil {
					t.Fatal("missing candidate was silently replaced")
				}
				if head := runTestGit(t, git, active, "rev-parse", "HEAD"); head != old {
					t.Fatal("missing candidate changed source")
				}
				if err := os.Rename(candidate+"-retained", candidate); err != nil {
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
			if runner.calls["acme/orders/pre-activate:acme/orders"] != 1 && scenario == "post-hook" {
				t.Fatal("successful pre-hook replayed")
			}
			assertActivationState(t, db, "complete")
		})
	}
}
