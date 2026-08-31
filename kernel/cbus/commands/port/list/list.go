// Package list implements port.list.
package list

import (
	"context"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

type summary struct {
	LeaseID      string `json:"lease_id"`
	Protocol     string `json:"protocol"`
	State        string `json:"state"`
	BindAddress  string `json:"bind_address"`
	HostPort     int    `json:"host_port"`
	SandboxID    string `json:"sandbox_id"`
	InternalPort int    `json:"internal_port"`
	Purpose      string `json:"purpose"`
	ExpiresAt    string `json:"expires_at,omitempty"`
}

func New(serviceSet *services.Services) core.Handler {
	return func(_ context.Context, _ core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Ports == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "port manager is unavailable")
		}
		leases := runtimeServices.Ports.List()
		items := make([]summary, 0, len(leases))
		for _, lease := range leases {
			expiresAt := ""
			if !lease.ExpiresAt.IsZero() {
				expiresAt = lease.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
			}
			items = append(items, summary{
				LeaseID: lease.LeaseID, Protocol: lease.Protocol, State: lease.State,
				BindAddress: lease.BindAddress, HostPort: lease.HostPort,
				SandboxID: lease.SandboxID, InternalPort: lease.InternalPort,
				Purpose: lease.Purpose, ExpiresAt: expiresAt,
			})
		}
		return core.Result{"ports": items}, nil
	}
}
