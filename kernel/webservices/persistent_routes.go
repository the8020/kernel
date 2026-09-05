package webservices

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"the8020/kernel/auth"
	"the8020/kernel/sandbox/model"
)

const RouteHeader = "the8020-route"

var errRouteNotFound = errors.New("persistent execution lost")

func persistentRequestToken(request *http.Request) (string, error) {
	headers := request.Header.Values(RouteHeader)
	var query []string
	if isWebSocketUpgrade(request) {
		query = request.URL.Query()["route"]
	}
	if len(headers) > 1 || len(query) > 1 {
		return "", auth.ErrInvalidRoute
	}
	token := ""
	for _, values := range [][]string{headers, query} {
		if len(values) == 0 {
			continue
		}
		value := strings.TrimSpace(values[0])
		if value == "" || token != "" && value != token {
			return "", auth.ErrInvalidRoute
		}
		token = value
	}
	return token, nil
}

func (m *Manager) beginPersistentDispatch(ctx context.Context, runtime *runtimeService, definition Specification, token string) (*persistentDispatch, error) {
	if token != "" {
		target, err := m.signing.VerifyRoute(token)
		if err != nil {
			return nil, err
		}
		if target.NodeID != m.nodeID {
			return &persistentDispatch{token: token, record: target, remoteNode: target.NodeID}, nil
		}
		// This service's live pool owns Worker membership. The supervisor checks
		// the exact execution's existence and original principal before dispatch.
		sandbox := m.selectPersistentSandbox(runtime, target.SandboxID, target.WorkerID)
		if sandbox == nil {
			return nil, errRouteNotFound
		}
		return &persistentDispatch{token: token, record: target, sandbox: sandbox, keepAlive: definition.Effective.Lifecycle.SessionKeepAlive}, nil
	}
	sandbox, err := m.selectCapacitySandbox(ctx, runtime, definition)
	if err != nil {
		return nil, err
	}
	executionID, err := model.NewID("persistent")
	if err != nil {
		m.finishRequest(runtime, sandbox, 0, 0, 0, false)
		return nil, err
	}
	return &persistentDispatch{
		record:  auth.RouteTarget{NodeID: m.nodeID, SandboxID: sandbox.status.SandboxID, ExecutionID: executionID},
		sandbox: sandbox, initial: true, keepAlive: definition.Effective.Lifecycle.SessionKeepAlive,
	}, nil
}

// Both HTTP and WebSocket responses pass here after the supervisor has chosen
// the exact Worker and before public headers are sent. Application headers
// cannot replace trusted routing or expose internal transport metadata.
func (m *Manager) prepareServiceResponse(response *http.Response, route *persistentDispatch) error {
	workerID := response.Header.Get(internalHeaderPrefix + "selected-worker-id")
	for name := range response.Header {
		if strings.HasPrefix(strings.ToLower(name), internalHeaderPrefix) {
			delete(response.Header, name)
		}
	}
	response.Header.Del(RouteHeader)
	if route == nil || response.StatusCode != http.StatusSwitchingProtocols && (response.StatusCode < 200 || response.StatusCode >= 400) {
		return nil
	}
	if workerID == "" || !route.initial && workerID != route.record.WorkerID {
		return errors.New("supervisor response is missing the selected execution Worker")
	}
	if route.initial {
		route.record.WorkerID = workerID
		token, err := m.signing.SignRoute(route.record)
		if err != nil {
			return err
		}
		route.token = token
	}
	response.Header.Set(RouteHeader, route.token)
	return nil
}
