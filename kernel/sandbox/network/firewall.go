package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"the8020/kernel/sandbox/model"
)

type NFTRunner interface {
	Run(context.Context, []byte, ...string) error
}

type nftExec struct{}

func (nftExec) Run(ctx context.Context, input []byte, arguments ...string) error {
	command := exec.CommandContext(ctx, "nft", arguments...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

type NFTFirewall struct {
	instanceUUID string
	runner       NFTRunner
	resolve      func(context.Context, string) ([]net.IP, error)
	dnsResolvers []string
	bridgeHost   string
	sandboxNet   *net.IPNet
}

type NFTFirewallConfig struct {
	InstanceUUID  string
	SandboxSubnet string
	Runner        NFTRunner
	Resolve       func(context.Context, string) ([]net.IP, error)
}

func NewNFTFirewall(config NFTFirewallConfig) (*NFTFirewall, error) {
	if config.InstanceUUID == "" {
		return nil, errors.New("instance UUID is required")
	}
	if config.Runner == nil {
		config.Runner = nftExec{}
	}
	resolve := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	}
	if config.Resolve != nil {
		resolve = config.Resolve
	}
	_, sandboxNet, err := net.ParseCIDR(config.SandboxSubnet)
	if err != nil || sandboxNet.IP.To4() == nil {
		return nil, errors.New("sandbox subnet must be a valid IPv4 CIDR")
	}
	bridgeHost := append(net.IP(nil), sandboxNet.IP.To4()...)
	bridgeHost[3]++
	if !sandboxNet.Contains(bridgeHost) {
		return nil, errors.New("sandbox subnet does not contain a bridge address")
	}
	return &NFTFirewall{instanceUUID: config.InstanceUUID, runner: config.Runner, resolve: resolve, dnsResolvers: systemDNSResolvers(), bridgeHost: bridgeHost.String(), sandboxNet: sandboxNet}, nil
}

func (f *NFTFirewall) Apply(ctx context.Context, allocation Allocation, policy model.NetworkConfiguration) error {
	ip := firstIPv4(allocation.IPs)
	if ip == nil {
		return errors.New("nftables egress policy requires an IPv4 sandbox address")
	}
	table := firewallTable(f.instanceUUID, allocation.RuntimeGroupID)
	rules := []string{
		fmt.Sprintf("ip daddr %s ct state established,related counter accept", ip.String()),
		fmt.Sprintf("ip saddr %s ip daddr %s counter accept", f.bridgeHost, ip.String()),
		fmt.Sprintf("ip daddr %s counter drop", ip.String()),
		fmt.Sprintf("ip saddr %s ct state established,related counter accept", ip.String()),
	}
	if policy.EgressEnabled && len(policy.AllowedHosts) == 0 {
		rules = append(rules, fmt.Sprintf("ip saddr %s counter accept", ip.String()))
	} else if policy.EgressEnabled {
		var allowed []firewallTarget
		usesDNS := false
		for _, host := range policy.AllowedHosts {
			targets, resolved, err := f.resolveHost(ctx, host)
			if err != nil {
				return err
			}
			usesDNS = usesDNS || resolved
			allowed = append(allowed, targets...)
		}
		sort.Slice(allowed, func(i, j int) bool {
			return allowed[i].destination+":"+strconv.Itoa(allowed[i].port) < allowed[j].destination+":"+strconv.Itoa(allowed[j].port)
		})
		for _, target := range allowed {
			base := fmt.Sprintf("ip saddr %s ip daddr %s", ip.String(), target.destination)
			if target.port == 0 {
				rules = append(rules, base+" counter accept")
			} else {
				rules = append(rules, fmt.Sprintf("%s tcp dport %d counter accept", base, target.port), fmt.Sprintf("%s udp dport %d counter accept", base, target.port))
			}
		}
		if usesDNS {
			for _, resolver := range f.dnsResolvers {
				base := fmt.Sprintf("ip saddr %s ip daddr %s", ip.String(), resolver)
				rules = append(rules, base+" udp dport 53 counter accept", base+" tcp dport 53 counter accept")
			}
		}
	}
	rules = append(rules, fmt.Sprintf("ip saddr %s counter drop", ip.String()))
	var script strings.Builder
	fmt.Fprintf(&script, "table inet %s {\n  chain forward {\n    type filter hook forward priority 0; policy accept;\n", table)
	for _, rule := range rules {
		fmt.Fprintf(&script, "    %s\n", rule)
	}
	script.WriteString("  }\n}\n")
	return f.runner.Run(ctx, []byte(script.String()), "-f", "-")
}

type firewallTarget struct {
	destination string
	port        int
}

func (f *NFTFirewall) resolveHost(ctx context.Context, value string) ([]firewallTarget, bool, error) {
	if _, network, err := net.ParseCIDR(value); err == nil {
		if network.IP.To4() == nil {
			return nil, false, fmt.Errorf("allowed egress network %q must be IPv4", value)
		}
		if networksOverlap(network, f.sandboxNet) {
			return nil, false, fmt.Errorf("allowed egress network %q overlaps the sandbox subnet", value)
		}
		return []firewallTarget{{destination: network.String()}}, false, nil
	}
	if parsed := net.ParseIP(value); parsed != nil {
		if parsed.To4() == nil {
			return nil, false, fmt.Errorf("allowed egress address %q must be IPv4", value)
		}
		if f.sandboxNet.Contains(parsed) {
			return nil, false, fmt.Errorf("allowed egress address %q belongs to the sandbox subnet", value)
		}
		return []firewallTarget{{destination: parsed.String()}}, false, nil
	}
	host, port := value, 0
	if splitHost, splitPort, err := net.SplitHostPort(value); err == nil {
		host = splitHost
		parsedPort, parseErr := strconv.Atoi(splitPort)
		if parseErr != nil || parsedPort < 1 || parsedPort > 65535 {
			return nil, false, fmt.Errorf("allowed egress host %q has an invalid port", value)
		}
		port = parsedPort
	}
	if host == "" || strings.ContainsAny(host, "/?#@") {
		return nil, false, fmt.Errorf("allowed egress host %q is invalid", value)
	}
	addresses, err := f.resolve(ctx, host)
	if err != nil {
		return nil, false, fmt.Errorf("resolve allowed egress host %q: %w", host, err)
	}
	seen := map[string]bool{}
	var targets []firewallTarget
	for _, address := range addresses {
		if ipv4 := address.To4(); ipv4 != nil && f.sandboxNet.Contains(ipv4) {
			return nil, false, fmt.Errorf("allowed egress host %q resolves inside the sandbox subnet", host)
		} else if ipv4 != nil && !seen[ipv4.String()] {
			seen[ipv4.String()] = true
			targets = append(targets, firewallTarget{destination: ipv4.String(), port: port})
		}
	}
	if len(targets) == 0 {
		return nil, false, fmt.Errorf("allowed egress host %q has no IPv4 address", host)
	}
	return targets, true, nil
}

func networksOverlap(left, right *net.IPNet) bool {
	return left.Contains(right.IP) || right.Contains(left.IP)
}

func systemDNSResolvers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var result []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "nameserver" {
			continue
		}
		if address := net.ParseIP(fields[1]); address != nil && address.To4() != nil && !seen[address.String()] {
			seen[address.String()] = true
			result = append(result, address.String())
		}
	}
	sort.Strings(result)
	return result
}

func (f *NFTFirewall) Remove(ctx context.Context, allocation Allocation) error {
	table := firewallTable(f.instanceUUID, allocation.RuntimeGroupID)
	err := f.runner.Run(ctx, nil, "delete", "table", "inet", table)
	if err != nil && strings.Contains(err.Error(), "No such file or directory") {
		return nil
	}
	return err
}

func firewallTable(instanceUUID, runtimeGroupID string) string {
	value := "pl_" + compactID(instanceUUID) + "_" + compactID(runtimeGroupID)
	if len(value) > 60 {
		value = value[:60]
	}
	return value
}
