//go:build workflowanalysis

package development

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"the8020/kernel/deployment"
)

func analysisActivationStorage(shared map[[2]uint64]string, roots ...string) (map[string]int64, error) {
	result := map[string]int64{"captured_payload_files": 0, "captured_payload_bytes": 0, "private_asset_files": 0}
	seen := map[[2]uint64]bool{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			if err != nil {
				return err
			}
			if path == filepath.Join(root, "lower") {
				return filepath.SkipDir
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			st := info.Sys().(*syscall.Stat_t)
			key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if source, known := shared[key]; known && info.Mode().IsRegular() {
				original, err := os.Stat(source)
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				if err == nil && os.SameFile(info, original) {
					result["shared_regular_links"]++
					return nil
				}
			}
			if seen[key] {
				return nil
			}
			seen[key] = true
			result["allocated_bytes"] += st.Blocks * 512
			switch {
			case entry.IsDir():
				result["directories"]++
			case info.Mode().IsRegular():
				result["regular_files"]++
				result["regular_bytes"] += info.Size()
				result["largest_regular_file"] = max(result["largest_regular_file"], info.Size())
				if strings.Contains(path, "/assets/") {
					result["private_asset_files"]++
				}
				relative, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				parts := strings.Split(relative, string(filepath.Separator))
				if len(parts) == 3 && parts[0] == "snapshots" && (parts[2] == "file" || parts[2] == "base") {
					result["captured_payload_files"]++
					result["captured_payload_bytes"] += info.Size()
				}
			default:
				result["other_inodes"]++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func analysisActivationResources(pids map[string]int) (map[string]int64, error) {
	result := map[string]int64{}
	for role, pid := range pids {
		body, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			fields := strings.Fields(line)
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return nil, err
			}
			result[role+"_"+strings.TrimSuffix(fields[0], ":")] = value
		}
		body, err = os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(string(body)[strings.LastIndex(string(body), ")")+1:])
		for _, index := range []int{11, 12, 21} {
			value, err := strconv.ParseInt(fields[index], 10, 64)
			if err != nil {
				return nil, err
			}
			if index == 21 {
				result[role+"_rss_bytes"] = value * int64(os.Getpagesize())
			} else {
				result[role+"_cpu_ticks"] += value
			}
		}
	}
	var children unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_CHILDREN, &children); err != nil {
		return nil, err
	}
	result["waited_children_cpu_us"] = (children.Utime.Sec+children.Stime.Sec)*1_000_000 + children.Utime.Usec + children.Stime.Usec
	result["waited_children_read_bytes"] = children.Inblock * 512
	result["waited_children_write_bytes"] = children.Oublock * 512
	return result, nil
}

func TestWorkflowAnalysisCost(t *testing.T) {
	if !t.Run("reused_inode_accounting", func(t *testing.T) {
		root := t.TempDir()
		original, private := filepath.Join(root, "original"), filepath.Join(root, "private/file")
		writeTestFile(t, original, "shared")
		writeTestFile(t, private, "private")
		info, err := os.Stat(private)
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		// Simulate a saved shared inode number recycled for a new private file.
		stale := map[[2]uint64]string{{uint64(st.Dev), uint64(st.Ino)}: original}
		count, err := analysisActivationStorage(stale, filepath.Dir(private))
		if err != nil || count["regular_bytes"] != 7 || count["shared_regular_links"] != 0 {
			t.Fatalf("stale inode identity hid private bytes: %v: %v", count, err)
		}
	}) {
		t.Fatal("storage accounting failed")
	}
	record := map[string]any{
		"full_workflow_qualified": false, "actual_schema_engine_included": false,
		"storage_reused_inode_guard_passed": true,
		"observed_at":                       time.Now().UTC(), "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"timing_boundary":   "native edit, then actual sandbox helper/HTTP/CBus/Git/publication/acknowledgement with a checking validation hook; excludes real schema engine and package hooks",
		"cache_boundary":    "first use of fresh private Git followed by repeated activations; host caches are not flushed and fixture staging primes asset data",
		"resource_boundary": "live test/kernel, Sentry and Gofer process counters plus waited host children; RSS is sampled before/after, not a per-activation peak; CPU ticks use the host /proc unit",
		"storage_boundary":  "workspace and activation directories, deduplicated by inode, including directory allocation; unchanged shared inodes excluded; durable system/home measured separately; ephemeral sandbox /tmp excluded",
		"gvisor_sdk":        os.Getenv("WORKFLOW_SPARSE_SDK"),
	}
	hashes := map[string]string{}
	for _, name := range []string{"run.py", "runtime_test.go", "gofer_probe.go", "native_probe.go", "sparse_test.go", "sparse_activation_test.go", "activation_cost_test.go", "activation-transaction.patch"} {
		body, err := os.ReadFile(filepath.Join("analysis", name))
		if err != nil {
			t.Fatal(err)
		}
		hashes[name] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	record["source_sha256"] = hashes
	var sdkFix map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("WORKFLOW_SPARSE_SDK_FIX")), &sdkFix); err != nil {
		t.Fatal(err)
	}
	record["sdk_setstat_fix"] = sdkFix
	var cases []map[string]any
	for _, assets := range []int{4, 2048} {
		t.Run(strconv.Itoa(assets), func(t *testing.T) {
			t.Logf("staging %d allocated, tracked 1 MiB assets", assets)
			m, sparse, sandbox, shared := analysisSparseRuntime(t, "cost"+strconv.Itoa(assets), assets)
			d := sparse.RunscDriver
			sharedInodes := map[[2]uint64]string{}
			if err := filepath.WalkDir(m.config.PackagesRoot, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				st := info.Sys().(*syscall.Stat_t)
				sharedInodes[[2]uint64{uint64(st.Dev), uint64(st.Ino)}] = path
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			storage := func(roots ...string) map[string]int64 {
				t.Helper()
				value, err := analysisActivationStorage(sharedInodes, roots...)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			activationRoot := filepath.Join(m.sandboxRoot(sandbox), "activation")
			initial := storage(sparse.storage, activationRoot)
			item := map[string]any{"asset_count": assets, "asset_bytes": int64(assets) << 20, "samples": 5, "initial_workspace": initial, "initial_system": storage(sandbox.SystemPath)}
			var hostFS unix.Statfs_t
			if err := unix.Statfs(shared, &hostFS); err != nil {
				t.Fatal(err)
			}
			item["host_statfs_type"] = fmt.Sprintf("0x%x", hostFS.Type)
			var state struct {
				GoferPID int `json:"goferPid"`
				Sandbox  struct {
					PID int `json:"pid"`
				} `json:"sandbox"`
			}
			if err := readJSON(filepath.Join(d.config.RuntimeRoot, sandbox.SandboxID+"_sandbox:"+sandbox.SandboxID+".state"), &state); err != nil {
				t.Fatal(err)
			}
			pids := map[string]int{"kernel": os.Getpid(), "sentry": state.Sandbox.PID, "gofer": state.GoferPID}
			resources := func() map[string]int64 {
				t.Helper()
				value, err := analysisActivationResources(pids)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			var expected, validated atomic.Value
			m.SetSchemaDeployment(analysisActivationHook{prepare: func(_ context.Context, _ string, candidates []deployment.Candidate) error {
				started := time.Now()
				if len(candidates) != 1 {
					return fmt.Errorf("expected one candidate, got %d", len(candidates))
				}
				root := candidates[0].Root
				body, err := os.ReadFile(filepath.Join(root, "same.txt"))
				if err != nil || string(body) != expected.Load().(string) {
					return fmt.Errorf("incorrect candidate label: %q: %v", body, err)
				}
				for n := range assets {
					name := filepath.Join("assets", strconv.Itoa(n)+".bin")
					original, err := os.Stat(filepath.Join(shared, name))
					if err != nil {
						return err
					}
					candidate, err := os.Stat(filepath.Join(root, name))
					if err != nil || !os.SameFile(original, candidate) {
						return fmt.Errorf("validation copied asset %d: %v", n, err)
					}
				}
				peak, err := analysisActivationStorage(sharedInodes, sparse.storage, activationRoot)
				if err != nil {
					return err
				}
				validated.Store(map[string]any{"checking_hook_ms": float64(time.Since(started).Microseconds()) / 1000, "validation_storage": peak})
				return nil
			}, complete: func(_ context.Context, _ string, activated bool) error {
				body, err := os.ReadFile(filepath.Join(shared, "same.txt"))
				if !activated || err != nil || string(body) != expected.Load().(string) {
					return fmt.Errorf("incorrect completion: activated=%v label=%q: %v", activated, body, err)
				}
				return nil
			}})
			assetTree, err := gitOutput(shared, "rev-parse", "HEAD:assets")
			if err != nil {
				t.Fatal(err)
			}
			item["asset_git_tree"] = assetTree
			var samples []map[string]any
			for n := range 5 {
				label := fmt.Sprintf("field label %d\n", n)
				expected.Store(label)
				before := resources()
				started := time.Now()
				analysisExec(t, d, sandbox, "printf %s "+shellQuote(label)+" >/workspace/packages/the8020/dev-core/same.txt")
				editMS := float64(time.Since(started).Microseconds()) / 1000
				started = time.Now()
				output := analysisExec(t, d, sandbox, "activate --json --message 'One field label'")
				activationMS := float64(time.Since(started).Microseconds()) / 1000
				after := resources()
				var result ActivationResult
				if err := json.Unmarshal([]byte(output), &result); err != nil || !result.Success || result.OverlayReset || len(result.Packages) != 1 {
					t.Fatalf("activation: %+v: %v", result, err)
				}
				if body, err := os.ReadFile(filepath.Join(shared, "same.txt")); err != nil || string(body) != label {
					t.Fatalf("publication: %q: %v", body, err)
				}
				if tree, err := gitOutput(shared, "rev-parse", "HEAD:assets"); err != nil || tree != assetTree {
					t.Fatal("label activation changed the asset tree", err)
				}
				if got := analysisExec(t, d, sandbox, "cat /workspace/packages/the8020/dev-core/same.txt"); got != label {
					t.Fatalf("workspace label: %q", got)
				}
				current := storage(sparse.storage, activationRoot)
				if current["private_asset_files"] != 0 || current["regular_bytes"] > 16<<20 {
					t.Fatalf("label edits copied asset-sized data: %v", current)
				}
				if current["captured_payload_files"] != 0 {
					t.Fatalf("completed activation retained captured file data: %v", current)
				}
				delta, rss := map[string]int64{}, map[string]int64{}
				for key, value := range after {
					if strings.HasSuffix(key, "_rss_bytes") {
						rss[key] = value
					} else {
						delta[key] = value - before[key]
					}
				}
				sample := map[string]any{"edit_helper_ms": editMS, "activation_helper_ms": activationMS, "workspace_after": current, "resource_delta": delta, "rss_after_bytes": rss, "validation": validated.Load()}
				samples = append(samples, sample)
				t.Logf("assets=%d sample=%d edit=%.3f ms activation=%.3f ms private=%d bytes", assets, n, editMS, activationMS, current["regular_bytes"])
			}
			item["observations"], item["final_system"] = samples, storage(sandbox.SystemPath)
			cases = append(cases, item)
		})
	}
	record["cases"] = cases
	record["single_file_activation_cost_check_passed"] = !t.Failed()
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("analysis/activation-cost-results.json", append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
