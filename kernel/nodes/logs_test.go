package nodes

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"the8020/kernel/logging/records"
)

type logReaderFunc func(context.Context, records.Query) (records.Page, error)

func (f logReaderFunc) Query(ctx context.Context, query records.Query) (records.Page, error) {
	return f(ctx, query)
}

func logPeers(t *testing.T, reader LogReader, override ...http.Handler) (*Manager, *Manager, *httptest.Server) {
	t.Helper()
	db := newTestNodeDatabase(t, t.TempDir())
	owner, err := New(db, "nod-aaaaaaaaaa", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	owner.SetLogReader(reader)
	handler := owner.authorize(owner.recipientHandler(http.NotFoundHandler()))
	if len(override) > 0 {
		handler = override[0]
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	address, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	if _, err := owner.Set(context.Background(), Node{ID: owner.localID, URL: server.URL, RecipientAddress: address, RecipientPort: port, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	peer, err := New(db, "nod-bbbbbbbbbb", testSharedSecret)
	if err != nil {
		t.Fatal(err)
	}
	return owner, peer, server
}

func TestLogQueriesUseExactAuthenticatedNodeAndPreserveReferences(t *testing.T) {
	observed := make(chan records.Query, 4)
	owner, peer, server := logPeers(t, logReaderFunc(func(_ context.Context, query records.Query) (records.Page, error) {
		observed <- query
		if query.Cursor != "" {
			return records.Page{State: "expired", Reason: "retention", Records: []records.LocatedRecord{}}, nil
		}
		return records.Page{State: "ok", Cursor: "next", More: true, ScannedBytes: 123, Records: []records.LocatedRecord{{Record: records.Record{
			NodeID: "nod-aaaaaaaaaa", JobID: query.JobID, ContextID: query.ContextID, Username: query.Username,
			Time: time.Now().UTC(), Level: "ERROR", Source: "deno", Component: "worker", Message: "failed\nstack",
		}}}}, nil
	}))
	query := records.Query{Position: "saved", Limit: 1, Filter: records.Filter{NodeID: owner.localID, JobID: "job-0123456789", ContextID: "ctx-0123456789", Username: "alice"}}
	page, err := peer.QueryLogs(context.Background(), query)
	if err != nil || page.State != "ok" || len(page.Records) != 1 || page.Cursor != "next" {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	if got := <-observed; got.Position != query.Position || got.Filter != query.Filter || got.Limit != 1 {
		t.Fatalf("query changed: %#v", got)
	}
	query.Position, query.Cursor = "", page.Cursor
	page, err = peer.QueryLogs(context.Background(), query)
	if err != nil || page.State != "expired" || page.Reason != "retention" {
		t.Fatalf("expiry lost: %#v, %v", page, err)
	}
	<-observed
	request, _ := http.NewRequest(http.MethodPost, server.URL+logQueryPath, strings.NewReader(`{"node_id":"nod-aaaaaaaaaa"}`))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated log query: %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, server.URL+logQueryPath, strings.NewReader(`{"node_id":"nod-bbbbbbbbbb"}`))
	request.Header.Set("Authorization", "Bearer "+testSharedSecret)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong-node query: %d", response.StatusCode)
	}
	select {
	case <-observed:
		t.Fatal("rejected query reached log reader")
	default:
	}
}

func TestStalledLogQueryBodyExpiresWithoutReachingTheReader(t *testing.T) {
	var calls atomic.Int32
	_, _, server := logPeers(t, logReaderFunc(func(context.Context, records.Query) (records.Page, error) {
		calls.Add(1)
		return records.Page{State: "ok"}, nil
	}))
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(2 * logQueryReadTimeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: node\r\nAuthorization: Bearer %s\r\nContent-Length: 100\r\nConnection: close\r\n\r\n{", logQueryPath, testSharedSecret); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("stalled log body did not expire: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("stalled request response=%s reader calls=%d", response.Status, calls.Load())
	}
}

func TestRemoteLogCancellationReachesFileOwner(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	owner, peer, _ := logPeers(t, logReaderFunc(func(ctx context.Context, _ records.Query) (records.Page, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return records.Page{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := peer.QueryLogs(ctx, records.Query{Filter: records.Filter{NodeID: owner.localID}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reader not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller did not cancel")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("file owner did not cancel")
	}
}

func TestRemoteLogPageLimitsAndNodeIdentity(t *testing.T) {
	for _, malformed := range []string{"bytes", "records", "node", "redirect"} {
		t.Run(malformed, func(t *testing.T) {
			var calls atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if malformed == "bytes" {
					_, _ = w.Write([]byte(strings.Repeat("x", records.MaxControlFrame+1)))
					return
				}
				if malformed == "redirect" {
					http.Redirect(w, r, "/must-not-follow", http.StatusTemporaryRedirect)
					return
				}
				response := logQueryResponse{NodeID: "nod-aaaaaaaaaa", Page: records.Page{State: "ok", Records: []records.LocatedRecord{}}}
				if malformed == "records" {
					response.Page.Records = make([]records.LocatedRecord, 2)
				}
				if malformed == "node" {
					response.NodeID = "nod-bbbbbbbbbb"
				}
				_ = json.NewEncoder(w).Encode(response)
			})
			owner, peer, _ := logPeers(t, nil, handler)
			page, err := peer.QueryLogs(context.Background(), records.Query{Limit: 1, Filter: records.Filter{NodeID: owner.localID}})
			if err != nil || page.State != "unavailable" || len(page.Records) != 0 {
				t.Fatalf("malformed response accepted: %#v, %v", page, err)
			}
			if calls.Load() != 1 {
				t.Fatal("log query followed a redirect or replayed a request")
			}
		})
	}
}
