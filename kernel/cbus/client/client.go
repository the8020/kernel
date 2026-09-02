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
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v1/cbus/execute", bytes.NewReader(payload))
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

// Close releases idle client connections.
func (c *Client) Close() { c.transport.CloseIdleConnections() }
