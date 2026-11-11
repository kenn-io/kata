package main

import (
	"fmt"
	"path/filepath"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kit/agenthook"
)

// daemonCaptureAdvice lists the evidence to read without stopping the daemon.
func daemonCaptureAdvice(e *client.LocalDaemonUnreachableError) string {
	advice := fmt.Sprintf("inspect the log file %q, if present, or retained foreground/supervisor stderr", filepath.Join(e.DataDir, "daemon.log"))
	if e.WebOrigin != "" {
		advice += ". If responsive, capture " + e.WebOrigin + "/debug/pprof/goroutine?debug=2 with daemon-owner access, and configured authentication requires the owner's credential"
	}
	return advice
}

// daemonRecoveryAdvice presents the evidence retained by discovery and the
// recovery commands that fit the launch mode.
func daemonRecoveryAdvice(e *client.LocalDaemonUnreachableError, profile, goos string) string {
	advice := "This probe failed, so " + daemonCaptureAdvice(e)
	advice += fmt.Sprintf(". Immediately before using a terminating fallback, verify that PID %d still identifies this daemon and that the selected endpoint remains unresponsive", e.PID)
	if e.VerifiedProcess && goos == "windows" {
		advice += fmt.Sprintf(". If both hold and you choose to stop it, taskkill /PID %d /F stops the recorded process", e.PID)
	} else if e.VerifiedProcess {
		advice += fmt.Sprintf(". If both hold, you choose to stop it, and the daemon's stderr is retained, kill -QUIT %d writes goroutine stacks there and terminates the daemon. If stderr is discarded, the dump is lost", e.PID)
	}
	advice += ". After the process exits, relaunch a supervised or foreground daemon through its supervisor or original foreground launch command. "
	if profile != "" {
		return advice + "For a named profile normally started with Kata, run " + profileRecoveryCommand(profile, goos) + "."
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
