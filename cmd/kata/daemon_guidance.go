package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kata/internal/client"
)

// daemonRecoveryAdvice presents the evidence retained by discovery and the
// recovery commands that fit the launch mode.
func daemonRecoveryAdvice(e *client.LocalDaemonUnreachableError, goos string) string {
	advice := fmt.Sprintf("inspect \"%s\" or the supervisor stderr log", filepath.Join(e.DataDir, "daemon.log"))
	if e.WebOrigin != "" {
		advice += ". If responsive, capture " + e.WebOrigin + "/debug/pprof/goroutine?debug=2 with daemon-owner access. With configured authentication use the owner's credential"
	}
	if e.VerifiedProcess && goos == "windows" {
		advice += fmt.Sprintf(". Stop the failed process with taskkill /PID %d /F", e.PID)
	} else if e.VerifiedProcess {
		advice += fmt.Sprintf(". As a fallback, kill -QUIT %d dumps goroutines to stderr and terminates the daemon", e.PID)
	} else {
		advice += ". Verify the process identity before stopping it for recovery"
	}
	advice += ". After it exits, relaunch a supervised or foreground daemon through its supervisor or original foreground launch command"
	if e.Profile != "" {
		advice += ". For a named profile you normally start with Kata, run " + profileRecoveryCommand(e.Profile, goos)
	} else {
		advice += ". For an auto-started current home, run kata daemon restart with the original KATA_HOME and storage environment"
	}
	return advice + "."
}

// profileRecoveryCommand quotes any catalog name, which is free-form text, for
// the shell that will run the command.
func profileRecoveryCommand(name, goos string) string {
	arg := "--daemon=" + name
	if goos != "windows" {
		return "kata " + shellQuoteArg(arg) + " daemon recover"
	}
	if strings.IndexFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("_.-", r)
	}) < 0 {
		return "kata " + arg + " daemon recover"
	}
	return "in PowerShell: kata '" + strings.ReplaceAll(arg, "'", "''") + "' daemon recover"
}

func localDaemonGuidance(e *client.LocalDaemonUnreachableError) string {
	return daemonRecoveryAdvice(e, runtime.GOOS)
}

// diagnosisMessage keeps an unreachable daemon's PID, endpoint and cause in the
// text users read; other states report only their next action.
func diagnosisMessage(result daemonDiagnosis) string {
	if result.State == "local_unreachable" && result.Message != "" {
		return result.State + ": " + result.Message + ". " + result.NextAction
	}
	return result.State + ": " + result.NextAction
}
