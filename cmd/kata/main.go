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
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// globalFlags carries the universal flags applied on every command. Sel is the
// output selection as typed; Mode is the single resolved answer, written once
// by PersistentPreRunE and read everywhere through currentOutputMode().
type globalFlags struct {
	Sel       outputSelection
	Mode      outputMode
	Quiet     bool
	As        string
	Workspace string
	Project   string
	Daemon    string
	Version   bool
}

var flags globalFlags

// runEEntered is set by PersistentPreRunE before any subcommand's RunE fires.
// It stays false when cobra fails during argument/flag parsing, allowing main()
// to distinguish a parse error (ExitUsage) from an operational failure (ExitInternal).
var runEEntered bool
var errorCommandName string

func newRootCmd() *cobra.Command {
	flags = globalFlags{}
	runEEntered = false
	errorCommandName = ""
	cmd := &cobra.Command{
		Use:           "kata",
		Short:         "kata — lightweight issue tracker for agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		// The root is runnable only to serve the conventional `--version`
		// flag; with no flag it falls back to printing help. Registering
		// the flag ourselves (rather than setting cobra's Version field)
		// keeps --json/--agent working, because cobra's built-in version
		// flag short-circuits before PersistentPreRunE resolves the output
		// mode.
		//
		// A runnable root also means PersistentPreRunE now runs for a bare
		// `kata`, where cobra previously short-circuited to help before it.
		// Plain `kata` still prints help and exits zero, but invalid or
		// conflicting global output flags are now a usage error on the root
		// instead of being silently ignored, matching every subcommand.
		// TestRoot_NoArgsValidatesGlobalOutputFlags pins that contract.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flags.Version {
				return writeVersion(cmd.OutOrStdout(), currentOutputMode())
			}
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			runEEntered = true
			errorCommandName = commandLeaf(cmd)
			mode, err := resolveOutputModeForCommand(cmd)
			if err != nil {
				return err
			}
			flags.Mode = mode
			return nil
		},
	}
	cmd.PersistentFlags().Var(outputFormatFlag{values: &flags.Sel.formats},
		"format", "output format: human|json|agent; contract for quickstart")
	cmd.PersistentFlags().BoolVar(&flags.Sel.json, "json", false, "emit machine-readable JSON")
	cmd.PersistentFlags().BoolVar(&flags.Sel.agent, "agent", false, "emit concise agent-readable text")
	cmd.PersistentFlags().BoolVarP(&flags.Quiet, "quiet", "q", false, "suppress non-essential output")
	cmd.PersistentFlags().StringVar(&flags.As, "as", "", "override actor (default: $KATA_AUTHOR > $USER > git > anonymous)")
	cmd.PersistentFlags().StringVar(&flags.Workspace, "workspace", "", "path used for project resolution (default: cwd)")
	cmd.PersistentFlags().StringVar(&flags.Project, "project", "", "project name for project-scoped commands")
	cmd.PersistentFlags().StringVar(&flags.Daemon, "daemon", "", "named daemon catalog entry to target for this command")
	// Root-local, not persistent: `kata --version` is the conventional
	// spelling, while `kata list --version` should stay an unknown flag.
	// No `-v` shorthand: that spelling conventionally means verbose, and
	// reclaiming it after release would break callers.
	cmd.Flags().BoolVar(&flags.Version, "version", false, "print version information")
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
		newConnectorCmd(),
		newBridgeCmd(),
		newMCPCmd(),
		newStorageCmd(),
		newInitCmd(),
		newCreateCmd(),
		newShowCmd(),
		newListCmd(),
		newEditCmd(),
		newScheduleCmd(),
		newDeadlineCmd(),
		newMetaCmd(),
		newMoveCmd(),
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
		newAttentionHookCmd(),
		newAgentContractHookCmd(),
		newReadyCmd(),
		newNextCmd(),
		newWaitCmd(),
		newSyncCmd(),
		newFederationCmd(),
		newEventsCmd(),
		newExportCmd(),
		newImportCmd(),
		newDigestCmd(),
		newAuditCmd(),
		newQuickstartCmd(),
		newWhoamiCmd(),
		newHealthCmd(),
		newOpenAPICmd(),
		newProjectsCmd(),
		newTokensCmd(),
		newUICmd(),
		newTUICmd(),
		newUpdateCmd(),
		newVersionCmd(),
		newWebAssetsCheckCmd(),
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
	cmd := newRootCmd()
	if err := cmd.ExecuteContext(ctx); err != nil {
		emitRootError(os.Stderr, cmd, os.Args[1:], err, runEEntered)
		os.Exit(exitCodeForErr(err, runEEntered))
	}
}

func emitErrorForMode(w io.Writer, err error, mode outputMode, runEReached bool) {
	switch mode {
	case outputJSON:
		emitJSONError(w, err, runEReached)
	case outputAgent:
		emitAgentError(w, commandNameForError(nil, nil, runEReached), cliErrorForErr(err, runEReached))
	default:
		emitHumanError(w, err, runEReached)
	}
}

func emitRootError(w io.Writer, cmd *cobra.Command, args []string, err error, runEReached bool) {
	mode, modeErr := resolvedOutputModeForError(cmd, args)
	if modeErr != nil {
		err = modeErr
	}
	switch mode {
	case outputAgent:
		emitAgentError(w, commandNameForError(cmd, args, runEReached), cliErrorForErr(err, runEReached))
	default:
		emitErrorForMode(w, err, mode, runEReached)
	}
}

func resolvedOutputModeForError(root *cobra.Command, args []string) (outputMode, error) {
	if flags.Mode != "" {
		return flags.Mode, nil
	}
	cmd, sel := preParse(root, args)
	return sel.resolve(isImportCommand(cmd), supportsContractOutput(cmd))
}

func commandNameForError(root *cobra.Command, args []string, runEReached bool) string {
	if errorCommandName != "" {
		return errorCommandName
	}
	if !runEReached {
		if cmd, _ := preParse(root, args); cmd != nil && cmd != root {
			return commandLeaf(cmd)
		}
	}
	return "kata"
}

func childCommandByName(cmd *cobra.Command, name string) *cobra.Command {
	if cmd == nil {
		return nil
	}
	for _, child := range cmd.Commands() {
		if child.Name() == name {
			return child
		}
		if slices.Contains(child.Aliases, name) {
			return child
		}
	}
	return nil
}

func commandLeaf(cmd *cobra.Command) string {
	if cmd == nil {
		return "kata"
	}
	parts := strings.Fields(cmd.CommandPath())
	if len(parts) == 0 {
		return "kata"
	}
	return parts[len(parts)-1]
}

// emitJSONError writes a JSON envelope shaped after the daemon's
// ErrorEnvelope plus a `kind` and `exit_code` for client-side classification.
// The JSON envelope is always emitted to stderr in main so stdout stays
// reserved for successful command output.
func emitJSONError(w io.Writer, err error, runEReached bool) {
	cli := cliErrorForErr(err, runEReached)
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
	emitHumanError(w, err, runEReached)
}

func emitHumanError(w io.Writer, err error, runEReached bool) {
	cli := cliErrorForErr(err, runEReached)
	_, _ = fmt.Fprintln(w, "kata:", cli.Message) //nolint:gosec // G705: CLI stderr error text, not HTML.
}

func cliErrorForErr(err error, runEReached bool) *cliError {
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
	return cli
}

// exitCodeForErr returns the exit code an error should produce. When
// err is a *cliError, its ExitCode wins; otherwise exitCodeFor's
// runE-reached heuristic decides.
func exitCodeForErr(err error, runEReached bool) int {
	if cli, ok := errors.AsType[*cliError](err); ok {
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
	_, after, ok := strings.Cut(msg, prefix)
	if !ok {
		return err
	}
	rest := after
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
