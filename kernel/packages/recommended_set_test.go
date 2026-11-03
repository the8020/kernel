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
)

type committedRecommendationSchema struct {
	*recordingSchemaHook
	candidates []deployment.Candidate
	committed  bool
}

func (h *committedRecommendationSchema) Prepare(ctx context.Context, id string, candidates []deployment.Candidate) error {
	if h.committed {
		return nil
	}
	if err := h.recordingSchemaHook.Prepare(ctx, id, candidates); err != nil {
		return err
	}
	h.candidates, h.committed = candidates, true
	return errors.New("lost committed schema response")
}

func (h *committedRecommendationSchema) ForwardRequired(context.Context, string) (bool, error) {
	return h.committed, nil
}

func (h *committedRecommendationSchema) RecoveryCandidates(context.Context, string) ([]deployment.Candidate, error) {
	return h.candidates, nil
}

// These modules have a real import cycle, with the new shared module absent
// from the old i18n release, as in the reported db/system/i18n upgrade.
func TestRecommendedSetMutualImportsAndFailureRetry(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("requires Deno for real mutual imports")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remotes := t.TempDir()
	old, next := map[string]string{}, map[string]string{}
	for _, name := range []string{"db", "system", "i18n"} {
		repository := t.TempDir()
		runTestGit(t, git, "", "init", "-q", "-b", "main", repository)
		writeFile(t, filepath.Join(repository, "package.toml"), "schema = 1\ndescription = 'Upgrade fixture'\n")
		writeFile(t, filepath.Join(repository, "tables", "value.ts"), "export default 'old';\n")
		runTestGit(t, git, repository, "add", ".")
		runTestGit(t, git, repository, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "old")
		old[name] = runTestGit(t, git, repository, "rev-parse", "HEAD")
		runTestGit(t, git, repository, "tag", "0.9.14")
		if name == "i18n" {
			writeFile(t, filepath.Join(repository, "messages.ts"), "import { identity } from '/p/acme/db/fields.ts'; export function message() { return identity('new'); }\n")
		} else if name == "db" {
			writeFile(t, filepath.Join(repository, "fields.ts"), "import { message } from '/p/acme/i18n/messages.ts'; export function identity(value: string) { return value; } export function label() { return message(); }\n")
		}
		writeFile(t, filepath.Join(repository, "tables", "value.ts"), "import { label } from '/p/acme/db/fields.ts'; export default label();\n")
		for _, hook := range []string{"pre-activate", "post-activate"} {
			writeFile(t, filepath.Join(repository, "hooks", hook+".toml"), fmt.Sprintf("hook = '%s'\ndescription = 'Upgrade hook'\nprogram = 'acme/%s/%s'\n", hook, name, hook))
			writeHandlerProgram(t, repository, hook)
		}
		runTestGit(t, git, repository, "add", ".")
		runTestGit(t, git, repository, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "new mutual imports")
		next[name] = runTestGit(t, git, repository, "rev-parse", "HEAD")
		runTestGit(t, git, repository, "tag", "0.10.1")
		bare := filepath.Join(remotes, "acme", name+".git")
		runTestGit(t, git, "", "clone", "-q", "--bare", repository, bare)
		runTestGit(t, git, bare, "update-server-info")
	}
	server := httptest.NewServer(http.FileServer(http.Dir(remotes)))
	defer server.Close()
	for _, order := range [][]string{
		{"db", "system", "i18n"}, {"i18n", "system", "db"},
		{"system", "db", "i18n"}, {"system", "i18n", "db"},
		{"db", "i18n", "system"}, {"i18n", "db", "system"},
	} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			root, store, db := activationStore(t)
			events := []string{}
			schema := &recordingSchemaHook{}
			runner := &activationJobRecorder{events: &events, calls: map[string]int{}, failures: map[string]int{}}
			coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Packages: store, Schema: schema,
				Jobs: runner,
			})
			if err != nil {
				t.Fatal(err)
			}
			store.SetSchemaDeployment(coordinator)
			listPath := filepath.Join(root, "node/kernel/scripts/bootstrap-packages.toml")
			setList := func(version string, names []string) {
				text := "schema = 1\n"
				for _, name := range names {
					text += fmt.Sprintf("[[packages]]\nid = 'acme/%s'\nsource = '%s/acme/%s.git'\nversion = '%s'\n", name, server.URL, name, version)
				}
				writeFile(t, listPath, text)
			}
			assertActive := func(commits map[string]string) {
				t.Helper()
				for name, commit := range commits {
					entry, err := store.InspectPackageIndex("acme/" + name)
					if err != nil || entry.ActiveCommit != commit || entry.State != "ready" {
						t.Fatalf("active %s = %+v, %v", name, entry, err)
					}
					if head := runTestGit(t, git, filepath.Join(root, "packages/acme", name), "rev-parse", "HEAD"); head != commit {
						t.Fatalf("source %s = %s, want %s", name, head, commit)
					}
				}
			}
			setList("0.9.14", order)
			if _, err := store.InstallRecommendedPackages(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertActive(old)
			checks := 0
			schema.inspect = func(candidates []deployment.Candidate) error {
				checks++
				imports := map[string]string{}
				for name := range old {
					imports["/p/acme/"+name+"/"] = "file://" + filepath.Join(root, "packages/acme", name) + "/"
				}
				for _, candidate := range candidates {
					imports["/p/"+candidate.PackageID+"/"] = "file://" + candidate.Root + "/"
				}
				encoded, _ := json.Marshal(map[string]any{"imports": imports})
				mapping := filepath.Join(root, "imports.json")
				writeFile(t, mapping, string(encoded))
				command := exec.CommandContext(t.Context(), deno, "eval", "--no-config", "--import-map", mapping,
					"for (const name of ['db', 'system', 'i18n']) { const value = await import('/p/acme/' + name + '/tables/value.ts'); if (value.default !== 'new') throw Error(name); }")
				output, err := command.CombinedOutput()
				if err != nil {
					return fmt.Errorf("candidate imports: %s: %w", output, err)
				}
				return nil
			}
			// A later source-fetch failure must leave every earlier package active at old.
			setList("0.10.1", append(append([]string{}, order...), "missing"))
			if _, err := store.InstallRecommendedPackages(t.Context()); err == nil || checks != 0 {
				t.Fatalf("fetch failure = %v, schema calls = %d", err, checks)
			}
			assertActive(old)
			// Demonstrate why the released one-at-a-time path fails even with db first.
			setList("0.10.1", []string{"db"})
			if _, err := store.InstallRecommendedPackages(t.Context()); err == nil || !strings.Contains(err.Error(), "messages.ts") {
				t.Fatalf("missing old peer module was accepted: %v", err)
			}
			assertActive(old)
			setList("0.10.1", order)
			// An incompatible retained dirty peer must fail and preserve its contents.
			private := filepath.Join(root, "packages/acme/i18n/private.txt")
			writeFile(t, private, "keep")
			if _, err := store.InstallRecommendedPackages(t.Context()); err == nil || !strings.Contains(err.Error(), "messages.ts") {
				t.Fatalf("incompatible retained peer was accepted: %v", err)
			}
			assertActive(old)
			if raw, _ := os.ReadFile(private); string(raw) != "keep" {
				t.Fatal("dirty work was lost")
			}
			if err := os.Remove(private); err != nil {
				t.Fatal(err)
			}
			runner.failures["acme/system/post-activate:acme/system"] = 1
			coordinator.schema = &committedRecommendationSchema{recordingSchemaHook: schema}
			if _, err := store.InstallRecommendedPackages(t.Context()); err == nil || !strings.Contains(err.Error(), "lost committed schema response") {
				t.Fatalf("committed preparation failure = %v", err)
			}
			assertActive(old)
			before := checks
			if _, err := store.InstallRecommendedPackages(t.Context()); err == nil || !strings.Contains(err.Error(), "injected hook failure") {
				t.Fatalf("post activation failure = %v", err)
			}
			rows, err := store.InstallRecommendedPackages(t.Context())
			if err != nil || len(schema.prepared) != 3 {
				t.Fatalf("batched retry = %+v, %v", rows, err)
			}
			assertActive(next)
			if checks != before || runner.calls["acme/db/pre-activate:acme/db"] != 1 || runner.calls["acme/db/post-activate:acme/db"] != 1 || runner.calls["acme/system/post-activate:acme/system"] != 2 {
				t.Fatalf("recovery repeated completed work: checks %d -> %d, hooks %+v", before, checks, runner.calls)
			}
			if _, err := store.InstallRecommendedPackages(context.Background()); err != nil || checks != before {
				t.Fatalf("same-version retry repeated schema: %v, calls %d -> %d", err, before, checks)
			}
		})
	}
}
