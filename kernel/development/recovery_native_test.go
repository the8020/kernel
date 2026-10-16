package development

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/deployment"
)

type recoveryInspectionHook struct {
	nativeActivationHook
	unavailable *bool
}

func (h recoveryInspectionHook) ForwardRequired(context.Context, string) (bool, error) {
	if *h.unavailable {
		return false, errors.New("fixture receipt lookup unavailable")
	}
	return false, nil
}

func TestNativeFailedRollbackRetainsActivation(t *testing.T) {
	for _, failurePhase := range []string{"preparation", "source_validation", "receipt_inspection"} {
		t.Run(failurePhase, func(t *testing.T) {
			m, sparse, sandbox, shared := nativeSparseRuntime(t, "recovery", 1)
			prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
			nativeExec(t, sparse.RunscDriver, sandbox, prefix+"printf 'captured edit\\n' >same.txt")
			preparations, blockRollback := 0, true
			hook := nativeActivationHook{
				prepare: func(ctx context.Context, _ string, candidates []deployment.Candidate) error {
					preparations++
					want := "captured edit\n"
					if preparations >= 2 {
						want = "later in-place edit\n"
					}
					if len(candidates) != 1 {
						return errors.New("expected one candidate")
					}
					body, err := os.ReadFile(filepath.Join(candidates[0].Root, "same.txt"))
					if err != nil || string(body) != want {
						return fmt.Errorf("candidate source = %q, expected %q: %v", body, want, err)
					}
					if preparations == 1 {
						if failurePhase != "source_validation" {
							return errors.New("fixture schema preparation failed")
						}
						if err := os.WriteFile(filepath.Join(shared, "untouched.txt"), []byte("shared update during validation\n"), 0600); err != nil {
							return err
						}
						_, err := gitCommand(ctx, shared, gitIdentity("Fixture", "fixture@example.test"), "commit", "-am", "Shared changed during validation")
						return err
					}
					return nil
				},
				complete: func(_ context.Context, _ string, activated bool) error {
					if !activated && blockRollback {
						return errors.New("fixture catalogue mismatch")
					}
					return nil
				},
			}
			if failurePhase == "receipt_inspection" {
				m.SetSchemaDeployment(recoveryInspectionHook{hook, &blockRollback})
			} else {
				m.SetSchemaDeployment(hook)
			}
			activate := func(exit int) ActivationResult {
				t.Helper()
				output := nativeExec(t, sparse.RunscDriver, sandbox, fmt.Sprintf("activate --json --message 'Recovery regression'; status=$?; test \"$status\" -eq %d", exit))
				var result ActivationResult
				if err := json.Unmarshal([]byte(output), &result); err != nil {
					t.Fatalf("helper JSON %q: %v", output, err)
				}
				return result
			}
			failed := activate(3)
			if failed.Success || failed.DeploymentID == "" || failed.Phase != "preparing" || !strings.Contains(failed.Error, failed.DeploymentID) {
				t.Fatalf("unconfirmed rollback discarded its identity/context: %+v", failed)
			}
			if failurePhase == "receipt_inspection" && (failed.Status != "failed" || !strings.Contains(failed.Error, "receipt lookup unavailable")) {
				t.Fatalf("unavailable receipt inspection claimed committed schema: %+v", failed)
			}
			journal := filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")
			var pending activationAttempt
			if err := readJSON(journal, &pending); err != nil || pending.TransactionID != failed.DeploymentID {
				t.Fatalf("lost pending journal: %+v, %v", pending, err)
			}
			nativeExec(t, sparse.RunscDriver, sandbox, prefix+`inode=$(stat -c %i same.txt); printf 'later in-place edit\n' >same.txt; test "$inode" = "$(stat -c %i same.txt)"`)
			blocked := activate(3)
			wantContext := "before loading new workspace changes"
			if failurePhase == "receipt_inspection" {
				wantContext = "before recovery"
			}
			if !blocked.Resumed || blocked.DeploymentID != failed.DeploymentID || blocked.Phase != "preparing" || !strings.Contains(blocked.Error, wantContext) || preparations != 1 {
				t.Fatalf("blocked recovery read a new candidate or hid its phase: %+v; preparations=%d", blocked, preparations)
			}
			blockRollback = false
			recovered := activate(0)
			if !recovered.Success || recovered.Resumed || recovered.DeploymentID == failed.DeploymentID || preparations != 2 {
				t.Fatalf("confirmed rollback did not acquire fresh source: %+v; preparations=%d", recovered, preparations)
			}
			if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != "later in-place edit\n" {
				t.Fatalf("new activation lost corrected bytes: %q, %v", body, err)
			}
			next := activate(0)
			if !next.Success || next.Resumed || next.Status != "not-committed" || preparations != 2 {
				t.Fatalf("published edits remained pending: %+v; preparations=%d", next, preparations)
			}
			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("successful activation retained its pending journal: %v", err)
			}
			if inspected, err := m.Inspect(sandbox.UserID); err != nil || inspected.SandboxID != sandbox.SandboxID {
				t.Fatalf("activation replaced its running sandbox: %+v, %v", inspected, err)
			}
		})
	}
}

func TestNativeFreshActivationAfterRollback(t *testing.T) {
	for _, edit := range []string{"in_place", "atomic_replace"} {
		t.Run(edit, func(t *testing.T) {
			m, sparse, sandbox, shared := nativeSparseRuntime(t, "freshcheck", 1)
			ctx := context.Background()
			prefix := "set -e; cd /workspace/packages/the8020/dev-core; "
			nativeExec(t, sparse.RunscDriver, sandbox, prefix+"printf 'rejected bytes\\n' >same.txt; printf 'other file current\\n' >untouched.txt")
			preparations, rollbacks := 0, 0
			m.SetSchemaDeployment(nativeActivationHook{
				prepare: func(_ context.Context, _ string, candidates []deployment.Candidate) error {
					preparations++
					if preparations == 1 {
						return errors.New("fixture initial schema rejection")
					}
					if len(candidates) != 1 {
						return fmt.Errorf("expected one candidate")
					}
					staged, err := os.ReadFile(filepath.Join(candidates[0].Root, "same.txt"))
					if err != nil {
						return err
					}
					blob, err := gitCommand(ctx, shared, nil, "show", candidates[0].Commit+":same.txt")
					if err != nil {
						return err
					}
					other, err := os.ReadFile(filepath.Join(candidates[0].Root, "untouched.txt"))
					if err != nil {
						return err
					}
					var pending activationAttempt
					if err := readJSON(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json"), &pending); err != nil {
						return err
					}
					captured := ""
					for _, capture := range pending.Packages[0].Captures {
						if capture.Path == "same.txt" {
							body, err := os.ReadFile(filepath.Join(sparse.storage, "snapshots", capture.ID, "file"))
							if err != nil {
								return err
							}
							captured = string(body)
						}
					}
					t.Logf("current sandbox=%q capture=%q candidate Git=%q staged evaluator input=%q other file=%q", "corrected bytes\n", captured, blob, staged, other)
					if string(staged) != "corrected bytes\n" || blob != "corrected bytes\n" || captured != "corrected bytes\n" {
						t.Errorf("NEW activation after confirmed rollback reuses rejected source")
					}
					if string(other) != "corrected companion\n" {
						t.Errorf("other file changed: %q", other)
					}
					return nil
				},
				complete: func(_ context.Context, _ string, activated bool) error {
					if !activated {
						rollbacks++
					}
					return nil
				},
			})
			failed, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "First rejected activation"})
			if err == nil || failed.Success || rollbacks != 1 {
				t.Fatalf("expected one completed rollback: %+v, %v, rollbacks=%d", failed, err, rollbacks)
			}
			if _, err := os.Stat(filepath.Join(m.sandboxRoot(sandbox), "activation/active.json")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("confirmed rollback retained the active journal: %v", err)
			}
			command := `inode=$(stat -c %i same.txt); printf 'corrected bytes\n' >same.txt; test "$inode" = "$(stat -c %i same.txt)"; cat same.txt`
			if edit == "atomic_replace" {
				command = `inode=$(stat -c %i same.txt); printf 'corrected bytes\n' >.replacement; mv .replacement same.txt; test "$inode" != "$(stat -c %i same.txt)"; cat same.txt`
			}
			if got := nativeExec(t, sparse.RunscDriver, sandbox, prefix+"printf 'corrected companion\\n' >untouched.txt; "+command); got != "corrected bytes\n" {
				t.Fatalf("current sandbox source=%q", got)
			}
			result, err := m.Activate(ctx, sandbox.UserID, ActivationOptions{Description: "New activation of corrected source"})
			if err != nil || !result.Success || result.Resumed || preparations != 2 {
				t.Fatalf("new activation: %+v, %v, preparations=%d", result, err, preparations)
			}
			published, err := os.ReadFile(filepath.Join(shared, "same.txt"))
			t.Logf("published=%q success=%t resumed=%t", published, result.Success, result.Resumed)
			if err != nil || string(published) != "corrected bytes\n" {
				t.Errorf("new activation published stale source: %q, %v", published, err)
			}
		})
	}
}
