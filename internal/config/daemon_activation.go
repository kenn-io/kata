package config

import (
	"bytes"
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

// ReadSocketActivationForHome reads only lifecycle ownership. Unrelated auth,
// connector and storage settings must not prevent a safe refusal to spawn.
func ReadSocketActivationForHome(ctx context.Context, home string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
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
		// Process-control fast paths historically ignore malformed unrelated
		// settings. A possible activation declaration must still fail closed,
		// including when its TOML key is quoted or escaped.
		for line := range bytes.Lines(data) {
			key, _, assignment := bytes.Cut(line, []byte("="))
			if !assignment {
				continue
			}
			var keys map[string]any
			if _, keyErr := toml.Decode(string(key)+" = true", &keys); keyErr == nil {
				if _, declared := keys["socket_activation"]; declared {
					return "", fmt.Errorf("read socket_activation: %w", err)
				}
			}
		}
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
