package network

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/types"
	cniv1 "github.com/containernetworking/cni/pkg/types/100"

	"the8020/kernel/sandbox/model"
)

type fakeCNI struct {
	addResult             types.Result
	addError              error
	adds, checks, deletes int
	configuration         *libcni.NetworkConfigList
}

func (f *fakeCNI) AddNetworkList(_ context.Context, configuration *libcni.NetworkConfigList, _ *libcni.RuntimeConf) (types.Result, error) {
	f.adds++
	f.configuration = configuration
	return f.addResult, f.addError
}
func (f *fakeCNI) CheckNetworkList(context.Context, *libcni.NetworkConfigList, *libcni.RuntimeConf) error {
	f.checks++
	return nil
}
func (f *fakeCNI) DelNetworkList(context.Context, *libcni.NetworkConfigList, *libcni.RuntimeConf) error {
	f.deletes++
	return nil
}

type recordedCommand struct {
	name string
	args []string
}
type fakeCommands struct {
	calls []recordedCommand
	err   error
}

func (f *fakeCommands) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, recordedCommand{name: name, args: append([]string(nil), args...)})
	return f.err
}

type fakeFirewall struct {
	applies, removes int
	applyError       error
}

func (f *fakeFirewall) Apply(context.Context, Allocation, model.NetworkConfiguration) error {
	f.applies++
	return f.applyError
}
func (f *fakeFirewall) Remove(context.Context, Allocation) error { f.removes++; return nil }

func TestAllocateCheckReleaseAndPersistence(t *testing.T) {
	root := t.TempDir()
	configuration := writeConfiguration(t, root)
	cni := &fakeCNI{addResult: &cniv1.Result{CNIVersion: "1.1.0", IPs: []*cniv1.IPConfig{{Address: net.IPNet{IP: net.ParseIP("10.88.0.4"), Mask: net.CIDRMask(16, 32)}}}}}
	commands := &fakeCommands{}
	firewall := &fakeFirewall{}
	manager, err := New(Config{InstanceUUID: "instance-one", PluginPaths: []string{filepath.Join(root, "plugins")}, ConfigPath: configuration, NetworkName: "custom-network", Bridge: "custom0", Subnet: "10.99.0.0/24", CacheDir: filepath.Join(root, "cache"), StateRoot: filepath.Join(root, "state"), NetNSRoot: filepath.Join(root, "netns"), CNI: cni, Commands: commands, Firewall: firewall})
	if err != nil {
		t.Fatal(err)
	}
	policy := model.NetworkConfiguration{Mode: "netstack", EgressEnabled: false}
	allocation, err := manager.Allocate(context.Background(), "group-one", "sandbox-one", policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocation.IPs) != 1 || allocation.IPs[0] != "10.88.0.4" || allocation.NetworkName != "custom-network" || cni.adds != 1 || cni.configuration == nil || !strings.Contains(string(cni.configuration.Bytes), `"bridge":"custom0"`) || !strings.Contains(string(cni.configuration.Bytes), `"subnet":"10.99.0.0/24"`) || firewall.applies != 1 || len(commands.calls) != 1 {
		t.Fatalf("allocation=%#v cni=%#v firewall=%#v commands=%#v", allocation, cni, firewall, commands.calls)
	}
	second, err := manager.Allocate(context.Background(), "group-one", "sandbox-one", policy)
	if err != nil || second.NamespaceName != allocation.NamespaceName || cni.adds != 1 {
		t.Fatalf("idempotent allocation=%#v err=%v adds=%d", second, err, cni.adds)
	}
	if err := manager.Check(context.Background(), "group-one"); err != nil || cni.checks != 1 {
		t.Fatalf("check: %v count=%d", err, cni.checks)
	}
	if err := manager.Release(context.Background(), "group-one"); err != nil {
		t.Fatal(err)
	}
	if cni.deletes != 1 || firewall.removes != 1 || len(commands.calls) != 2 || commands.calls[1].args[2] != allocation.NamespaceName {
		t.Fatalf("release cni=%#v firewall=%#v commands=%#v", cni, firewall, commands.calls)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "group-one.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state remains: %v", err)
	}
	if err := manager.Release(context.Background(), "group-one"); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestAllocateRollsBackCNIAndNamespace(t *testing.T) {
	root := t.TempDir()
	cni := &fakeCNI{addResult: &cniv1.Result{CNIVersion: "1.1.0", IPs: []*cniv1.IPConfig{{Address: net.IPNet{IP: net.ParseIP("10.88.0.9"), Mask: net.CIDRMask(16, 32)}}}}}
	commands := &fakeCommands{}
	firewall := &fakeFirewall{applyError: errors.New("nft failed")}
	manager, err := New(Config{InstanceUUID: "instance", PluginPaths: []string{root}, ConfigPath: writeConfiguration(t, root), CacheDir: filepath.Join(root, "cache"), StateRoot: filepath.Join(root, "state"), NetNSRoot: filepath.Join(root, "netns"), CNI: cni, Commands: commands, Firewall: firewall})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Allocate(context.Background(), "group", "sandbox", model.NetworkConfiguration{Mode: "netstack"}); err == nil || !strings.Contains(err.Error(), "firewall") {
		t.Fatalf("allocation error = %v", err)
	}
	if cni.deletes != 1 || len(commands.calls) != 2 || commands.calls[1].args[1] != "delete" {
		t.Fatalf("rollback cni=%#v commands=%#v", cni, commands.calls)
	}
}

type fakeNFTRunner struct {
	input     []byte
	arguments []string
	err       error
}

func (f *fakeNFTRunner) Run(_ context.Context, input []byte, arguments ...string) error {
	f.input = append([]byte(nil), input...)
	f.arguments = append([]string(nil), arguments...)
	return f.err
}

func TestNFTFirewallBuildsRestrictedAndDeniedPolicies(t *testing.T) {
	runner := &fakeNFTRunner{}
	firewall, err := NewNFTFirewall(NFTFirewallConfig{InstanceUUID: "instance-one", SandboxSubnet: "10.88.0.0/16", Runner: runner, Resolve: func(_ context.Context, host string) ([]net.IP, error) {
		if host != "example.com" {
			return nil, errors.New("unknown host")
		}
		return []net.IP{net.ParseIP("203.0.113.8")}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	allocation := Allocation{RuntimeGroupID: "group-one", IPs: []string{"10.88.0.3"}}
	policy := model.NetworkConfiguration{EgressEnabled: true, AllowedHosts: []string{"192.0.2.5", "198.51.100.0/24"}}
	if err := firewall.Apply(context.Background(), allocation, policy); err != nil {
		t.Fatal(err)
	}
	script := string(runner.input)
	for _, expected := range []string{"table inet pl_instanceone_groupone", "ip daddr 10.88.0.3 ct state established,related", "ip saddr 10.88.0.1 ip daddr 10.88.0.3", "ip daddr 10.88.0.3 counter drop", "ip saddr 10.88.0.3 ip daddr 192.0.2.5", "ip daddr 198.51.100.0/24", "ip saddr 10.88.0.3 counter drop"} {
		if !strings.Contains(script, expected) {
			t.Errorf("script missing %q:\n%s", expected, script)
		}
	}
	firewall.dnsResolvers = []string{"192.0.2.53"}
	if err := firewall.Apply(context.Background(), allocation, model.NetworkConfiguration{EgressEnabled: true, AllowedHosts: []string{"example.com:443"}}); err != nil {
		t.Fatal(err)
	}
	script = string(runner.input)
	for _, expected := range []string{"ip daddr 203.0.113.8 tcp dport 443", "ip daddr 192.0.2.53 udp dport 53", "counter drop"} {
		if !strings.Contains(script, expected) {
			t.Errorf("hostname script missing %q:\n%s", expected, script)
		}
	}
	if err := firewall.Apply(context.Background(), allocation, model.NetworkConfiguration{EgressEnabled: true, AllowedHosts: []string{"https://example.com"}}); err == nil {
		t.Fatal("accepted URL as hostname")
	}
	if err := firewall.Apply(context.Background(), allocation, model.NetworkConfiguration{EgressEnabled: true, AllowedHosts: []string{"10.88.0.9"}}); err == nil || !strings.Contains(err.Error(), "sandbox subnet") {
		t.Fatalf("sandbox-to-sandbox address error=%v", err)
	}
	if err := firewall.Apply(context.Background(), allocation, model.NetworkConfiguration{EgressEnabled: true, AllowedHosts: []string{"10.88.0.0/15"}}); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping sandbox network error=%v", err)
	}
	if err := firewall.Apply(context.Background(), allocation, model.NetworkConfiguration{EgressEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if script := string(runner.input); !strings.Contains(script, "ip saddr 10.88.0.3 counter accept") {
		t.Fatalf("unrestricted egress policy was not accepted:\n%s", script)
	}
	if err := firewall.Remove(context.Background(), allocation); err != nil {
		t.Fatal(err)
	}
	if strings.Join(runner.arguments, " ") != "delete table inet pl_instanceone_groupone" {
		t.Fatalf("remove arguments: %#v", runner.arguments)
	}
}

func TestNFTFirewallRequiresValidSandboxSubnet(t *testing.T) {
	if _, err := NewNFTFirewall(NFTFirewallConfig{InstanceUUID: "instance", SandboxSubnet: "invalid", Runner: &fakeNFTRunner{}}); err == nil {
		t.Fatal("accepted invalid sandbox subnet")
	}
}

func writeConfiguration(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "the8020.conflist")
	data := `{"cniVersion":"1.1.0","name":"the8020","plugins":[{"type":"bridge"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
