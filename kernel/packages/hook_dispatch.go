package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/sandbox/model"
)

const HookDispatcherEntrypoint = "file:///opt/runtime/worker/hook_dispatch.ts"

type JobRunner interface {
	Run(context.Context, string, string, jobs.Options) (jobs.Record, error)
}

type HookReference struct {
	ID         string `json:"id"`
	Entrypoint string `json:"entrypoint"`
	Commit     string `json:"commit"`
}

// RunHookChain submits the dispatcher through the ordinary job path. Package
// revisions join the resolved chain in release compatibility because a handler
// may import any other active package, including a dependency with no hooks.
func (s *Store) RunHookChain(ctx context.Context, runner JobRunner, packageID, hook string, handlers []HookDefinition, scope, state any, mounts []model.Mount) (jobs.Record, error) {
	if _, err := ParsePackageID(packageID); err != nil {
		return jobs.Record{}, err
	}
	references := make([]HookReference, 0, len(handlers))
	for _, handler := range handlers {
		if handler.Program.EntrypointURL == "" || handler.Program.Commit == "" {
			return jobs.Record{}, fmt.Errorf("unresolved hook: %s", handler.ID)
		}
		references = append(references, HookReference{ID: handler.ID, Entrypoint: handler.Program.EntrypointURL, Commit: handler.Program.Commit})
	}
	revision, err := s.index.Revision(ctx)
	if err != nil {
		return jobs.Record{}, err
	}
	encoded, err := json.Marshal(struct {
		Revision uint64
		Handlers []HookReference
	}{revision, references})
	if err != nil {
		return jobs.Record{}, err
	}
	digest := sha256.Sum256(encoded)
	namespace, _, _ := strings.Cut(packageID, "/")
	return runner.Run(ctx, packageID+"/"+hook, HookDispatcherEntrypoint, jobs.Options{
		User: execution.SystemUser(), OwnerID: packageID, Namespace: namespace,
		Arguments: []any{references, scope, state}, Mounts: mounts,
		ReleaseID: hex.EncodeToString(digest[:]), Timeout: 5 * time.Minute,
	})
}
