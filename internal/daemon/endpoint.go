package daemon

import (
	"fmt"
	"net"

	kitdaemon "go.kenn.io/kit/daemon"
)

// AutoStartMarkerEnv names the environment variable set on implicitly spawned
// daemon processes so hosted $PORT detection can be skipped.
const AutoStartMarkerEnv = "KATA_AUTOSTART"

// ValidateNonPublicAddress applies kata's daemon listen policy: loopback,
// RFC1918, CGNAT, link-local, ULA, and wildcard binds are accepted; public
// addresses and hostnames are rejected.
func ValidateNonPublicAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("address %q is not a literal IP (resolve hostnames before calling)", addr)
	}
	if ip.IsUnspecified() {
		return nil
	}
	if err := kitdaemon.RequireNonPublic(addr); err != nil {
		return fmt.Errorf("address %q is non-public: use a private address (loopback, RFC1918, CGNAT, link-local, ULA)", addr)
	}
	return nil
}
