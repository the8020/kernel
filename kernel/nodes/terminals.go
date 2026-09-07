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

	"the8020/kernel/identity"
)

const terminalClosePath = "/__the8020/node/terminal-close"
const terminalControlBytes = 1024
const terminalControlTimeout = 10 * time.Second

// TerminalCloser owns physical destruction and local attachment bookkeeping.
// A display Worker is not required to close an existing native process.
type TerminalCloser interface {
	CloseTerminal(context.Context, string) error
}

type terminalCloseRequest struct {
	NodeID     string `json:"node_id"`
	TerminalID string `json:"terminal_id"`
}

type terminalCloseResponse struct {
	terminalCloseRequest
	Closed bool `json:"closed"`
}

func (m *Manager) SetTerminalCloser(closer TerminalCloser) {
	m.mu.Lock()
	m.terminals = closer
	m.mu.Unlock()
}

// CloseTerminal addresses one exact node and never retries another owner.
// The private recipient uses the existing shared kernel authentication.
func (m *Manager) CloseTerminal(ctx context.Context, nodeID, terminalID string) error {
	if nodeID == "" {
		nodeID = m.localID
	}
	if !identity.Is(nodeID, "nod") || !identity.Is(terminalID, "tty") {
		return errors.New("canonical node and terminal IDs are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if nodeID == m.localID {
		m.mu.RLock()
		closer := m.terminals
		m.mu.RUnlock()
		if closer == nil {
			return errors.New("native terminal close is unavailable")
		}
		return closer.CloseTerminal(ctx, terminalID)
	}
	node, err := m.Inspect(nodeID)
	if err != nil || !node.Enabled {
		return errors.New("terminal owner node is unavailable")
	}
	input := terminalCloseRequest{NodeID: nodeID, TerminalID: terminalID}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	target := "http://" + net.JoinHostPort(node.RecipientAddress, strconv.Itoa(node.RecipientPort)) + terminalClosePath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+m.secret)
	request.Header.Set("Content-Type", "application/json")
	client := *m.http
	client.Timeout = terminalControlTimeout
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("close terminal on owning node: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("terminal owner rejected close: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, terminalControlBytes+1))
	if err != nil {
		return err
	}
	if len(data) > terminalControlBytes {
		return errors.New("terminal close response exceeds limit")
	}
	var result terminalCloseResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.Decode(&struct{}{}) != io.EOF || result.terminalCloseRequest != input || !result.Closed {
		return errors.New("invalid terminal close acknowledgement")
	}
	return nil
}

func (m *Manager) serveTerminalClose(writer http.ResponseWriter, request *http.Request) {
	control := http.NewResponseController(writer)
	if err := control.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		http.Error(writer, "terminal request deadline unavailable", http.StatusInternalServerError)
		return
	}
	if err := control.SetWriteDeadline(time.Now().Add(terminalControlTimeout)); err != nil {
		http.Error(writer, "terminal response deadline unavailable", http.StatusInternalServerError)
		return
	}
	var input terminalCloseRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, terminalControlBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF || input.NodeID != m.localID || !identity.Is(input.TerminalID, "tty") {
		http.Error(writer, "invalid terminal close target", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), terminalControlTimeout)
	defer cancel()
	if err := m.CloseTerminal(ctx, input.NodeID, input.TerminalID); err != nil {
		http.Error(writer, "native terminal close failed", http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(terminalCloseResponse{terminalCloseRequest: input, Closed: true})
}
