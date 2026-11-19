package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"the8020/kernel/deployment"
	"the8020/kernel/identity"
)

const packageSyncPath = "/__the8020/node/package-sync"

type packageSyncRequest struct {
	Node      string                   `json:"node"`
	Selection deployment.NodeSelection `json:"selection"`
}

func (m *Manager) SyncPackage(ctx context.Context, node string, selection deployment.NodeSelection) error {
	if !identity.Is(node, "nod") {
		return errors.New("invalid synchronization node")
	}
	if node == m.localID {
		if m.packageSync == nil {
			return errors.New("node package synchronization unavailable")
		}
		return m.packageSync(ctx, selection)
	}
	peer, err := m.Inspect(node)
	if err != nil || !peer.Enabled {
		return fmt.Errorf("node %s unavailable", node)
	}
	body, err := json.Marshal(packageSyncRequest{node, selection})
	if err != nil {
		return err
	}
	target := "https://" + net.JoinHostPort(peer.RecipientAddress, strconv.Itoa(peer.RecipientPort)) + packageSyncPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	client := *m.http
	client.Timeout = 10 * time.Minute
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("node %s: %s: %s", node, res.Status, message)
	}
	return nil
}

func (m *Manager) SetPackageSync(sync func(context.Context, deployment.NodeSelection) error) {
	m.packageSync = sync
}

func (m *Manager) servePackageSync(w http.ResponseWriter, r *http.Request) {
	control := http.NewResponseController(w)
	if err := control.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		http.Error(w, "control deadline unavailable", 500)
		return
	}
	var input packageSyncRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil || dec.Decode(&struct{}{}) != io.EOF || input.Node != m.localID {
		http.Error(w, "invalid synchronization request", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	if err := m.SyncPackage(ctx, input.Node, input.Selection); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
