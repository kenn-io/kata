package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kit/agenthook"
)

func daemonRecoveryAdvice(err error, goos string) string {
	e, ok := errors.AsType[*client.LocalDaemonUnreachableError](err)
	if !ok {
		return ""
	}
	advice := fmt.Sprintf("Check for logs at %q", filepath.Join(e.DataDir, "daemon.log"))
	if e.WebOrigin != "" {
		advice += ". Capture " + e.WebOrigin + "/debug/pprof/goroutine?debug=2; if configured, send the daemon token in the Authorization: Bearer header"
	}
	if errors.Is(err, client.ErrSQLiteMigrationActive) {
		return advice + ". Wait for the migration to finish, then retry."
	}
	if errors.Is(err, context.Canceled) {
		return advice
	}
	if e.ManagedExternally || errors.Is(err, client.ErrDaemonManagedExternally) {
		return advice + ". Restart it through its service manager."
	}
	if e.VerifiedProcess {
		if goos == "windows" {
			advice += fmt.Sprintf(". Stop it with taskkill /PID %d /F", e.PID)
		} else {
			advice += fmt.Sprintf(". Stop it with kill -QUIT %d", e.PID)
		}
	}
	if e.Profile != "" {
		return advice + ". Then run " + profileRecoveryCommand(e.Profile, goos) + "."
	}
	return advice + ". Then run kata daemon restart for the selected home."
}

func profileRecoveryCommand(name, goos string) string {
	commands, _ := agenthook.BuildCommand("kata", "--daemon", name, "daemon", "recover")
	if goos == "windows" {
		return "in PowerShell " + commands.PowerShell
	}
	return commands.POSIX
}
