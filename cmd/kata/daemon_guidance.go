package main

import (
	"fmt"
	"path/filepath"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kit/agenthook"
)

// daemonRecoveryAdvice presents the evidence retained by discovery and the
// recovery commands that fit the launch mode.
func daemonRecoveryAdvice(e *client.LocalDaemonUnreachableError, profile, goos string) string {
	advice := fmt.Sprintf("Read %q, if present, or the daemon's retained stderr", filepath.Join(e.DataDir, "daemon.log"))
	if e.WebOrigin != "" {
		advice += fmt.Sprintf(". After independently confirming %[1]s is this daemon's browser origin, capture %[1]s/debug/pprof/goroutine?debug=2 with daemon-owner access", e.WebOrigin)
	}
	advice += fmt.Sprintf(". If you choose to stop it, immediately recheck that PID %d is still this daemon and the selected endpoint still fails", e.PID)
	if e.VerifiedProcess && goos == "windows" {
		advice += fmt.Sprintf(". Then taskkill /PID %d /F stops it", e.PID)
	} else if e.VerifiedProcess {
		advice += fmt.Sprintf(". Then kill -QUIT %d stops it and writes goroutine stacks to its stderr, which are lost if stderr is discarded", e.PID)
	}
	advice += ". After it exits, restart a supervised or foreground daemon through its supervisor or original command. "
	if profile != "" {
		return advice + "For a named profile Kata normally starts, run " + profileRecoveryCommand(profile, goos) + "."
	}
	return advice + "For an auto-started current home, run kata daemon restart with the original KATA_HOME and storage environment."
}

// profileRecoveryCommand quotes any catalog name, which is free-form text, for
// the shell that will run the command. Windows output is labeled PowerShell.
func profileRecoveryCommand(name, goos string) string {
	arg := "--daemon=" + name
	if goos != "windows" {
		return "kata " + shellQuoteArg(arg) + " daemon recover"
	}
	commands, _ := agenthook.BuildCommand("kata", arg, "daemon", "recover") // the executable is never blank
	return "in PowerShell " + commands.PowerShell
}
