// Package network owns CNI-backed network namespaces for gVisor sandboxes.
package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/types"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"

	"the8020/kernel/sandbox/model"
)

type CNI interface {
	AddNetworkList(context.Context, *libcni.NetworkConfigList, *libcni.RuntimeConf) (types.Result, error)
	CheckNetworkList(context.Context, *libcni.NetworkConfigList, *libcni.RuntimeConf) error
	DelNetworkList(context.Context, *libcni.NetworkConfigList, *libcni.RuntimeConf) error
}

type CommandRunner interface {
	Run(context.Context, string, ...string) error
}

type Firewall interface {
	Apply(context.Context, Allocation, model.NetworkConfiguration) error
	Remove(context.Context, Allocation) error
}

type Config struct {
	InstanceUUID string
	PluginPaths  []string
	ConfigPath   string
	NetworkName  string
	Bridge       string
	Subnet       string
	CacheDir     string
	StateRoot    string
	NetNSRoot    string
	CNI          CNI
	Commands     CommandRunner
	Firewall     Firewall
}

type Allocation struct {
	RuntimeGroupID string   `json:"runtime_group_id"`
	ContainerID    string   `json:"container_id"`
	NetworkName    string   `json:"network_name"`
	NamespaceName  string   `json:"namespace_name,omitempty"`
	NamespacePath  string   `json:"namespace_path,omitempty"`
	InterfaceName  string   `json:"interface_name,omitempty"`
	IPs            []string `json:"ips"`
	SupervisorPort int      `json:"supervisor_port"`
	InspectorPort  int      `json:"inspector_port"`
}

type Manager struct {
	mu            sync.Mutex
	instanceUUID  string
	configuration *libcni.NetworkConfigList
	stateRoot     string
	netNSRoot     string
	cni           CNI
	commands      CommandRunner
	firewall      Firewall
}

type execCommands struct{}

func (execCommands) Run(ctx context.Context, name string, arguments ...string) error {
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func New(config Config) (*Manager, error) {
	if config.InstanceUUID == "" || len(config.PluginPaths) == 0 || config.ConfigPath == "" || config.CacheDir == "" || config.StateRoot == "" {
		return nil, errors.New("instance UUID, CNI plugin/config/cache paths, and network state root are required")
	}
	if config.NetNSRoot == "" {
		config.NetNSRoot = "/var/run/netns"
	}
	for _, directory := range []string{config.CacheDir, config.StateRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("initialize network directory: %w", err)
		}
	}
	configuration, err := configuredNetwork(config.ConfigPath, config.NetworkName, config.Bridge, config.Subnet)
	if err != nil {
		return nil, fmt.Errorf("load CNI configuration: %w", err)
	}
	if config.CNI == nil {
		config.CNI = libcni.NewCNIConfigWithCacheDir(config.PluginPaths, config.CacheDir, nil)
	}
	if config.Commands == nil {
		config.Commands = execCommands{}
	}
	if config.Firewall == nil {
		return nil, errors.New("firewall implementation is required")
	}
	return &Manager{instanceUUID: config.InstanceUUID, configuration: configuration, stateRoot: config.StateRoot, netNSRoot: config.NetNSRoot, cni: config.CNI, commands: config.Commands, firewall: config.Firewall}, nil
}

func (m *Manager) Allocate(ctx context.Context, runtimeGroupID, containerID string, policy model.NetworkConfiguration) (allocation Allocation, returnError error) {
	if !safeID(runtimeGroupID) || !safeID(containerID) || policy.Mode != "netstack" {
		return allocation, errors.New("safe runtime-group/container IDs and netstack mode are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, err := m.load(runtimeGroupID); err == nil {
		return existing, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return allocation, err
	}
	configuration := m.configuration
	namespaceName := namespaceName(m.instanceUUID, runtimeGroupID)
	allocation = Allocation{
		RuntimeGroupID: runtimeGroupID, ContainerID: containerID, NetworkName: configuration.Name,
		NamespaceName: namespaceName, NamespacePath: filepath.Join(m.netNSRoot, namespaceName), InterfaceName: "eth0",
		SupervisorPort: model.DefaultSupervisorPort, InspectorPort: model.DefaultInspectorPort,
	}
	if err := m.commands.Run(ctx, "ip", "netns", "add", namespaceName); err != nil {
		return Allocation{}, fmt.Errorf("create network namespace: %w", err)
	}
	namespaceCreated := true
	cniAdded := false
	firewallAdded := false
	defer func() {
		if returnError == nil {
			return
		}
		if firewallAdded {
			_ = m.firewall.Remove(context.Background(), allocation)
		}
		if cniAdded {
			_ = m.cni.DelNetworkList(context.Background(), configuration, runtimeConfig(allocation))
		}
		if namespaceCreated {
			_ = m.commands.Run(context.Background(), "ip", "netns", "delete", namespaceName)
		}
	}()
	result, err := m.cni.AddNetworkList(ctx, configuration, runtimeConfig(allocation))
	if err != nil {
		return Allocation{}, fmt.Errorf("attach CNI network: %w", err)
	}
	cniAdded = true
	allocation.IPs, err = resultIPs(result)
	if err != nil {
		return Allocation{}, err
	}
	if err := m.firewall.Apply(ctx, allocation, policy); err != nil {
		return Allocation{}, fmt.Errorf("apply sandbox firewall: %w", err)
	}
	firewallAdded = true
	if err := m.save(allocation); err != nil {
		return Allocation{}, err
	}
	return allocation, nil
}

func (m *Manager) Check(ctx context.Context, runtimeGroupID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	allocation, err := m.load(runtimeGroupID)
	if err != nil {
		return err
	}
	return m.cni.CheckNetworkList(ctx, m.configuration, runtimeConfig(allocation))
}

func (m *Manager) Release(ctx context.Context, runtimeGroupID string) error {
	if !safeID(runtimeGroupID) {
		return errors.New("safe runtime-group ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	allocation, err := m.load(runtimeGroupID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	configuration := m.configuration
	var joined error
	if err := m.firewall.Remove(ctx, allocation); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := m.cni.DelNetworkList(ctx, configuration, runtimeConfig(allocation)); err != nil {
		joined = errors.Join(joined, err)
	}
	if err := m.commands.Run(ctx, "ip", "netns", "delete", allocation.NamespaceName); err != nil {
		joined = errors.Join(joined, err)
	}
	if joined != nil {
		return joined
	}
	if err := os.Remove(m.recordPath(runtimeGroupID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func configuredNetwork(path, name, bridge, subnet string) (*libcni.NetworkConfigList, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if name != "" {
		if !safeID(name) {
			return nil, errors.New("CNI network name is unsafe")
		}
		raw["name"] = name
	}
	if bridge != "" && (!safeID(bridge) || len(bridge) > 15) {
		return nil, errors.New("CNI bridge must be a safe Linux interface name of at most 15 characters")
	}
	var subnetValue *net.IPNet
	if subnet != "" {
		ip, network, parseErr := net.ParseCIDR(subnet)
		if parseErr != nil || ip.To4() == nil || network.IP.To4() == nil {
			return nil, errors.New("CNI subnet must be a valid IPv4 CIDR")
		}
		subnetValue = network
	}
	plugins, ok := raw["plugins"].([]any)
	if !ok || len(plugins) == 0 {
		return nil, errors.New("CNI configuration has no plugins")
	}
	foundBridge := false
	for _, item := range plugins {
		plugin, valid := item.(map[string]any)
		if !valid || plugin["type"] != "bridge" {
			continue
		}
		foundBridge = true
		if bridge != "" {
			plugin["bridge"] = bridge
		}
		if subnetValue != nil {
			gateway := append(net.IP(nil), subnetValue.IP.To4()...)
			for index := len(gateway) - 1; index >= 0; index-- {
				gateway[index]++
				if gateway[index] != 0 {
					break
				}
			}
			ipam, valid := plugin["ipam"].(map[string]any)
			if !valid {
				ipam = map[string]any{"type": "host-local"}
				plugin["ipam"] = ipam
			}
			ipam["ranges"] = []any{[]any{map[string]any{"subnet": subnetValue.String(), "gateway": gateway.String()}}}
		}
	}
	if !foundBridge {
		return nil, errors.New("CNI configuration has no bridge plugin")
	}
	configured, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return libcni.ConfListFromBytes(configured)
}

func runtimeConfig(allocation Allocation) *libcni.RuntimeConf {
	return &libcni.RuntimeConf{ContainerID: allocation.ContainerID, NetNS: allocation.NamespacePath, IfName: allocation.InterfaceName, Args: [][2]string{{"IgnoreUnknown", "1"}, {"K8S_POD_NAME", allocation.RuntimeGroupID}}}
}

func resultIPs(result types.Result) ([]string, error) {
	converted, err := cniv1.NewResultFromResult(result)
	if err != nil {
		return nil, fmt.Errorf("convert CNI result: %w", err)
	}
	values := make([]string, 0, len(converted.IPs))
	for _, configuration := range converted.IPs {
		if configuration == nil || configuration.Address.IP == nil {
			continue
		}
		values = append(values, configuration.Address.IP.String())
	}
	if len(values) == 0 {
		return nil, errors.New("CNI did not assign a sandbox IP")
	}
	sort.Strings(values)
	return values, nil
}

func (m *Manager) save(allocation Allocation) error {
	data, err := json.MarshalIndent(allocation, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(m.stateRoot, ".network-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, m.recordPath(allocation.RuntimeGroupID))
}

func (m *Manager) load(runtimeGroupID string) (Allocation, error) {
	var allocation Allocation
	data, err := os.ReadFile(m.recordPath(runtimeGroupID))
	if err != nil {
		return allocation, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&allocation); err != nil {
		return allocation, err
	}
	if allocation.RuntimeGroupID != runtimeGroupID || !safeID(allocation.ContainerID) || allocation.NamespacePath != filepath.Join(m.netNSRoot, allocation.NamespaceName) {
		return Allocation{}, errors.New("network allocation identity mismatch")
	}
	return allocation, nil
}

func (m *Manager) recordPath(runtimeGroupID string) string {
	return filepath.Join(m.stateRoot, runtimeGroupID+".json")
}

func namespaceName(instanceUUID, runtimeGroupID string) string {
	value := "pl-" + compactID(instanceUUID) + "-" + compactID(runtimeGroupID)
	if len(value) > 63 {
		value = value[:63]
	}
	return value
}

func compactID(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(value) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func safeID(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func firstIPv4(ips []string) net.IP {
	for _, value := range ips {
		if ip := net.ParseIP(value); ip != nil && ip.To4() != nil {
			return ip.To4()
		}
	}
	return nil
}
