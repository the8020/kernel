package packages

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type countingPackageIndex struct {
	PackageIndexStore
	lists int
}

type countingServiceRevisions struct {
	revision uint64
	changes  []ServiceChange
	loads    int
}

func (s *countingServiceRevisions) ServiceRevision(context.Context) (uint64, error) {
	return s.revision, nil
}
func (s *countingServiceRevisions) ServiceChanges(context.Context, uint64, uint64) ([]ServiceChange, error) {
	s.loads++
	return append([]ServiceChange(nil), s.changes...), nil
}

func TestServiceRevisionFollowerUsesScalarNoChangePath(t *testing.T) {
	store := &countingServiceRevisions{revision: 12}
	follower := &ServiceRevisionFollower{store: store, revision: 12}
	if update, err := follower.Poll(context.Background()); err != nil || update.Revision != 0 || store.loads != 0 {
		t.Fatalf("unchanged poll=%#v change_loads=%d err=%v", update, store.loads, err)
	}
	store.revision = 13
	store.changes = []ServiceChange{{ServiceID: "acme/orders/api", Active: true}}
	if update, err := follower.Poll(context.Background()); err != nil || update.Revision != 13 || store.loads != 1 || !slices.Equal(update.ReconcileServices, []string{"acme/orders/api"}) {
		t.Fatalf("changed poll=%#v change_loads=%d err=%v", update, store.loads, err)
	}
}

func (s *countingPackageIndex) List(ctx context.Context) ([]PackageIndex, error) {
	s.lists++
	return s.PackageIndexStore.List(ctx)
}

func TestPackageRevisionFollowerUsesCheapNoChangePathAndTargetsChangedPackage(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	remoteRoot := t.TempDir()
	working := filepath.Join(t.TempDir(), "source")
	runTestGit(t, gitPath, "", "init", "-q", "-b", "main", working)
	runTestGit(t, gitPath, working, "config", "user.name", "Package Test")
	runTestGit(t, gitPath, working, "config", "user.email", "packages@example.test")
	writeFile(t, filepath.Join(working, "package.toml"), "schema = 1\ndescription = \"Revision test\"\n")
	runTestGit(t, gitPath, working, "add", ".")
	runTestGit(t, gitPath, working, "commit", "-q", "-m", "first")
	firstCommit := runTestGit(t, gitPath, working, "rev-parse", "HEAD")
	bare := filepath.Join(remoteRoot, "acme", "demo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, gitPath, "", "clone", "-q", "--bare", working, bare)
	runTestGit(t, gitPath, bare, "update-server-info")
	server := httptest.NewTLSServer(http.FileServer(http.Dir(remoteRoot)))
	defer server.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	source := server.URL + "/acme/demo.git"

	root := t.TempDir()
	packagesRoot := filepath.Join(root, "packages")
	if err := os.MkdirAll(filepath.Join(packagesRoot, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, gitPath, "", "clone", "-q", source, filepath.Join(packagesRoot, "acme", "demo"))
	db := packageDatabase(t)
	store, err := New(Config{WorkspaceRoot: root, Database: db})
	if err != nil {
		t.Fatal(err)
	}
	entry := PackageIndex{PackageID: "acme/demo", Author: "acme", Repository: "demo", Source: source}
	if err := store.index.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.index.SetActivation(context.Background(), entry.PackageID, "ready", firstCommit, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO "the8020__system__revisions" ("domain", "revision", "updatedAt") VALUES ('packages', 1, $1)`, databaseTime(db)); err != nil {
		t.Fatal(err)
	}
	installConvergenceService(t, store, "acme/demo/old", firstCommit)

	counter := &countingPackageIndex{PackageIndexStore: store.index}
	store.index = counter
	follower, err := NewPackageRevisionFollower(context.Background(), store, map[string]string{"acme/demo": firstCommit})
	if err != nil {
		t.Fatal(err)
	}
	if update, err := follower.Poll(context.Background()); err != nil || update.Revision != 0 || counter.lists != 0 {
		t.Fatalf("unchanged poll=%#v lists=%d err=%v", update, counter.lists, err)
	}

	writeFile(t, filepath.Join(working, "second.txt"), "second\n")
	runTestGit(t, gitPath, working, "add", ".")
	runTestGit(t, gitPath, working, "commit", "-q", "-m", "second")
	secondCommit := runTestGit(t, gitPath, working, "rev-parse", "HEAD")
	runTestGit(t, gitPath, working, "push", "-q", bare, "main")
	runTestGit(t, gitPath, bare, "update-server-info")
	if err := store.index.SetActivation(context.Background(), entry.PackageID, "ready", secondCommit, nil); err != nil {
		t.Fatal(err)
	}
	installConvergenceService(t, store, "acme/demo/new", secondCommit)
	definitions := store.state.(ServiceDefinitionStore)
	if err := definitions.RetirePackage(context.Background(), "acme/demo", []string{"acme/demo/new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE "the8020__system__revisions" SET "revision" = 2`); err != nil {
		t.Fatal(err)
	}

	update, err := follower.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if update.Revision != 2 || !slices.Equal(update.Packages, []string{"acme/demo"}) ||
		!slices.Equal(update.ReconcileServices, []string{"acme/demo/new"}) ||
		!slices.Equal(update.RetireServices, []string{"acme/demo/old"}) {
		t.Fatalf("targeted update=%#v", update)
	}
	if counter.lists != 1 {
		t.Fatalf("package rows loaded %d times", counter.lists)
	}
	head := runTestGit(t, gitPath, filepath.Join(packagesRoot, "acme", "demo"), "rev-parse", "HEAD")
	if head != secondCommit {
		t.Fatalf("checkout=%s want=%s", head, secondCommit)
	}
	if retry, err := follower.Poll(context.Background()); err != nil || retry.Revision != 2 {
		t.Fatalf("unacknowledged revision did not retry: %#v err=%v", retry, err)
	}
	if err := follower.Acknowledge(2); err != nil {
		t.Fatal(err)
	}
	if update, err := follower.Poll(context.Background()); err != nil || update.Revision != 0 {
		t.Fatalf("acknowledged poll=%#v err=%v", update, err)
	}
}

func installConvergenceService(t *testing.T, store *Store, serviceID, commit string) {
	t.Helper()
	identity, err := ParseServiceID(serviceID)
	if err != nil {
		t.Fatal(err)
	}
	definition := Definition{Identity: identity, Service: ServiceManifest{
		Schema: packageManifestSchema + 1, Description: serviceID, Entrypoint: "service.ts",
		Lifecycle: LifecycleManifest{DefaultEnabled: true, ServiceType: ServiceTypeStateless},
		Access:    AccessManifest{Mode: AccessModePublic, Unauthenticated: UnauthenticatedManifest{Action: UnauthenticatedReject, Status: 401, Message: "Authentication is required."}},
	}}
	state := DesiredServiceState{Enabled: true}
	effective := EffectiveConfiguration{
		Lifecycle: LifecycleConfiguration{ServiceType: ServiceTypeStateless, SessionKeepAlive: 10 * time.Minute},
		Scaling:   ScalingConfiguration{ConcurrencyPerWorker: 32, TargetUtilization: .7, WorkerKeepAlive: 2 * time.Minute},
		Placement: PlacementConfiguration{WorkersPerSandbox: 4},
	}
	if err := store.state.(ServiceDefinitionStore).InstallDefinition(context.Background(), definition, state, effective, commit); err != nil {
		t.Fatal(err)
	}
}

func databaseTime(store interface{ Backend() string }) any {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
