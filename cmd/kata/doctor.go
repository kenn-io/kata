package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/diagnostics"
	"go.kenn.io/kata/internal/textsafe"
)

const doctorRequestTimeout = 5 * time.Second

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "diagnose setup problems without starting or changing the daemon",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := collectDoctor(cmd.Context())
			if err := writeDoctorReport(cmd.OutOrStdout(), report, currentOutputMode(), flags.Quiet); err != nil {
				return err
			}
			if report.Failed() {
				return &cliError{Kind: kindChecksFailed, ExitCode: 1, Message: "doctor found failed checks; see the report for suggested fixes"}
			}
			return nil
		},
	}
}

func collectDoctor(ctx context.Context) diagnostics.Report {
	s := &doctorState{}
	s.localChecks()
	s.daemonChecks(ctx)
	return diagnostics.NewReport(s.checks)
}

func writeDoctorReport(w io.Writer, report diagnostics.Report, mode outputMode, quiet bool) error {
	if mode == outputJSON {
		return emitJSON(w, report)
	}
	for _, check := range report.Checks {
		if quiet && (check.Status == diagnostics.StatusOK || check.Status == diagnostics.StatusInfo) {
			continue
		}
		if mode == outputAgent {
			if _, err := fmt.Fprintf(w, "- id=%s category=%s status=%s summary=%q fix=%q", check.ID, check.Category, check.Status, check.Summary, check.Fix); err != nil {
				return err
			}
			for _, detail := range check.Details {
				if _, err := fmt.Fprintf(w, " detail=%q", detail); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(w, "[%s] %s: %s\n", check.Status, check.ID, check.Summary); err != nil {
				return err
			}
			if check.Fix != "" {
				if _, err := fmt.Fprintf(w, "  Fix: %s\n", check.Fix); err != nil {
					return err
				}
			}
		}
		if mode != outputAgent {
			for _, detail := range check.Details {
				if _, err := fmt.Fprintf(w, "  %s\n", textsafe.Line(detail)); err != nil {
					return err
				}
			}
		}
	}
	_, err := fmt.Fprintf(w, "doctor: %d ok, %d info, %d warn, %d fail\n", report.Summary.OK, report.Summary.Info, report.Summary.Warn, report.Summary.Fail)
	return err
}
