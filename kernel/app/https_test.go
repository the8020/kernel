package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"the8020/kernel/settings"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/programs"
	"the8020/kernel/network"
	"the8020/kernel/packages"
	"the8020/kernel/webservices"
)

type httpsPrograms struct {
	run func(string, jobs.Options) (any, error)
}

func (h httpsPrograms) ResolveProgram(_ context.Context, id string) (packages.ProgramDefinition, error) {
	return packages.ProgramDefinition{ID: id, PackageID: "the8020/https", Commit: "active", EntrypointURL: "file:///program.ts"}, nil
}
func (h httpsPrograms) Run(_ context.Context, id, _ string, options jobs.Options) (jobs.Record, error) {
	value, err := h.run(id, options)
	return jobs.Record{Result: value}, err
}

func TestHTTPSPublicationUsesPackageProgramsAndPreservesAcceptedListener(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}))
	state := network.HTTPSState{Revision: 1, Certificates: []network.ServerCertificate{{ID: "one", Name: "localhost", KeySecret: "key", Chain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}}}
	state.Configuration.Enabled = true
	state.Configuration.AllowedDomains = []string{"localhost"}
	keyReads := 0
	fixture := httpsPrograms{run: func(id string, options jobs.Options) (any, error) {
		if options.User != execution.SystemUser() || len(options.Secrets) != 0 || len(options.Mounts) != 0 {
			t.Fatal("HTTPS bypassed ordinary system programs")
		}
		switch id {
		case "the8020/https/snapshot":
			encoded, _ := json.Marshal(state)
			var result any
			if err := json.Unmarshal(encoded, &result); err != nil {
				t.Fatal(err)
			}
			return result, nil
		case "the8020/secrets/get":
			keyReads++
			return map[string]any{"secret": map[string]any{"value": privateKey}}, nil
		case "the8020/https/challenge":
			if options.Timeout != 5*time.Second {
				t.Fatal("unbounded challenge")
			}
			return "challenge-response", nil
		default:
			return nil, fmt.Errorf("unexpected program %s", id)
		}
	}}
	runner, err := programs.New(fixture, fixture)
	if err != nil {
		t.Fatal(err)
	}
	publisher := newHTTPSPublication(t.Context(), runner, availablePort(t))
	if publisher.publish(t.Context(), true) == nil {
		t.Fatal("published before ingress readiness")
	}
	manager, err := network.New(availablePort(t), "the8020/uui/shell/")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	publisher.attach(manager)
	if err := publisher.publish(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	port := manager.TLSPort()
	connect := func() {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{ServerName: "localhost", InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	connect()
	state.Revision++
	if err := publisher.publish(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if keyReads != 1 {
		t.Fatalf("unchanged key loaded %d times", keyReads)
	}

	nodeSettings := filepath.Join(t.TempDir(), "kernel.toml")
	defs := []settings.Definition{{Key: "network.https_port", Type: settings.TypeInteger, Storage: settings.StorageNode, Default: int64(port), Environment: "THE8020_NETWORK_HTTPS_PORT", RuntimeMutable: true, Description: "Native TLS port."}}
	configured, err := settings.New(defs, settings.PersistencePaths{Node: nodeSettings}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configured.RegisterApplier([]string{"network.https_port"}, publisher); err != nil {
		t.Fatal(err)
	}
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	if _, err := configured.Set(t.Context(), "network.https_port", strconv.Itoa(occupiedPort)); err == nil {
		t.Fatal("occupied port accepted")
	}
	occupied.Close()
	if manager.TLSPort() != port {
		t.Fatal("failed preparation changed live TLS")
	}
	candidate := availablePort(t)
	if _, err := configured.Set(t.Context(), "network.https_port", strconv.Itoa(candidate)); err != nil {
		t.Fatal(err)
	}
	port = candidate
	connect()
	// Settings and package publications serialize on this native owner.
	var wait sync.WaitGroup
	results := make(chan error, 2)
	wait.Add(2)
	go func() { defer wait.Done(); results <- publisher.publish(t.Context(), true) }()
	go func() {
		defer wait.Done()
		_, err := configured.Set(t.Context(), "network.https_port", strconv.Itoa(port))
		results <- err
	}()
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if manager.TLSPort() != port {
		t.Fatal("package publication restored an obsolete port")
	}
	// A persistence failure discards the prepared listener and leaves the old one serving.
	badNode := filepath.Join(t.TempDir(), "blocked", "kernel.toml")
	badSettings, err := settings.New(defs, settings.PersistencePaths{Node: badNode}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(badNode), []byte("block parent directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := badSettings.RegisterApplier([]string{"network.https_port"}, publisher); err != nil {
		t.Fatal(err)
	}
	if _, err := badSettings.Set(t.Context(), "network.https_port", strconv.Itoa(availablePort(t))); err == nil {
		t.Fatal("failed persistence succeeded")
	}
	if manager.TLSPort() != port {
		t.Fatal("failed persistence changed live TLS")
	}
	connect()
	state.Revision++
	state.Certificates[0].ID = "two"
	privateKey = "invalid-private-material"
	if err := publisher.publish(t.Context(), true); err == nil {
		t.Fatal("invalid pair accepted")
	}
	status := manager.HTTPSStatus()
	if status.AppliedRevision != 2 || status.DesiredRevision != 3 || status.Error == "" || manager.TLSPort() != port {
		t.Fatalf("status=%+v", status)
	}
	connect()
	response, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/.well-known/acme-challenge/0123456789abcdef", manager.Port()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("challenge status %d", response.StatusCode)
	}
	if err := publisher.publish(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if manager.TLSPort() != 0 || manager.HTTPSStatus().Enabled {
		t.Fatal("retired package retained HTTPS")
	}
}

func TestHTTPSPublicationUsesExistingPendingRetry(t *testing.T) {
	root := t.TempDir()
	source := &runtimePackageIndex{entries: map[string]packages.PackageIndex{
		"the8020/https": {PackageID: "the8020/https", State: "ready", ActiveCommit: "one"},
		"acme/other":    {PackageID: "acme/other", State: "ready", ActiveCommit: "one"},
	}}
	store, err := packages.New(packages.Config{WorkspaceRoot: root, PackagesRoot: root, IndexStore: source})
	if err != nil {
		t.Fatal(err)
	}
	failed, calls := true, 0
	indexer := &runtimeIndexer{packages: store, services: webservices.NewIndex(), jobs: indexJobFunc(func(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
		return jobs.Record{State: "SUCCEEDED", Result: options.Arguments[2]}, nil
	}), https: func(_ context.Context, active bool) error {
		calls++
		if !active {
			t.Fatal("HTTPS package unexpectedly retired")
		}
		if failed {
			return fmt.Errorf("certificate rejected")
		}
		return nil
	}}
	_, _, err = indexer.indexServices(t.Context(), nil, false)
	if err == nil || !indexer.pending["the8020/https"] || indexer.pending["acme/other"] {
		t.Fatalf("pending=%v, err=%v", indexer.pending, err)
	}
	failed = false
	if err := indexer.RetryPending(t.Context()); err != nil || len(indexer.pending) != 0 || calls != 2 {
		t.Fatalf("pending=%v, calls=%d, err=%v", indexer.pending, calls, err)
	}
}
