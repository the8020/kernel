package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
	"the8020/kernel/network"
)

// httpsPublication consumes package snapshots through the ordinary program path.
// It knows neither the certificate tables nor ACME/account policy.
type httpsPublication struct {
	programs  *programs.Runner
	transport *network.HTTPS
	network   atomic.Pointer[network.Manager]
	port      int
}

func newHTTPSPublication(ctx context.Context, runner *programs.Runner, port int) *httpsPublication {
	p := &httpsPublication{programs: runner, port: port}
	p.transport = network.NewHTTPS(ctx, p.issue, p.challenge)
	return p
}

func (p *httpsPublication) attach(manager *network.Manager) {
	manager.SetHTTPSIngress(p.transport)
	p.network.Store(manager)
}

func (p *httpsPublication) publish(ctx context.Context, active bool) (err error) {
	defer func() { p.transport.SetFailure(err) }()
	manager := p.network.Load()
	if manager == nil {
		return errors.New("HTTPS publication awaits native ingress readiness")
	}
	if !active {
		listener, err := manager.PrepareTLS(0, nil)
		if err != nil {
			return err
		}
		p.transport.Clear()
		listener.Commit()
		return nil
	}
	var state network.HTTPSState
	result, err := p.programs.RunWithOptions(ctx, "the8020/https/snapshot", "", nil, nil,
		programs.Options{User: execution.SystemUser(), Timeout: time.Minute})
	if err != nil {
		return err
	}
	value, ok := result.Value.(map[string]any)
	if !ok || value["revision"] == nil || value["certificates"] == nil {
		return errors.New("invalid HTTPS snapshot result")
	}
	configuration, ok := value["configuration"].(map[string]any)
	if !ok {
		return errors.New("invalid HTTPS configuration result")
	}
	if _, ok := configuration["enabled"].(bool); !ok {
		return errors.New("missing HTTPS enabled state")
	}
	encoded, err := json.Marshal(result.Value)
	if err != nil {
		return errors.New("invalid HTTPS snapshot result")
	}
	if len(encoded) > 36*1024*1024 {
		return errors.New("HTTPS snapshot exceeds 36 MiB")
	}
	if err := json.Unmarshal(encoded, &state); err != nil {
		return errors.New("invalid HTTPS snapshot")
	}
	if !state.Configuration.Enabled {
		state.Certificates = nil
	}
	commit, err := p.transport.Prepare(ctx, state, func(ctx context.Context, name string) (string, error) {
		return (&packageSecrets{context: ctx, programs: p.programs}).SecretValue(name)
	})
	if err != nil {
		return err
	}
	port := 0
	if state.Configuration.Enabled {
		port = p.port
	}
	listener, err := manager.PrepareTLS(port, p.transport.TLSConfig())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		listener.Discard()
		return err
	}
	commit()
	listener.Commit()
	return nil
}

func (p *httpsPublication) issue(ctx context.Context, name string) error {
	result, err := p.programs.RunWithOptions(ctx, "the8020/https/issue", "", []any{name}, nil,
		programs.Options{User: execution.SystemUser(), Timeout: 4*time.Minute + 10*time.Second})
	if err != nil {
		return err
	}
	value, ok := result.Value.(map[string]any)
	if !ok {
		return errors.New("invalid certificate issuance response")
	}
	if value["status"] == "busy" {
		return nil
	}
	if (value["status"] != "ready" && value["status"] != "issued") || value["applied"] != true {
		return errors.New("certificate issuance or local publication failed")
	}
	return nil
}

func (p *httpsPublication) challenge(ctx context.Context, name, kind, token string, localOnly bool) (string, error) {
	result, err := p.programs.RunWithOptions(ctx, "the8020/https/challenge", "", []any{name, kind, token, localOnly}, nil,
		programs.Options{User: execution.SystemUser(), Timeout: 5 * time.Second})
	if err != nil {
		return "", err
	}
	if result.Value == nil {
		return "", nil
	}
	value, ok := result.Value.(string)
	if !ok || len(value) > 2048 {
		return "", errors.New("invalid challenge result")
	}
	return value, nil
}
