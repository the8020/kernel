// Package client invokes typed command requests over a local Unix socket.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"the8020/kernel/cbus/core"
)

const commandTimeout = 5 * time.Minute

// Client is a reusable command-bus client bound to one Unix socket.
type Client struct {
	http      *http.Client
	transport *http.Transport
}

// New creates a client for socketPath.
func New(socketPath string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		return dialer.DialContext(ctx, "unix", socketPath)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: commandTimeout}, transport: transport}
}

// Execute sends one typed request and returns its complete response envelope.
func (c *Client) Execute(ctx context.Context, request core.Request) (core.Response, error) {
	if request.ProtocolVersion == 0 {
		request.ProtocolVersion = core.ProtocolVersion
	}
	if request.RequestID == "" {
		request.RequestID = core.NewRequestID()
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return core.Response{}, fmt.Errorf("encode command request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v2/cbus/execute", bytes.NewReader(payload))
	if err != nil {
		return core.Response{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		return core.Response{}, fmt.Errorf("connect to kernel command bus: %w", err)
	}
	defer httpResponse.Body.Close()
	var response core.Response
	decoder := json.NewDecoder(httpResponse.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return core.Response{}, fmt.Errorf("decode command response: %w", err)
	}
	if response.ProtocolVersion != core.ProtocolVersion {
		return core.Response{}, fmt.Errorf("unsupported response protocol version %d", response.ProtocolVersion)
	}
	return response, nil
}

// Catalog fetches the current process-local catalog. When knownRevision still
// matches, unchanged is true and no catalog body is transferred.
func (c *Client) Catalog(ctx context.Context, knownRevision string) (catalog core.Catalog, unchanged bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/v2/cbus/catalog", nil)
	if err != nil {
		return catalog, false, err
	}
	if knownRevision != "" {
		request.Header.Set("If-None-Match", `"`+knownRevision+`"`)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return catalog, false, fmt.Errorf("connect to kernel command bus: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return catalog, true, nil
	}
	if response.StatusCode != http.StatusOK {
		return catalog, false, fmt.Errorf("fetch command catalog: HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&catalog); err != nil {
		return catalog, false, fmt.Errorf("decode command catalog: %w", err)
	}
	if catalog.ProtocolVersion != core.ProtocolVersion || catalog.Revision == "" {
		return catalog, false, fmt.Errorf("unsupported command catalog protocol version %d", catalog.ProtocolVersion)
	}
	return catalog, false, nil
}

// Close releases idle client connections.
func (c *Client) Close() { c.transport.CloseIdleConnections() }
