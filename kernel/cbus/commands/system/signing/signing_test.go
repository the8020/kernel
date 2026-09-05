package signing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"the8020/kernel/auth"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func TestKeyCommandsWorkWithoutDatabaseOrRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "signing.key")
	signer, err := auth.OpenSigner(path, "")
	if err != nil {
		t.Fatal(err)
	}
	serviceSet := &services.Services{Signing: signer}
	registry := core.NewRegistry(nil)
	if err := registry.Register(core.Command{Version: 1, ID: "kernel.signing.status", Path: []string{"kernel.signing.status"}}, Status(serviceSet)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(core.Command{Version: 1, ID: "kernel.signing.replace", Path: []string{"kernel.signing.replace"}, Parameters: []core.Parameter{{Name: "key", Type: "string", Required: true, Secret: true}}}, Replace(serviceSet)); err != nil {
		t.Fatal(err)
	}
	oldSignature := signer.Sign([]byte("payload"))
	seed := base64.StdEncoding.EncodeToString(make([]byte, 32))
	response := registry.Execute(context.Background(), core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: "kernel.signing.replace", Secrets: map[string]string{"key": seed}})
	if !response.Success || signer.Verify([]byte("payload"), oldSignature) {
		t.Fatal("replacement did not invalidate previous signatures")
	}
	encoded, err := json.Marshal(response)
	if err != nil || strings.Contains(string(encoded), seed) {
		t.Fatal("key material escaped command response")
	}
	reloaded, err := auth.OpenSigner(path, "")
	if err != nil || reloaded.Fingerprint() != signer.Fingerprint() {
		t.Fatal("command replacement did not persist")
	}
	status := registry.Execute(context.Background(), core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: "kernel.signing.status"})
	if !status.Success || status.Result.(core.Result)["fingerprint"] != signer.Fingerprint() {
		t.Fatal("key status unavailable without database")
	}
}
