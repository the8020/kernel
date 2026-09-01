package ports

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomaticPortStreamsAndCloses(t *testing.T) {
	upstream := echoServer(t)
	target := upstream.Addr().(*net.TCPAddr)
	manager, err := New(filepath.Join(t.TempDir(), "ports"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	lease, err := manager.Expose(context.Background(), Request{SandboxID: "sandbox", SandboxIP: target.IP.String(), InternalPort: target.Port, Protocol: "tcp", Purpose: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.HostPort == 0 || lease.BindAddress != "127.0.0.1" || len(manager.List()) != 1 {
		t.Fatalf("lease=%#v list=%#v", lease, manager.List())
	}
	connection, err := net.Dial("tcp", net.JoinHostPort(lease.BindAddress, fmt.Sprintf("%d", lease.HostPort)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(connection, "hello\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(connection).ReadString('\n')
	_ = connection.Close()
	if err != nil || line != "hello\n" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if err := manager.Close(lease.LeaseID); err != nil {
		t.Fatal(err)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("lease remains: %#v", manager.List())
	}
	if _, err := os.Stat(filepath.Join(manager.root, lease.LeaseID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record remains: %v", err)
	}
}

func TestCloseForSandboxReleasesOnlyMatchingLeases(t *testing.T) {
	upstream := echoServer(t)
	target := upstream.Addr().(*net.TCPAddr)
	manager, err := New(filepath.Join(t.TempDir(), "ports"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	first, err := manager.Expose(context.Background(), Request{SandboxID: "sandbox-one", SandboxIP: target.IP.String(), InternalPort: target.Port, Purpose: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Expose(context.Background(), Request{SandboxID: "sandbox-two", SandboxIP: target.IP.String(), InternalPort: target.Port, Purpose: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.CloseForSandbox("sandbox-one"); err != nil {
		t.Fatal(err)
	}
	leases := manager.List()
	if len(leases) != 1 || leases[0].LeaseID != second.LeaseID {
		t.Fatalf("remaining leases=%#v", leases)
	}
	if _, err := os.Stat(filepath.Join(manager.root, first.LeaseID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed lease record remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.root, second.LeaseID+".json")); err != nil {
		t.Fatalf("unrelated lease record missing: %v", err)
	}
	if err := manager.CloseForSandbox("sandbox-one"); err != nil {
		t.Fatalf("repeated cleanup: %v", err)
	}
	if err := manager.CloseForSandbox(""); err == nil {
		t.Fatal("empty sandbox ID accepted")
	}
}

func TestExplicitOccupiedAndPublicPortsAreRejected(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	manager, err := New(filepath.Join(t.TempDir(), "ports"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{SandboxID: "sandbox", SandboxIP: "127.0.0.1", InternalPort: 8000, BindAddress: "127.0.0.1", HostPort: occupied.Addr().(*net.TCPAddr).Port, Purpose: "test"}
	if _, err := manager.Expose(context.Background(), request); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("occupied error=%v", err)
	}
	request.HostPort = 0
	request.BindAddress = "0.0.0.0"
	if _, err := manager.Expose(context.Background(), request); err == nil || !strings.Contains(err.Error(), "public") {
		t.Fatalf("public error=%v", err)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("failed exposure changed state: %#v", manager.List())
	}
}

func TestExposeHTTPUsesGoOwnedStreamingHandler(t *testing.T) {
	manager, err := New(filepath.Join(t.TempDir(), "ports"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-80-20-Test", "stream")
		writer.WriteHeader(http.StatusCreated)
		_, _ = io.Copy(writer, request.Body)
	})
	lease, err := manager.ExposeHTTP(context.Background(), Request{SandboxID: "sandbox", SandboxIP: "10.88.0.2", InternalPort: 8000, Purpose: "service"}, handler)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/echo", lease.HostPort), "text/plain", strings.NewReader("streamed"))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusCreated || response.Header.Get("X-80-20-Test") != "stream" || string(body) != "streamed" {
		t.Fatalf("status=%d header=%q body=%q err=%v", response.StatusCode, response.Header.Get("X-80-20-Test"), body, readErr)
	}
}

func TestLeaseExpiresAndRestoreRebindsSafeRecord(t *testing.T) {
	upstream := echoServer(t)
	target := upstream.Addr().(*net.TCPAddr)
	root := filepath.Join(t.TempDir(), "ports")
	manager, err := New(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Expose(context.Background(), Request{SandboxID: "sandbox", SandboxIP: target.IP.String(), InternalPort: target.Port, Purpose: "expiring", ExpiresAt: time.Now().Add(30 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(manager.List()) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("lease did not expire: %#v", manager.List())
	}

	persistent, err := manager.Expose(context.Background(), Request{SandboxID: "sandbox", SandboxIP: target.IP.String(), InternalPort: target.Port, Purpose: "restore"})
	if err != nil {
		t.Fatal(err)
	}
	port := persistent.HostPort
	manager.mu.Lock()
	listener := manager.listeners[persistent.LeaseID]
	cancel := manager.cancel[persistent.LeaseID]
	delete(manager.listeners, persistent.LeaseID)
	delete(manager.cancel, persistent.LeaseID)
	delete(manager.leases, persistent.LeaseID)
	manager.mu.Unlock()
	cancel()
	_ = listener.Close()
	restored, err := manager.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	if len(restored) != 1 || restored[0].HostPort != port || restored[0].LeaseID != persistent.LeaseID {
		t.Fatalf("restored=%#v", restored)
	}
}

func TestRestoreDiscardsDebugLeaseWithoutItsMemoryOnlyToken(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ports")
	manager, err := New(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Expose(context.Background(), Request{SandboxID: "sandbox", SandboxIP: "127.0.0.1", InternalPort: 9229, Purpose: "debug", ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	listener := manager.listeners[lease.LeaseID]
	cancel := manager.cancel[lease.LeaseID]
	delete(manager.listeners, lease.LeaseID)
	delete(manager.cancel, lease.LeaseID)
	delete(manager.leases, lease.LeaseID)
	manager.mu.Unlock()
	cancel()
	_ = listener.Close()
	restored, err := manager.Restore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatalf("debug lease restored: %#v", restored)
	}
	if _, err := os.Stat(filepath.Join(root, lease.LeaseID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("debug lease record remains: %v", err)
	}
}

func TestRestoreForRebindsOnlyAllowedLeasesAndRemovesRejectedRecords(t *testing.T) {
	upstream := echoServer(t)
	target := upstream.Addr().(*net.TCPAddr)
	root := filepath.Join(t.TempDir(), "ports")
	manager, err := New(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := manager.Expose(context.Background(), Request{SandboxID: "sandbox-healthy", OwnerID: "owner-healthy", SandboxIP: target.IP.String(), InternalPort: target.Port, Purpose: "administrative"})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := manager.Expose(context.Background(), Request{SandboxID: "sandbox-failed", OwnerID: "owner-failed", SandboxIP: target.IP.String(), InternalPort: target.Port, Purpose: "service"})
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range []Lease{allowed, rejected} {
		manager.mu.Lock()
		listener := manager.listeners[lease.LeaseID]
		cancel := manager.cancel[lease.LeaseID]
		delete(manager.listeners, lease.LeaseID)
		delete(manager.cancel, lease.LeaseID)
		delete(manager.leases, lease.LeaseID)
		manager.mu.Unlock()
		cancel()
		_ = listener.Close()
	}
	restored, err := manager.RestoreFor(context.Background(), func(lease Lease) bool {
		return lease.SandboxID == "sandbox-healthy"
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.CloseAll()
	if len(restored) != 1 || restored[0].LeaseID != allowed.LeaseID || len(manager.List()) != 1 {
		t.Fatalf("restored=%#v active=%#v", restored, manager.List())
	}
	if _, err := os.Stat(filepath.Join(root, rejected.LeaseID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected lease record remains: %v", err)
	}
}

func echoServer(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
		}
	}()
	return listener
}
