package packages

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type outcomeFailure struct{ recordingSchemaHook }

func (h *outcomeFailure) Complete(ctx context.Context, id string, applied bool) error {
	_ = h.recordingSchemaHook.Complete(ctx, id, applied)
	return errors.New("cleanup failed")
}

func (h *outcomeFailure) SchemaOutcome(context.Context, string) (json.RawMessage, error) {
	return nil, errors.New("outcome read failed")
}

func TestOutcomeFailureCompletesPreparedActivation(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	runTestGit(t, git, remote, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(remote, "package.toml"), "schema = 1\ndescription = \"Recovery analysis fixture\"\n")
	runTestGit(t, git, remote, "add", ".")
	runTestGit(t, git, remote, "commit", "-qm", "analysis fixture")
	runTestGit(t, git, remote, "update-server-info")
	server := httptest.NewServer(http.StripPrefix("/repro/analysis.git", http.FileServer(http.Dir(filepath.Join(remote, ".git")))))
	defer server.Close()
	store := newTestStore(t, t.TempDir())
	hook := &outcomeFailure{}
	store.SetSchemaDeployment(hook)
	if _, err := store.SetPackageIndex(context.Background(), PackageIndex{Author: "repro", Repository: "analysis", Source: server.URL + "/repro/analysis.git"}); err != nil {
		t.Fatal(err)
	}
	_, err = store.synchronizePackage(context.Background(), "repro/analysis", "", "", nil)
	if err == nil || !strings.Contains(err.Error(), "outcome read failed") {
		t.Fatalf("wrong failure: %v", err)
	}
	if !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cleanup failure was lost: %v", err)
	}
	if len(hook.prepared) != 1 {
		t.Fatalf("Prepare did not succeed: %#v", hook)
	}
	if len(hook.completed) != 1 || hook.completed[0] {
		t.Fatalf("successful Prepare was not rolled back: %#v", hook.completed)
	}
}
