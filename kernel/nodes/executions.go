package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"the8020/kernel/identity"
	"the8020/kernel/sandbox/model"
)

const executionInspectPath = "/__the8020/node/execution"

type ExecutionObservation struct {
	NodeID      string                 `json:"node_id"`
	ExecutionID string                 `json:"execution_id"`
	State       string                 `json:"state"`
	SandboxID   string                 `json:"sandbox_id,omitempty"`
	WorkerID    string                 `json:"worker_id,omitempty"`
	Resources   *model.WorkerResources `json:"resources,omitempty"`
}

type ExecutionReader interface {
	ObserveExecution(context.Context, string) (ExecutionObservation, error)
}

func (m *Manager) SetExecutionReader(reader ExecutionReader) {
	m.mu.Lock()
	m.executions = reader
	m.mu.Unlock()
}

func (m *Manager) InspectExecution(ctx context.Context, nodeID, executionID string) (ExecutionObservation, error) {
	if nodeID == "" {
		nodeID = m.localID
	}
	if !identity.Is(nodeID, "nod") || !identity.Is(executionID, "job") {
		return ExecutionObservation{}, errors.New("invalid execution target")
	}
	if nodeID == m.localID {
		m.mu.RLock()
		reader := m.executions
		m.mu.RUnlock()
		if reader == nil {
			return ExecutionObservation{}, errors.New("execution reader unavailable")
		}
		return reader.ObserveExecution(ctx, executionID)
	}
	node, err := m.Inspect(nodeID)
	if err != nil || !node.Enabled {
		return ExecutionObservation{}, errors.New("execution node unavailable")
	}
	body, _ := json.Marshal(map[string]string{"node_id": nodeID, "execution_id": executionID})
	target := "https://" + net.JoinHostPort(node.RecipientAddress, strconv.Itoa(node.RecipientPort)) + executionInspectPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return ExecutionObservation{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := m.http.Do(request)
	if err != nil {
		return ExecutionObservation{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ExecutionObservation{}, errors.New("execution inspection unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 {
		return ExecutionObservation{}, errors.New("execution response exceeds limit")
	}
	var result ExecutionObservation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.Decode(&struct{}{}) != io.EOF || result.NodeID != nodeID || result.ExecutionID != executionID {
		return ExecutionObservation{}, errors.New("execution response identity mismatch")
	}
	return result, nil
}

func (m *Manager) serveExecution(writer http.ResponseWriter, request *http.Request) {
	control := http.NewResponseController(writer)
	if err := control.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		http.Error(writer, "execution read deadline unavailable", http.StatusInternalServerError)
		return
	}
	if err := control.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		http.Error(writer, "execution write deadline unavailable", http.StatusInternalServerError)
		return
	}
	var input struct {
		NodeID      string `json:"node_id"`
		ExecutionID string `json:"execution_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 512))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF || input.NodeID != m.localID {
		http.Error(writer, "invalid execution target", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	result, err := m.InspectExecution(ctx, input.NodeID, input.ExecutionID)
	if err != nil {
		http.Error(writer, "execution inspection unavailable", http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(result)
}
