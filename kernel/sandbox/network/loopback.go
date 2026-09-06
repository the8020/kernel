package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"

	"the8020/kernel/sandbox/model"
)

// LoopbackManager assigns distinct host-loopback control endpoints to direct
// rootless runsc sandboxes. It intentionally does not claim network-namespace
// or firewall isolation; those guarantees belong to Manager's CNI path.
type LoopbackManager struct {
	mu        sync.Mutex
	stateRoot string
	listen    func(string, string) (net.Listener, error)
}

func NewLoopback(stateRoot string) (*LoopbackManager, error) {
	if !filepath.IsAbs(stateRoot) {
		return nil, errors.New("absolute loopback network state root is required")
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return nil, fmt.Errorf("initialize loopback network state: %w", err)
	}
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		return nil, fmt.Errorf("restrict loopback network state: %w", err)
	}
	return &LoopbackManager{stateRoot: stateRoot, listen: net.Listen}, nil
}

func (m *LoopbackManager) Allocate(_ context.Context, sandboxID string, policy model.NetworkConfiguration) (Allocation, error) {
	if !safeID(sandboxID) || policy.Mode != "netstack" {
		return Allocation{}, errors.New("safe sandbox ID and netstack mode are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, err := m.loadLoopback(sandboxID); err == nil {
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Allocation{}, err
	}

	first, err := m.listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Allocation{}, fmt.Errorf("reserve rootless supervisor port: %w", err)
	}
	defer first.Close()
	second, err := m.listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Allocation{}, fmt.Errorf("reserve rootless inspector port: %w", err)
	}
	defer second.Close()
	allocation := Allocation{
		SandboxID:      sandboxID,
		NetworkName:    "rootless-host",
		IPs:            []string{"127.0.0.1"},
		SupervisorPort: first.Addr().(*net.TCPAddr).Port,
		InspectorPort:  second.Addr().(*net.TCPAddr).Port,
	}
	if allocation.SupervisorPort == allocation.InspectorPort {
		return Allocation{}, errors.New("rootless control-port allocation collided")
	}
	if err := m.saveLoopback(allocation); err != nil {
		return Allocation{}, err
	}
	return allocation, nil
}

func (m *LoopbackManager) Check(_ context.Context, sandboxID string) error {
	if !safeID(sandboxID) {
		return errors.New("safe sandbox ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	allocation, err := m.loadLoopback(sandboxID)
	if err != nil {
		return err
	}
	if len(allocation.IPs) != 1 || allocation.IPs[0] != "127.0.0.1" || allocation.NetworkName != "rootless-host" || allocation.SupervisorPort < 1 || allocation.InspectorPort < 1 || allocation.SupervisorPort == allocation.InspectorPort {
		return errors.New("rootless loopback allocation is invalid")
	}
	return nil
}

func (m *LoopbackManager) Release(_ context.Context, sandboxID string) error {
	if !safeID(sandboxID) {
		return errors.New("safe sandbox ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.Remove(m.loopbackPath(sandboxID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove rootless loopback allocation: %w", err)
	}
	return nil
}

func (m *LoopbackManager) saveLoopback(allocation Allocation) error {
	data, err := json.MarshalIndent(allocation, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(m.stateRoot, ".loopback-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(append(data, '\n'))
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write rootless loopback allocation: %w", err)
	}
	if err := os.Rename(name, m.loopbackPath(allocation.SandboxID)); err != nil {
		return fmt.Errorf("replace rootless loopback allocation: %w", err)
	}
	return nil
}

func (m *LoopbackManager) loadLoopback(sandboxID string) (Allocation, error) {
	file, err := os.Open(m.loopbackPath(sandboxID))
	if err != nil {
		return Allocation{}, err
	}
	defer file.Close()
	var allocation Allocation
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&allocation); err != nil {
		return Allocation{}, fmt.Errorf("decode rootless loopback allocation: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Allocation{}, errors.New("decode rootless loopback allocation: trailing data")
	}
	if allocation.SandboxID != sandboxID {
		return Allocation{}, errors.New("rootless loopback allocation identity mismatch")
	}
	return allocation, nil
}

func (m *LoopbackManager) loopbackPath(sandboxID string) string {
	return filepath.Join(m.stateRoot, sandboxID+".json")
}
