package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemonruntime"
)

// startDaemonLogging owns the process-wide diagnostic sink until shutdown and
// restart reporting finish. Detached launches already send stderr to this file.
func startDaemonLogging(ctx context.Context) (io.Writer, func()) {
	ns, err := daemonruntime.NewNamespace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kata daemon: cannot resolve daemon.log: %v\n", err)
		return os.Stderr, func() {}
	}
	file := client.DaemonLogWriter(ns.DataDir)
	if file == nil {
		fmt.Fprintln(os.Stderr, "kata daemon: cannot open daemon.log in "+ns.DataDir+"; diagnostics remain on stderr")
		return os.Stderr, func() {}
	}
	var writer io.Writer = io.MultiWriter(os.Stderr, file)
	stderrInfo, stderrErr := os.Stderr.Stat()
	fileInfo, fileErr := file.Stat()
	sameFile := stderrErr == nil && fileErr == nil && os.SameFile(stderrInfo, fileInfo)
	if sameFile {
		writer = os.Stderr
	} else if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		fmt.Fprintf(writer, "kata daemon: cannot retain fatal diagnostics: %v\n", err)
	}
	previousOutput := log.Writer()
	// The default slog handler uses the standard logger too. Keeping it preserves
	// the existing diagnostic format and any provider-specific derived loggers.
	log.SetOutput(writer)
	return writer, func() {
		log.SetOutput(previousOutput)
		if !sameFile {
			_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		}
		_ = file.Close()
	}
}
