// Package daemonadvice formats recovery instructions for Kata's user interfaces.
package daemonadvice

import (
	"errors"
	"runtime"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kit/agenthook"
)

// Recovery returns the recovery advice with a shell-quoted profile command.
func Recovery(e *client.LocalDaemonUnreachableError) string {
	advice := e.RecoveryAdvice()
	if e.Profile == "" || !e.ProfileAutostart || e.ManagedExternally || !e.VerifiedProcess || advice == "" || errors.Is(e, client.ErrSQLiteMigrationActive) {
		return advice
	}
	commands, _ := agenthook.BuildCommand("kata", "--daemon", e.Profile, "daemon", "recover")
	if runtime.GOOS == "windows" {
		return advice + ". Then run in PowerShell " + commands.PowerShell
	}
	return advice + ". Then run " + commands.POSIX
}
