package main

import (
	"bytes"
	"fmt"
	"io"
	"runtime"

	"go.kenn.io/kata/internal/version"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeVersion(cmd.OutOrStdout(), currentOutputMode())
		},
	}
}

// writeVersion renders the build identity for the given output mode. It backs
// both `kata version` and the conventional root `--version` flag so the two
// spellings can never drift.
func writeVersion(out io.Writer, mode outputMode) error {
	switch mode {
	case outputAgent:
		_, err := fmt.Fprintf(out, "OK version version=%s agent_format=%d\n",
			agentValue(version.Version), agentFormatVersion)
		return err
	case outputJSON:
		var buf bytes.Buffer
		payload := map[string]any{
			"name":         "kata",
			"version":      version.Version,
			"commit":       version.Commit,
			"built":        version.BuildDate,
			"distribution": version.Distribution,
			"go":           runtime.Version(),
			"os":           runtime.GOOS,
			"arch":         runtime.GOARCH,
			"agent_format": agentFormatVersion,
		}
		if err := emitJSON(&buf, payload); err != nil {
			return err
		}
		_, err := fmt.Fprint(out, buf.String())
		return err
	}
	_, err := fmt.Fprintf(out,
		"kata %s\n"+
			"  commit:  %s\n"+
			"  built:   %s\n"+
			"  go:      %s\n"+
			"  os/arch: %s/%s\n",
		version.Version, version.Commit, version.BuildDate,
		runtime.Version(), runtime.GOOS, runtime.GOARCH,
	)
	return err
}
