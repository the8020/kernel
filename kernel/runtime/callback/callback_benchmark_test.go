package callback

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/state"
)

func BenchmarkConcurrentRuntimeHeartbeat(b *testing.B) {
	root := b.TempDir()
	store, err := state.New(filepath.Join(root, "groups"))
	if err != nil {
		b.Fatal(err)
	}
	spec, _ := callbackFixture(b, store)
	server := newCallbackTestServer(b, store, nil)
	if err := server.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = server.Close(context.Background()) })
	body := callbackMessage(b, protocol.MessageHeartbeat, spec, statusPayload{
		Revision: 1, ProtocolVersion: protocol.ProtocolVersion,
		RuntimeGroupID: spec.RuntimeGroupID, SandboxID: spec.SandboxID,
		WorkloadType: string(spec.WorkloadType),
	})
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", server.Address())
		},
	}
	client := &http.Client{Transport: transport}
	b.Cleanup(transport.CloseIdleConnections)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			request, err := http.NewRequest(http.MethodPost, "http://kernel/v1/runtime/heartbeat", bytes.NewReader(body))
			if err != nil {
				b.Error(err)
				continue
			}
			request.Header.Set("Authorization", "Bearer "+spec.InternalToken)
			response, err := client.Do(request)
			if err != nil {
				b.Error(err)
				continue
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK {
				b.Errorf("status=%d read=%v close=%v", response.StatusCode, readErr, closeErr)
			}
		}
	})
}
