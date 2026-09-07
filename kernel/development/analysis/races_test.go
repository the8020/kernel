//go:build workflowanalysis

package development

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"the8020/kernel/deployment"
)

type analysisPauseDriver struct {
	SandboxDriver
	atPause func()
}

func (d analysisPauseDriver) Pause(ctx context.Context, id string) error {
	d.atPause()
	return d.SandboxDriver.Pause(ctx, id)
}

type analysisGateHook struct{ entered, release chan struct{} }

func (h analysisGateHook) Prepare(context.Context, []deployment.Candidate) error {
	close(h.entered)
	<-h.release
	return nil
}
func (h analysisGateHook) Complete(context.Context, bool) error { return nil }

func TestWorkflowAnalysisRaces(t *testing.T) {
	platform := newTestPlatform(t)
	m := platform.manager
	sandbox, err := m.Create(context.Background(), "analysisrace")
	if err != nil {
		t.Fatal(err)
	}
	shell(t, m, sandbox.UserID, "write packages/the8020/dev-core/src/message.ts captured")
	m.driver = analysisPauseDriver{SandboxDriver: platform.driver, atPause: func() {
		writeTestFile(t, filepath.Join(platform.driver.views[sandbox.SandboxID].packages, "the8020/dev-core/late.txt"), "written after capture\n")
	}}
	result, err := m.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Capture race"})
	if err != nil {
		t.Fatal(err)
	}
	_, sharedErr := os.Stat(filepath.Join(platform.root, "packages/the8020/dev-core/late.txt"))
	_, privateErr := os.Stat(filepath.Join(platform.driver.views[sandbox.SandboxID].packages, "the8020/dev-core/late.txt"))
	t.Logf("capture-to-pause write: committed=%v shared_retained=%v private_retained=%v", result.Success, sharedErr == nil, privateErr == nil)
	m.driver = platform.driver
	shell(t, m, sandbox.UserID, "write packages/the8020/dev-core/src/message.ts second")
	hook := analysisGateHook{entered: make(chan struct{}), release: make(chan struct{})}
	m.SetSchemaDeployment(hook)
	done := make(chan error, 1)
	go func() {
		_, err := m.Activate(context.Background(), sandbox.UserID, ActivationOptions{Description: "Lock probe"})
		done <- err
	}()
	select {
	case <-hook.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not start")
	}
	read := make(chan struct{})
	started := time.Now()
	go func() { m.repositoryMu.RLock(); m.repositoryMu.RUnlock(); close(read) }()
	select {
	case <-read:
		t.Log("repository read progressed during schema hook")
	case <-time.After(200 * time.Millisecond):
		t.Log("unrelated repository read blocked for entire 200ms schema-hook window")
	}
	close(hook.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation did not finish after release")
	}
	select {
	case <-read:
		t.Logf("repository read released after %s", time.Since(started))
	case <-time.After(time.Second):
		t.Fatal("repository lock remained held")
	}
}
