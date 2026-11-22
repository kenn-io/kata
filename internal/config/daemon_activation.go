package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// ValidateSocketActivation validates the durable manager policy without
// depending on server-side descriptor acquisition or the caller's platform.
func ValidateSocketActivation(mode string) error {
	if mode == "" || mode == "systemd" {
		return nil
	}
	if key, ok := strings.CutPrefix(mode, "launchd:"); ok && key != "" && strings.TrimSpace(key) == key && !strings.ContainsAny(key, "\x00\r\n:") {
		return nil
	}
	return errors.New("socket_activation must be empty, systemd, or launchd:NAME")
}

// ReadSocketActivationForHome reads only lifecycle ownership. A config that
// does not parse reports no manager: process-control fast paths such as stop
// keep working, and the daemon itself refuses to start from that config, so
// no unmanaged replacement can begin. Live managed daemons stay protected by
// their runtime metadata.
func ReadSocketActivationForHome(ctx context.Context, home string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml")) //nolint:gosec // G304: fixed config file in the operator-selected home, consistent with daemon config.
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var policy struct {
		Mode string `toml:"socket_activation"`
	}
	if _, err := toml.Decode(string(data), &policy); err != nil {
		return "", nil
	}
	return policy.Mode, ValidateSocketActivation(policy.Mode)
}

// SocketActivationControlError directs operators to the selected manager,
// without guessing a service unit or launchd job identifier.
func SocketActivationControlError(mode string) error {
	manager, _, _ := strings.Cut(mode, ":")
	return fmt.Errorf("daemon uses socket_activation=%q; use %s to control or restart its service (and its activation socket when stopping completely), not kata daemon start/stop/restart", mode, manager)
}

// RequireUnmanagedDaemonHome refuses process control in a manager-owned home.
func RequireUnmanagedDaemonHome(ctx context.Context, home string) error {
	mode, err := ReadSocketActivationForHome(ctx, home)
	if err != nil {
		return err
	}
	if mode != "" {
		return SocketActivationControlError(mode)
	}
	return nil
}
