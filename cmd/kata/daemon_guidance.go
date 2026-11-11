package main

import (
	"fmt"
	"path/filepath"
	"runtime"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kit/agenthook"
)

// daemonRecoveryAdvice presents the evidence retained by discovery and the
// recovery commands that fit the launch mode.
func daemonRecoveryAdvice(e *client.LocalDaemonUnreachableError, profile, goos string) string {
	advice := fmt.Sprintf("inspect \"%s\" or the supervisor stderr log", filepath.Join(e.DataDir, "daemon.log"))
	if e.WebOrigin != "" {
		advice += ". If responsive, capture " + e.WebOrigin + "/debug/pprof/goroutine?debug=2 with daemon-owner access; configured authentication requires the owner's credential"
	}
	if e.VerifiedProcess && goos == "windows" {
		advice += fmt.Sprintf(". Stop the recorded process with taskkill /PID %d /F", e.PID)
	} else if e.VerifiedProcess {
		advice += fmt.Sprintf(". Fallback: kill -QUIT %d dumps goroutines to stderr and terminates the daemon", e.PID)
	} else {
		advice += ". Verify the process identity before stopping it for recovery"
	}
	advice += ". After the process exits, relaunch a supervised or foreground daemon through its supervisor or original foreground launch command"
	if profile != "" {
		advice += ". After the process exits, for a named profile normally started with Kata, run " + profileRecoveryCommand(profile, goos)
	} else {
		advice += ". After the process exits, for an auto-started current home, run kata daemon restart with the original KATA_HOME and storage environment"
	}
	return advice + "."
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

func localDaemonGuidance(e *client.LocalDaemonUnreachableError, profile string) string {
	return daemonRecoveryAdvice(e, profile, runtime.GOOS)
}
