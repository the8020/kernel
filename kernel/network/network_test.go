package network

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"the8020/kernel/settings"
)

const testRootAlias = "the8020/uui/shell/"

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}
func getOK(t *testing.T, port int, path string) {
	t.Helper()
	client := http.Client{Timeout: time.Second}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != "OK" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
}

func getRootRedirect(t *testing.T, port int, query, want string) {
	t.Helper()
	client := http.Client{
		Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || response.Header.Get("Location") != want {
		t.Fatalf("root redirect = %d %q, want %d %q", response.StatusCode, response.Header.Get("Location"), http.StatusTemporaryRedirect, want)
	}
}

func TestListenerAndRuntimeReplacement(t *testing.T) {
	oldPort, newPort := freePort(t), freePort(t)
	manager, err := New(oldPort, testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if address := manager.active.listener.Addr().(*net.TCPAddr); !address.IP.IsUnspecified() {
		t.Fatalf("main listener address=%s want all interfaces", address.IP)
	}
	getRootRedirect(t, oldPort, "?next=1", "/the8020/uui/shell/?next=1")
	getOK(t, oldPort, "/health")
	prepared, err := manager.Prepare(context.Background(), settings.Values{"network.main_port": int64(newPort)})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	getRootRedirect(t, newPort, "", "/the8020/uui/shell/")
	if _, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(oldPort)); err != nil {
		t.Fatalf("old port was not released: %v", err)
	}
	noop, err := manager.Prepare(context.Background(), settings.Values{"network.main_port": int64(newPort)})
	if err != nil {
		t.Fatal(err)
	}
	noop.Commit()
	if manager.Port() != newPort {
		t.Fatal("no-op changed port")
	}
}

func TestCloseClearsActivePort(t *testing.T) {
	manager, err := New(freePort(t), testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	manager.Close()
	if port := manager.Port(); port != 0 {
		t.Fatalf("port after close=%d want=0", port)
	}
}

func TestAvailabilityGateKeepsListenerBoundAndRecovers(t *testing.T) {
	port := freePort(t)
	manager, err := New(port, testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	manager.SetAvailable(false, "database unavailable")
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || string(body) != "database unavailable\n" {
		t.Fatalf("gated response=%d %q", response.StatusCode, body)
	}
	if manager.Port() != port {
		t.Fatal("availability gate released the listener")
	}
	manager.SetAvailable(true, "")
	getOK(t, port, "/health")
}

func TestUnavailableReplacementPreservesOldListener(t *testing.T) {
	oldPort := freePort(t)
	manager, err := New(oldPort, testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	if _, err := manager.Prepare(context.Background(), settings.Values{"network.main_port": int64(port)}); err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	if manager.Port() != oldPort {
		t.Fatal("active port changed after failure")
	}
	getOK(t, oldPort, "/health")
}

func TestDynamicLongestPrefixRoutesSurviveReplacement(t *testing.T) {
	oldPort, newPort := freePort(t), freePort(t)
	manager, err := New(oldPort, testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RegisterRoute("/services", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, "general") })); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterRoute("/services/special", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(writer, "special") })); err != nil {
		t.Fatal(err)
	}
	assertBody := func(port int, path, want string) {
		response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if string(body) != want {
			t.Fatalf("%s body=%q want=%q", path, body, want)
		}
	}
	assertBody(oldPort, "/services/a", "general")
	assertBody(oldPort, "/services/special/a", "special")
	prepared, err := manager.Prepare(context.Background(), settings.Values{"network.main_port": int64(newPort)})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Commit()
	assertBody(newPort, "/services/special/a", "special")
	manager.UnregisterRoute("/services/special")
	assertBody(newPort, "/services/special/a", "general")
}

func TestServiceBoundaryHandlesOnlyRequestsWithoutExplicitRoutes(t *testing.T) {
	manager, err := New(freePort(t), testRootAlias)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.RegisterServiceBoundary(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, "service:"+request.URL.Path)
	})); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterRoute("/internal", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "internal")
	})); err != nil {
		t.Fatal(err)
	}
	request := func(path string) (int, string) {
		response, requestErr := http.Get("http://127.0.0.1:" + strconv.Itoa(manager.Port()) + path)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	status, body := request("/core/example/api")
	if status != http.StatusServiceUnavailable || body != "service:/core/example/api" {
		t.Fatalf("service boundary status=%d body=%q", status, body)
	}
	status, body = request("/internal/path")
	if status != http.StatusOK || body != "internal" {
		t.Fatalf("explicit route status=%d body=%q", status, body)
	}
	if err := manager.RegisterServiceBoundary(http.NotFoundHandler()); err == nil {
		t.Fatal("duplicate service boundary accepted")
	}
}

func TestRootAliasRejectsNonCanonicalPaths(t *testing.T) {
	for _, alias := range []string{"", "/the8020/uui/shell/", "../shell", "example//shell", "example/./shell", "example/shell?next=1", "example/shell#fragment", `example\shell`} {
		t.Run(alias, func(t *testing.T) {
			if _, err := New(freePort(t), alias); err == nil {
				t.Fatalf("accepted root alias %q", alias)
			}
		})
	}
}
