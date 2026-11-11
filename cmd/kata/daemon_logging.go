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
func startDaemonLogging(_ context.Context) (io.Writer, io.Writer, func()) {
	ns, err := daemonruntime.NewNamespace()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kata daemon: cannot resolve daemon.log: %v\n", err)
		return os.Stderr, nil, func() {}
	}
	file := client.DaemonLogWriter(ns.DataDir)
	if file == nil {
		fmt.Fprintln(os.Stderr, "kata daemon: cannot open daemon.log in "+ns.DataDir+". Diagnostics remain on stderr")
		return os.Stderr, nil, func() {}
	}
	var writer io.Writer = daemonDiagnosticWriter{os.Stderr, file}
	var errorFile io.Writer = file
	stderrInfo, stderrErr := os.Stderr.Stat()
	fileInfo, fileErr := file.Stat()
	sameFile := stderrErr == nil && fileErr == nil && os.SameFile(stderrInfo, fileInfo)
	if !sameFile && stderrErr == nil {
		nullInfo, err := os.Stat(os.DevNull)
		if err == nil && os.SameFile(stderrInfo, nullInfo) && redirectDaemonStderr(file) == nil {
			sameFile = true
		}
	}
	if sameFile {
		writer = os.Stderr
		errorFile = nil
	} else if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
		fmt.Fprintf(writer, "kata daemon: cannot retain fatal diagnostics: %v\n", err)
	}
	previousOutput := log.Writer()
	// The default slog handler uses the standard logger too. Keeping it preserves
	// the existing diagnostic format and any provider-specific derived loggers.
	log.SetOutput(writer)
	return writer, errorFile, func() {
		log.SetOutput(previousOutput)
		if !sameFile {
			_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		}
		_ = file.Close()
	}
}

// Each destination is attempted even when the other one fails.
type daemonDiagnosticWriter struct{ stderr, file io.Writer }

func (w daemonDiagnosticWriter) Write(p []byte) (int, error) {
	fileN, fileErr := w.file.Write(p)
	n, err := w.stderr.Write(p)
	if err != nil {
		return n, err
	}
	if fileErr != nil {
		return fileN, fileErr
	}
	return n, nil
}
