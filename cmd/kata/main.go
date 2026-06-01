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
	Format       string
	FormatValues []string
	JSON         bool
	Agent        bool
	Mode         outputMode
	Quiet        bool
	As           string
	Workspace    string
	Project      string
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
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			runEEntered = true
			errorCommandName = commandLeaf(cmd)
			mode, err := resolveOutputModeForCommand(cmd)
			if err != nil {
				return err
			}
			flags.Mode = mode
			if mode == outputJSON {
				flags.JSON = true
			}
			return nil
		},
	}
	cmd.PersistentFlags().Var(outputFormatFlag{value: &flags.Format, values: &flags.FormatValues},
		"format", "output format: human|json|agent")
	cmd.PersistentFlags().BoolVar(&flags.JSON, "json", false, "emit machine-readable JSON")
	cmd.PersistentFlags().BoolVar(&flags.Agent, "agent", false, "emit concise agent-readable text")
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
		newFederationCmd(),
		newEventsCmd(),
		newExportCmd(),
		newImportCmd(),
		newMigrateCmd(),
		newDigestCmd(),
		newAuditCmd(),
		newQuickstartCmd(),
		newWhoamiCmd(),
		newHealthCmd(),
		newProjectsCmd(),
		newTokensCmd(),
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
	cmd := newRootCmd()
	if err := cmd.ExecuteContext(ctx); err != nil {
		emitRootError(os.Stderr, cmd, os.Args[1:], err, runEEntered)
		os.Exit(exitCodeForErr(err, runEEntered))
	}
}

// emitError preserves the legacy bool-shaped test/helper API while main uses
// the resolved output mode.
func emitError(w io.Writer, err error, jsonMode bool, runEReached bool) {
	if jsonMode {
		emitErrorForMode(w, err, outputJSON, runEReached)
		return
	}
	emitErrorForMode(w, err, outputHuman, runEReached)
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
	importLegacy := false
	if cmd := commandFromArgs(root, args); isImportCommand(cmd) {
		importLegacy = true
	}
	mode, err := resolveOutputModeArgsForCommand(args, flags.Format, flags.JSON, flags.Agent, importLegacy, root)
	if err != nil {
		return outputModeHintForResolutionError(importLegacy), err
	}
	return mode, nil
}

func outputModeHintForResolutionError(importLegacy bool) outputMode {
	formats := flags.FormatValues
	if len(formats) == 0 && flags.Format != "" {
		formats = []string{flags.Format}
	}
	selected := make([]outputMode, 0, len(formats)+2)
	for _, format := range formats {
		switch mode := outputMode(outputFormatValue(format, importLegacy)); mode {
		case outputHuman, outputJSON, outputAgent:
			selected = append(selected, mode)
		}
	}
	if flags.JSON {
		selected = append(selected, outputJSON)
	}
	if flags.Agent {
		selected = append(selected, outputAgent)
	}
	if len(selected) == 0 {
		return outputHuman
	}
	first := selected[0]
	for _, mode := range selected[1:] {
		if mode != first {
			return outputHuman
		}
	}
	return first
}

func commandNameForError(root *cobra.Command, args []string, runEReached bool) string {
	if errorCommandName != "" {
		return errorCommandName
	}
	if !runEReached {
		if cmd := commandFromArgs(root, args); cmd != nil && cmd != root {
			return commandLeaf(cmd)
		}
	}
	return "kata"
}

func commandFromArgs(root *cobra.Command, args []string) *cobra.Command {
	if root == nil {
		return nil
	}
	cmd := root
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "-") {
			if flagArgConsumesValue(cmd, arg) && i+1 < len(args) {
				i++
			}
			continue
		}
		next := childCommandByName(cmd, arg)
		if next == nil {
			break
		}
		cmd = next
	}
	return cmd
}

func childCommandByName(cmd *cobra.Command, name string) *cobra.Command {
	if cmd == nil {
		return nil
	}
	for _, child := range cmd.Commands() {
		if child.Name() == name {
			return child
		}
		for _, alias := range child.Aliases {
			if alias == name {
				return child
			}
		}
	}
	return nil
}

func flagArgConsumesValue(cmd *cobra.Command, arg string) bool {
	name := strings.TrimLeft(arg, "-")
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		return false
	}
	if name == "" {
		return false
	}
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		flag = cmd.PersistentFlags().Lookup(name)
	}
	if flag == nil {
		flag = cmd.InheritedFlags().Lookup(name)
	}
	return flag != nil && flag.Value.Type() != "bool"
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
