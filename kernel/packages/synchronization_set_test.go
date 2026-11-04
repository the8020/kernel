package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/deployment"
	"the8020/kernel/execution/jobs"
)

func synchronizationRemotes(t *testing.T) (string, map[string]string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root, commits := t.TempDir(), map[string]string{}
	for _, name := range []string{"alpha", "beta", "missingref", "invalid"} {
		repo := writeActivationPackage(t, t.TempDir(), "acme/"+name, true)
		writeFile(t, filepath.Join(repo, "package.toml"), "schema = 1\ndescription = 'Staged set fixture'\n")
		switch name {
		case "alpha":
			writeFile(t, filepath.Join(repo, "mod.ts"), "import { beta } from '/p/acme/beta/mod.ts'; export function alpha() { return 'alpha'; } export function result() { return beta(); }\n")
		case "beta":
			writeFile(t, filepath.Join(repo, "mod.ts"), "import { alpha } from '/p/acme/alpha/mod.ts'; export function beta() { return alpha() + '-beta'; }\n")
		case "missingref":
			writeFile(t, filepath.Join(repo, "hooks/pre-activate.toml"), "hook = 'pre-activate'\ndescription = 'Missing peer'\nprogram = 'acme/absent/prepare'\n")
		case "invalid":
			writeFile(t, filepath.Join(repo, "package.toml"), "invalid toml [")
		}
		runTestGit(t, git, repo, "init", "-q", "-b", "main")
		runTestGit(t, git, repo, "add", ".")
		runTestGit(t, git, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "fixture")
		commits[name] = runTestGit(t, git, repo, "rev-parse", "HEAD")
		runTestGit(t, git, repo, "tag", "selected")
		bare := filepath.Join(root, "acme", name+".git")
		runTestGit(t, git, "", "clone", "-q", "--bare", repo, bare)
		runTestGit(t, git, bare, "update-server-info")
	}
	server := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(server.Close)
	return server.URL, commits
}

func encodeSynchronization(t *testing.T, id, url string, commits map[string]string, names ...string) string {
	t.Helper()
	request := SynchronizationSet{ActivationID: id}
	for _, name := range names {
		commit := commits[name]
		if commit == "" {
			commit = strings.Repeat("a", 40)
		}
		request.Packages = append(request.Packages, SynchronizationSelection{PackageID: "acme/" + name, Source: url + "/acme/" + name + ".git", Commit: commit})
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestSynchronizationSetMutualImportsAndRecovery(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("requires Deno for real mutual imports")
	}
	url, commits := synchronizationRemotes(t)
	for _, names := range [][]string{{"alpha", "beta"}, {"beta", "alpha"}} {
		t.Run(strings.Join(names, "-"), func(t *testing.T) {
			root, store, db := activationStore(t)
			calls, fail := map[string]int{}, true
			runner := activationRunFunc(func(ctx context.Context, id, _ string, options jobs.Options) (jobs.Record, error) {
				calls[id]++
				imports := map[string]string{}
				for _, name := range names {
					imports["/p/acme/"+name+"/"] = "file://" + filepath.Join(root, "packages/acme", name) + "/"
				}
				for _, mount := range options.Mounts {
					imports["/p/"+mount.OwnerScope+"/"] = "file://" + mount.Source + "/"
				}
				encoded, _ := json.Marshal(map[string]any{"imports": imports})
				mapping := filepath.Join(root, "imports.json")
				writeFile(t, mapping, string(encoded))
				command := exec.CommandContext(ctx, deno, "eval", "--no-config", "--import-map", mapping, "const m = await import('/p/acme/alpha/mod.ts'); if (m.result() !== 'alpha-beta') throw Error('cycle');")
				if output, err := command.CombinedOutput(); err != nil {
					return jobs.Record{}, fmt.Errorf("%s: %s: %w", id, output, err)
				}
				if fail && id == "acme/beta/post-activate" {
					return jobs.Record{}, errors.New("injected post hook failure")
				}
				return jobs.Record{State: "SUCCEEDED"}, nil
			})
			schema := &recordingSchemaHook{}
			schema.inspect = func(candidates []deployment.Candidate) error {
				if len(candidates) != 2 {
					return errors.New("not one complete schema set")
				}
				for _, candidate := range candidates {
					if _, err := os.Stat(filepath.Join(root, "packages", candidate.PackageID, "mod.ts")); !os.IsNotExist(err) {
						return errors.New("source published before preparation")
					}
				}
				return nil
			}
			c, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store, Jobs: runner})
			if err != nil {
				t.Fatal(err)
			}
			store.SetSchemaDeployment(c)
			const id = "act-0123456789"
			input := encodeSynchronization(t, id, url, commits, names...)
			if _, err := store.SynchronizeSet(t.Context(), input, "", ""); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("failure: %v", err)
			}
			assertActivationState(t, db, "code_switched")
			for _, name := range names {
				entry, _, err := store.index.Get(t.Context(), "acme/"+name)
				if err != nil || entry.Source != "" || entry.ActiveCommit != "" {
					t.Fatalf("premature desired/active publication: %+v, %v", entry, err)
				}
			}
			if _, err := store.SynchronizeSet(t.Context(), input, "", ""); err == nil || !strings.Contains(err.Error(), "identity already exists") {
				t.Fatalf("replayed: %v", err)
			}
			// A source configuration changed after failure cannot be overwritten by resume.
			if _, err := db.ExecContext(t.Context(), `UPDATE `+packagesTable+` SET "local" = $1 WHERE "packageId" = 'acme/alpha'`, true); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ResumeSynchronization(t.Context(), id); err == nil || !strings.Contains(err.Error(), "configuration changed") {
				t.Fatalf("resumed changed config: %v", err)
			}
			if calls["acme/beta/post-activate"] != 1 {
				t.Fatal("hook repeated before configuration validation")
			}
			if _, err := db.ExecContext(t.Context(), `UPDATE `+packagesTable+` SET "local" = $1 WHERE "packageId" = 'acme/alpha'`, false); err != nil {
				t.Fatal(err)
			}
			fail = false
			inspection, err := store.ResumeSynchronization(t.Context(), id)
			if err != nil || inspection.Stage != "complete" {
				t.Fatalf("resume: %+v %v", inspection, err)
			}
			for _, name := range names {
				entry, _, err := store.index.Get(t.Context(), "acme/"+name)
				if err != nil || entry.Source != url+"/acme/"+name+".git" || entry.Commit != commits[name] || entry.ActiveCommit != commits[name] || entry.Local || entry.State != "ready" {
					t.Fatalf("publication: %+v %v", entry, err)
				}
				if calls["acme/"+name+"/pre-activate"] != 1 {
					t.Fatal("replayed successful pre hook")
				}
			}
			if calls["acme/alpha/post-activate"] != 1 || calls["acme/beta/post-activate"] != 2 {
				t.Fatalf("hook retry counts: %v", calls)
			}
			if _, err := store.ResumeSynchronization(t.Context(), id); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSynchronizationSetRejectsBeforePublication(t *testing.T) {
	url, commits := synchronizationRemotes(t)
	for _, name := range []string{"missing", "invalid", "missingref", "local", "custom", "secret", "retired", "unindexed", "dirty", "old-schema", "duplicate", "oversize", "oversize-intent"} {
		t.Run(name, func(t *testing.T) {
			root, store, db := activationStore(t)
			events := []string{}
			c, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: &activationJobRecorder{events: &events, calls: map[string]int{}, failures: map[string]int{}}})
			if err != nil {
				t.Fatal(err)
			}
			store.SetSchemaDeployment(c)
			names := []string{"alpha", "beta"}
			switch name {
			case "missing", "invalid", "missingref":
				names[1] = name
			case "duplicate":
				names[1] = "alpha"
			case "old-schema":
				if _, err := db.ExecContext(t.Context(), `DROP TABLE `+synchronizationsTable); err != nil {
					t.Fatal(err)
				}
			case "local", "custom", "secret", "retired", "oversize-intent":
				entry := PackageIndex{PackageID: "acme/beta", Author: "acme", Repository: "beta", Source: url + "/acme/beta.git"}
				if name == "local" {
					entry.Local = true
				}
				if name == "custom" || name == "retired" {
					entry.Source = "https://other.test/acme/beta.git"
				}
				if name == "secret" {
					entry.Secret = "private"
				}
				if name == "oversize-intent" {
					entry.Tag = strings.Repeat("v", 512*1024)
				}
				if err := store.index.Put(t.Context(), entry); err != nil {
					t.Fatal(err)
				}
				if name == "retired" {
					if err := store.index.SetActivation(t.Context(), entry.PackageID, "retired", "", nil); err != nil {
						t.Fatal(err)
					}
				}
			case "unindexed", "dirty":
				git, _ := exec.LookPath("git")
				path := filepath.Join(root, "packages/acme/beta")
				runTestGit(t, git, "", "clone", "-q", url+"/acme/beta.git", path)
				if name == "dirty" {
					writeFile(t, filepath.Join(path, "dirty"), "preserve")
				}
			}
			input := encodeSynchronization(t, "act-0123456789", url, commits, names...)
			if name == "oversize" {
				input = strings.Repeat(" ", 256*1024+1)
			}
			_, err = store.SynchronizeSet(t.Context(), input, "", "")
			if err == nil {
				t.Fatal("accepted unsafe set")
			}
			if name == "missingref" && (!strings.Contains(err.Error(), "hooks/pre-activate.toml") || !strings.Contains(err.Error(), "acme/absent/prepare")) {
				t.Fatalf("no dependency provenance: %v", err)
			}
			var count int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+activationsTable).Scan(&count); err != nil || count != 0 {
				t.Fatalf("activation was admitted: %d %v", count, err)
			}
			if len(events) != 0 {
				t.Fatalf("schema/hooks ran: %v", events)
			}
			if _, exists, err := store.index.Get(t.Context(), "acme/alpha"); err != nil || exists {
				t.Fatalf("partial desired record: %t %v", exists, err)
			}
			if _, err := os.Stat(filepath.Join(root, "packages/acme/alpha/mod.ts")); !os.IsNotExist(err) {
				t.Fatalf("partial source: %v", err)
			}
		})
	}
}

func TestSynchronizationResumeRejectsOtherOwners(t *testing.T) {
	root, store, db := activationStore(t)
	events := []string{}
	c, _ := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: &activationJobRecorder{events: &events}})
	store.SetSchemaDeployment(c)
	candidate := writeActivationPackage(t, filepath.Join(root, "stage"), "acme/legacy", false)
	if err := c.Prepare(t.Context(), "act-0123456789", []deployment.Candidate{{PackageID: "acme/legacy", Root: candidate, Commit: "legacy"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResumeSynchronization(t.Context(), "act-0123456789"); err == nil || !strings.Contains(err.Error(), "original owner") {
		t.Fatalf("accepted foreign activation: %v", err)
	}
	assertActivationState(t, db, "pre_activated")
}

func TestSynchronizationSetCommittedSchemaRecovery(t *testing.T) {
	url, commits := synchronizationRemotes(t)
	root, store, db := activationStore(t)
	events := []string{}
	schema := &committedRecommendationSchema{recordingSchemaHook: &recordingSchemaHook{}}
	coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: schema, Packages: store,
		Jobs: &activationJobRecorder{events: &events, calls: map[string]int{}, failures: map[string]int{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.SetSchemaDeployment(coordinator)
	const id = "act-0123456789"
	if _, err := store.SynchronizeSet(t.Context(), encodeSynchronization(t, id, url, commits, "alpha", "beta"), "", ""); err == nil || !strings.Contains(err.Error(), "committed schema") {
		t.Fatalf("committed failure: %v", err)
	}
	assertActivationState(t, db, "staged")
	if len(schema.candidates) != 2 {
		t.Fatal("missing durable schema candidates")
	}
	for _, candidate := range schema.candidates {
		if _, err := os.Stat(candidate.Root); err != nil {
			t.Fatal("lost forward candidate:", err)
		}
		if _, err := os.Stat(filepath.Join(root, "packages", candidate.PackageID, "mod.ts")); !os.IsNotExist(err) {
			t.Fatal("published source before recovery")
		}
	}
	result, err := store.ResumeSynchronization(t.Context(), id)
	if err != nil || result.Stage != "complete" {
		t.Fatalf("forward recovery: %+v %v", result, err)
	}
	for name, commit := range commits {
		if name != "alpha" && name != "beta" {
			continue
		}
		entry, err := store.InspectPackageIndex("acme/" + name)
		if err != nil || entry.ActiveCommit != commit || entry.Commit != commit || entry.Source != url+"/acme/"+name+".git" {
			t.Fatalf("recovered desired/active: %+v %v", entry, err)
		}
	}
}
