// Package main is the kata CLI entry point.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// globalFlags carries the universal flags applied on every command.
type globalFlags struct {
	JSON      bool
	Quiet     bool
	As        string
	Workspace string
	Project   string
}

var flags globalFlags

// runEEntered is set by PersistentPreRunE before any subcommand's RunE fires.
// It stays false when cobra fails during argument/flag parsing, allowing main()
// to distinguish a parse error (ExitUsage) from an operational failure (ExitInternal).
var runEEntered bool

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "kata",
		Short:         "kata — lightweight issue tracker for agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			runEEntered = true
			return nil
		},
	}
	cmd.PersistentFlags().BoolVar(&flags.JSON, "json", false, "emit machine-readable JSON")
	cmd.PersistentFlags().BoolVarP(&flags.Quiet, "quiet", "q", false, "suppress non-essential output")
	cmd.PersistentFlags().StringVar(&flags.As, "as", "", "override actor (default: $KATA_AUTHOR > $USER > git > anonymous)")
	cmd.PersistentFlags().StringVar(&flags.Workspace, "workspace", "", "path used for project resolution (default: cwd)")
	cmd.PersistentFlags().StringVar(&flags.Project, "project", "", "project name for project-scoped commands")
	// Catch the cobra/pflag pitfall where a positional that looks like
	// a negative integer (kata show -1, kata delete -1) is parsed as
	// a flag and produces "unknown shorthand flag: '1' in -1" — useless
	// to humans and to agents. Translate the cryptic pflag message into
	// a kindUsage cliError that points at the `--` separator workaround
	// (hammer-test finding #9). Applies to every subcommand because
	// FlagErrorFunc is inherited from the root.
	cmd.SetFlagErrorFunc(translateFlagError)

	subs := []*cobra.Command{
		newDaemonCmd(),
		newInitCmd(),
		newCreateCmd(),
		newShowCmd(),
		newListCmd(),
		newEditCmd(),
		newCommentCmd(),
		newCloseCmd(),
		newReopenCmd(),
		newDeleteCmd(),
		newRestoreCmd(),
		newPurgeCmd(),
		newSearchCmd(),
		newLabelCmd(),
		newLabelsCmd(),
		newAssignCmd(),
		newUnassignCmd(),
		newClaimCmd(),
		newReadyCmd(),
		newEventsCmd(),
		newExportCmd(),
		newImportCmd(),
		newDigestCmd(),
		newAuditCmd(),
		newQuickstartCmd(),
		newWhoamiCmd(),
		newHealthCmd(),
		newProjectsCmd(),
		newTUICmd(),
		newVersionCmd(),
	}
	cmd.AddCommand(subs...)
	return cmd
}

func main() {
	// Wire SIGINT/SIGTERM into cobra's command context so long-running
	// subcommands (notably `kata daemon start`) can shut down gracefully via
	// their deferred cleanups instead of being torn down mid-syscall. Once the
	// first signal arrives, restore default handling so a second ctrl-C
	// escalates to a hard kill (e.g. if a deferred cleanup hangs).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		emitError(os.Stderr, err, flags.JSON, runEEntered)
		os.Exit(exitCodeForErr(err, runEEntered))
	}
}

// emitError writes the error to w. When jsonMode is true, the output
// is a JSON envelope shaped after the daemon's ErrorEnvelope plus a
// `kind` and `exit_code` for client-side classification — agents can
// branch on a stable taxonomy without grepping the human message.
// When jsonMode is false, the human path stays "kata: <message>".
//
// The JSON envelope is always emitted to stderr (where the human
// message also goes) so consumers don't have to reconfigure stream
// routing per --json. Stdout stays reserved for successful command
// output, so a partial-success run can emit useful stdout JSON and
// an error envelope on stderr without the streams being mixed.
func emitError(w io.Writer, err error, jsonMode bool, runEReached bool) {
	var cli *cliError
	if !errors.As(err, &cli) {
		// Non-cliError: synthesize one so the JSON path has uniform
		// shape. Kind/code are inferred from exit-code conventions.
		exit := exitCodeForErr(err, runEReached)
		cli = &cliError{
			Message:  err.Error(),
			Kind:     kindForExit(exit),
			ExitCode: exit,
		}
	}
	if jsonMode {
		env := struct {
			Error struct {
				Kind     errKind `json:"kind"`
				Code     string  `json:"code,omitempty"`
				Message  string  `json:"message"`
				ExitCode int     `json:"exit_code"`
			} `json:"error"`
		}{}
		env.Error.Kind = cli.Kind
		env.Error.Code = cli.Code
		env.Error.Message = cli.Message
		env.Error.ExitCode = cli.ExitCode
		bs, mErr := json.Marshal(env)
		if mErr == nil {
			_, _ = fmt.Fprintln(w, string(bs))
			return
		}
		// JSON marshal failed (shouldn't happen on a fixed shape) —
		// fall through to plain text so the user still gets *something*.
	}
	_, _ = fmt.Fprintln(w, "kata:", cli.Message)
}

// exitCodeForErr returns the exit code an error should produce. When
// err is a *cliError, its ExitCode wins; otherwise exitCodeFor's
// runE-reached heuristic decides.
func exitCodeForErr(err error, runEReached bool) int {
	var cli *cliError
	if errors.As(err, &cli) {
		return cli.ExitCode
	}
	return exitCodeFor(err, runEReached)
}

// translateFlagError rewrites pflag's "unknown shorthand flag: 'N' in
// -N..." message into a useful cliError when N is a digit, so users
// who typed `kata show -1` get a clear pointer at the `--` separator
// workaround (hammer-test finding #9) instead of a cryptic flag-parse
// trace. All other flag errors pass through unchanged.
//
// The detection is intentionally narrow: we look for a leading digit
// after the dash because that's the exact pflag message shape for the
// negative-integer-as-positional case. Other "-x" flag typos still
// produce pflag's regular message.
func translateFlagError(_ *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	const prefix = "unknown shorthand flag: '"
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return err
	}
	rest := msg[idx+len(prefix):]
	if rest == "" || !isDigit(rest[0]) {
		return err
	}
	return &cliError{
		Message: "negative numbers in positional args need the `--` " +
			"separator (e.g. `kata show -- -1`)",
		Kind:     kindUsage,
		ExitCode: ExitUsage,
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// exitCodeFor maps a non-cliError ExecuteContext error to a CLI exit code
// based on whether RunE was reached. PersistentPreRunE flips runEEntered to
// true before any subcommand's RunE runs, so a false value means cobra
// rejected the invocation during arg/flag parsing.
func exitCodeFor(_ error, runEReached bool) int {
	if !runEReached {
		// Cobra failed before PersistentPreRunE — unknown command, missing
		// positional arg (cobra.ExactArgs / NoArgs), or bad flag value.
		return ExitUsage
	}
	// RunE entered and returned a plain error — operational failure (daemon
	// startup, HTTP transport, filesystem).
	return ExitInternal
}
