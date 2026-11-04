package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/deployment"
	"the8020/kernel/execution/jobs"
)

func TestActivationHookCandidateMountsWithMutualImports(t *testing.T) {
	deno, err := exec.LookPath("deno")
	if err != nil {
		t.Skip("requires Deno for real mutual imports")
	}
	for _, order := range [][]string{{"acme/alpha", "acme/beta"}, {"acme/beta", "acme/alpha"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			root, store, db := activationStore(t)
			candidateParent := t.TempDir()
			candidates := make([]deployment.Candidate, 0, len(order))
			for _, id := range order {
				writeActivationPackage(t, filepath.Join(root, "packages"), id, true)
				putActivePackage(t, store, id, "old")
				candidate := writeActivationPackage(t, candidateParent, id, true)
				candidates = append(candidates, deployment.Candidate{PackageID: id, Root: candidate, Commit: "new"})
			}
			writeFile(t, filepath.Join(candidateParent, "acme/alpha/mod.ts"), "import { beta } from '/p/acme/beta/mod.ts'; export function alpha() { return 'alpha'; } export function result() { return beta(); }\n")
			writeFile(t, filepath.Join(candidateParent, "acme/beta/mod.ts"), "import { alpha } from '/p/acme/alpha/mod.ts'; export function beta() { return alpha() + '-beta'; }\n")
			calls := map[string]int{}
			runner := activationRunFunc(func(ctx context.Context, id, _ string, options jobs.Options) (jobs.Record, error) {
				pre := strings.HasSuffix(id, "/pre-activate")
				phase := "post"
				if pre {
					phase = "pre"
				}
				calls[phase]++
				roots := map[string]string{}
				for _, candidate := range candidates {
					installed := filepath.Join(root, "packages", candidate.PackageID)
					_, statErr := os.Stat(filepath.Join(installed, "mod.ts"))
					if pre && !os.IsNotExist(statErr) || !pre && statErr != nil {
						return jobs.Record{}, fmt.Errorf("%s installed source %s: %v", phase, candidate.PackageID, statErr)
					}
					roots[candidate.PackageID] = installed
				}
				if pre && len(options.Mounts) != len(candidates) || !pre && len(options.Mounts) != 0 {
					return jobs.Record{}, fmt.Errorf("%s mounts: %#v", phase, options.Mounts)
				}
				for _, mount := range options.Mounts {
					if !mount.ReadOnly || !strings.HasPrefix(mount.Target, "/workspace/packages/") {
						return jobs.Record{}, fmt.Errorf("invalid candidate mount: %#v", mount)
					}
					roots[strings.TrimPrefix(mount.Target, "/workspace/packages/")] = mount.Source
				}
				imports := map[string]string{}
				for id, path := range roots {
					imports["/p/"+id+"/"] = "file://" + path + "/"
				}
				encoded, _ := json.Marshal(map[string]any{"imports": imports})
				mapping := filepath.Join(root, "imports.json")
				if err := os.WriteFile(mapping, encoded, 0600); err != nil {
					return jobs.Record{}, err
				}
				command := exec.CommandContext(ctx, deno, "eval", "--no-config", "--import-map", mapping, "const m = await import('/p/acme/alpha/mod.ts'); if (m.result() !== 'alpha-beta') throw Error('unexpected result');")
				if output, err := command.CombinedOutput(); err != nil {
					return jobs.Record{}, fmt.Errorf("%s mutual imports: %s: %w", phase, output, err)
				}
				return jobs.Record{State: "SUCCEEDED"}, nil
			})
			events := []string{}
			coordinator, err := NewActivationCoordinator(ActivationCoordinatorConfig{Database: db, Schema: &activationSchemaRecorder{events: &events}, Packages: store, Jobs: runner})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := coordinator.Prepare(ctx, "act-0123456789", candidates); err != nil {
				t.Fatal(err)
			}
			for _, candidate := range candidates {
				if _, err := replacePackageDirectory(filepath.Join(root, "packages", candidate.PackageID), candidate.Root); err != nil {
					t.Fatal(err)
				}
			}
			if err := coordinator.Complete(ctx, "act-0123456789", true); err != nil {
				t.Fatal(err)
			}
			if calls["pre"] != 2 || calls["post"] != 2 {
				t.Fatalf("hook calls: %#v", calls)
			}
		})
	}
}
