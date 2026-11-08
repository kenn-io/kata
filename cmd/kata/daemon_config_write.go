package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kit/atomicfile"
)

// readDaemonConfig snapshots config.toml before a command edits it.
// A dangling symlink reads as absent, but writing through it would create its target.
func readDaemonConfig(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path) // #nosec G304 G703 -- read the operator-selected KATA_HOME configuration
	if !errors.Is(err, os.ErrNotExist) {
		return data, err == nil, err
	}
	if _, err := os.Lstat(path); err == nil { // #nosec G703 -- inspect the same operator-selected configuration path
		return nil, false, fmt.Errorf("configuration %q is a symlink to a missing file; create its target or remove the link, then rerun", path)
	}
	return nil, false, nil
}

// writeDaemonConfigAndRestart replaces the snapshot readDaemonConfig returned with proposed and restarts
// this home's running daemons. It refuses when the file changed since the snapshot, and restores the
// snapshot when a restart fails.
func writeDaemonConfigAndRestart(ctx context.Context, stderr io.Writer, path string, original []byte, originalExists bool, proposed []byte) error {
	ns, err := daemonruntime.NewNamespace()
	if err != nil {
		return err
	}
	running, release, err := lockLiveDaemons(ctx, ns, stderr)
	if err != nil {
		return err
	}
	defer release()
	if err := requireRestartable(running); err != nil {
		return err
	}
	latest, err := os.ReadFile(path) // #nosec G304 -- same config snapshot read before setup
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read configuration before writing: %w", err)
	}
	if (err == nil) != originalExists || !bytes.Equal(latest, original) {
		return fmt.Errorf("configuration %q changed during setup; rerun the command", path)
	}
	if !bytes.Equal(original, proposed) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := atomicfile.WriteFile(path, proposed, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink()); err != nil {
			return err
		}
	}
	if _, _, err := restartRunningDaemons(ctx, ns, running, nil); err != nil {
		if !bytes.Equal(original, proposed) {
			latest, readErr := os.ReadFile(path) // #nosec G304 -- compare the snapshot before restoring it
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return errors.Join(err, readErr)
			}
			if readErr == nil && bytes.Equal(latest, proposed) {
				var restoreErr error
				if originalExists {
					restoreErr = atomicfile.WriteFile(path, original, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink())
				} else {
					restoreErr = os.Remove(path)
				}
				if restoreErr != nil {
					return errors.Join(err, restoreErr)
				}
				return fmt.Errorf("%w; configuration restored", err)
			}
		}
		return err
	}
	_, err = fmt.Fprintln(stderr, "Service environment settings can override this configuration.")
	return err
}
