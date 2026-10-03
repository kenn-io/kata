package daemon

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// iface binds resolve exactly one up, non-public IPv4 address, or fail closed.
func TestInterfaceListenResolution(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		up                bool
		addresses         []string
		lookupErr         bool
	}{
		{name: "concrete unchanged", input: "127.0.0.1:7777", want: "127.0.0.1:7777"},
		{name: "up overlay", input: "iface:overlay0:7777", up: true, addresses: []string{"100.64.0.5/32", "fd00::1/128"}, want: "100.64.0.5:7777"},
		{name: "loopback", input: "iface:lo:7777", up: true, addresses: []string{"127.0.0.1/8"}, want: "127.0.0.1:7777"},
		{name: "absent", input: "iface:absent0:7777", lookupErr: true},
		{name: "down", input: "iface:overlay0:7777", addresses: []string{"100.64.0.5/32"}},
		{name: "ipv6 only", input: "iface:overlay0:7777", up: true, addresses: []string{"fd00::1/128"}},
		{name: "public only", input: "iface:overlay0:7777", up: true, addresses: []string{"8.8.8.8/32"}},
		{name: "ambiguous", input: "iface:overlay0:7777", up: true, addresses: []string{"100.64.0.5/32", "100.64.0.6/32"}},
		{name: "empty name", input: "iface::7777"},
		{name: "zero port", input: "iface:overlay0:0"},
		{name: "invalid port", input: "iface:overlay0:65536"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveInterfaceListen(tc.input, func(string) (net.Flags, []net.Addr, error) {
				if tc.lookupErr {
					return 0, nil, errors.New("interface unavailable")
				}
				var flags net.Flags
				if tc.up {
					flags = net.FlagUp
				}
				var addresses []net.Addr
				for _, addr := range tc.addresses {
					ip, network, err := net.ParseCIDR(addr)
					require.NoError(t, err)
					network.IP = ip
					addresses = append(addresses, network)
				}
				return flags, addresses, nil
			})
			if tc.want == "" {
				require.Error(t, err)
				require.Empty(t, got)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestInterfaceListenRealLoopback(t *testing.T) {
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 {
			got, err := ResolveInterfaceListen("iface:" + iface.Name + ":7777")
			require.NoError(t, err)
			host, port, err := net.SplitHostPort(got)
			require.NoError(t, err)
			require.True(t, net.ParseIP(host).IsLoopback())
			require.Equal(t, "7777", port)
			return
		}
	}
	t.Skip("no up loopback interface")
}
