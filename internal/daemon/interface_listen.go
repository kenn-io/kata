package daemon

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ResolveInterfaceListen resolves iface:<name>:<port> to one concrete private
// IPv4 bind. It never falls back to another interface or a wildcard listener.
func ResolveInterfaceListen(listen string) (string, error) {
	return resolveInterfaceListen(listen, func(name string) (net.Flags, []net.Addr, error) {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return 0, nil, err
		}
		addresses, err := iface.Addrs()
		return iface.Flags, addresses, err
	})
}

func resolveInterfaceListen(listen string, lookup func(string) (net.Flags, []net.Addr, error)) (string, error) {
	if !strings.HasPrefix(listen, "iface:") {
		return listen, nil
	}
	parts := strings.Split(listen, ":")
	if len(parts) != 3 || parts[1] == "" {
		return "", fmt.Errorf("interface listen must be iface:<name>:<port>")
	}
	port, err := strconv.Atoi(parts[2])
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("interface listen port must be between 1 and 65535")
	}
	flags, addresses, err := lookup(parts[1])
	if err != nil {
		return "", fmt.Errorf("interface %q unavailable: %w", parts[1], err)
	}
	if flags&net.FlagUp == 0 {
		return "", fmt.Errorf("interface %q is down", parts[1])
	}
	candidates := make(map[string]struct{})
	var selected string
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		candidate := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		if ValidateNonPublicAddress(candidate) == nil {
			candidates[candidate] = struct{}{}
			selected = candidate
		}
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("interface %q must have exactly one eligible non-public IPv4 address (found %d)", parts[1], len(candidates))
	}
	return selected, nil
}
