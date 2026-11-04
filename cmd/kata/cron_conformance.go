package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/pkg/cron/conformance"
	"go.kenn.io/kit/atomicfile"
)

const conformanceInputLimit = 16 << 20

type conformanceHeader struct {
	Schema      string                  `json:"schema"`
	Declaration conformance.Declaration `json:"declaration"`
}

func newCronConformanceCmd() *cobra.Command {
	group := &cobra.Command{Use: "conformance", Short: "check adapter scheduling decisions offline", Long: "Export versioned adapter scenarios or check JSONL decision answers without a project, daemon, network or executor. Results are JSON."}
	var dir, declarationPath, answersPath, schema string
	export := &cobra.Command{Use: "export", Short: "export versioned decision scenarios", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if dir == "" {
			return cronCLIValidation("--dir is required")
		}
		d := conformance.Reference{}.Declare()
		if declarationPath != "" {
			raw, err := readConformanceFile(cmd.Context(), cmd.InOrStdin(), declarationPath)
			if err != nil {
				return cronCLIValidation(err.Error())
			}
			// A supplied file is complete; missing policy members must not inherit
			// defaults from the reference declaration.
			d = conformance.Declaration{}
			if err := json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)); err != nil {
				return cronCLIValidation(err.Error())
			}
		}
		if err := d.Validate(); err != nil {
			return cronCLIValidation(err.Error())
		}
		suite, err := conformance.NewSuite(cmd.Context(), schema, d)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		files, err := suite.ExportFiles(cmd.Context())
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		if err := exportConformanceFiles(cmd.Context(), dir, files); err != nil {
			return err
		}
		names := []string{}
		for name := range files {
			names = append(names, name)
		}
		slices.Sort(names)
		return emitJSON(cmd.OutOrStdout(), struct {
			Schema    string   `json:"schema"`
			Directory string   `json:"directory"`
			Files     []string `json:"files"`
		}{schema, dir, names})
	}}
	export.Flags().StringVar(&schema, "schema", conformance.Schema, "fixture schema version; v1 default or explicit v2")
	export.Flags().StringVar(&dir, "dir", "", "destination directory; existing files are never replaced")
	export.Flags().StringVar(&declarationPath, "declaration", "", "complete adapter declaration JSON file, or - for stdin")
	check := &cobra.Command{Use: "check", Short: "compare adapter answers against the declared profile", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if answersPath == "" {
			return cronCLIValidation("--answers is required")
		}
		raw, err := readConformanceFile(cmd.Context(), cmd.InOrStdin(), answersPath)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		header, answers, err := readConformanceAnswers(cmd.Context(), raw)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		suite, err := conformance.NewSuite(cmd.Context(), header.Schema, header.Declaration)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		report, err := suite.Check(cmd.Context(), answers)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		if err := emitJSON(cmd.OutOrStdout(), report); err != nil {
			return err
		}
		if len(report.Failed) > 0 {
			return &cliError{Kind: kindChecksFailed, ExitCode: 1, Message: "adapter conformance failed; see the JSON report"}
		}
		return nil
	}}
	check.Flags().StringVar(&answersPath, "answers", "", "JSONL header and scenario answers file, or - for stdin")
	group.AddCommand(export, check)
	return group
}

func readConformanceFile(ctx context.Context, stdin io.Reader, path string) ([]byte, error) {
	raw, err := readCronInput(ctx, stdin, path, conformanceInputLimit)
	if errors.Is(err, errCronInputTooLarge) {
		return nil, fmt.Errorf("conformance input exceeds 16 MiB")
	}
	return raw, err
}

func readConformanceAnswers(ctx context.Context, raw []byte) (conformanceHeader, []conformance.Answer, error) {
	var header conformanceHeader
	answers := []conformance.Answer{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), conformanceInputLimit)
	line := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return header, nil, err
		}
		line++
		if line == 1 {
			if err := json.Unmarshal(scanner.Bytes(), &header, json.RejectUnknownMembers(true)); err != nil {
				return header, nil, fmt.Errorf("header: %w", err)
			}
			if err := header.Declaration.Validate(); err != nil {
				return header, nil, err
			}
		} else {
			var answer conformance.Answer
			if err := json.Unmarshal(scanner.Bytes(), &answer, json.RejectUnknownMembers(true)); err != nil {
				return header, nil, fmt.Errorf("line %d: %w", line, err)
			}
			if strings.TrimSpace(answer.Name) == "" || answer.Outputs == nil {
				return header, nil, fmt.Errorf("line %d requires name and outputs array", line)
			}
			answers = append(answers, answer)
		}
	}
	if err := scanner.Err(); err != nil {
		return header, nil, err
	}
	if line == 0 {
		return header, nil, fmt.Errorf("missing conformance header")
	}
	return header, answers, nil
}

func exportConformanceFiles(ctx context.Context, dir string, files map[string][]byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("destination exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	created := []string{}
	// Finalization must remove this invocation's published files even after the
	// caller cancels. Never remove a pre-existing destination.
	defer func() {
		if err != nil {
			for _, path := range created {
				err = errors.Join(err, os.Remove(path))
			}
		}
	}()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		writeErr := atomicfile.WriteNew(path, files[name])
		if writeErr == nil || errors.Is(writeErr, atomicfile.ErrPublished) {
			created = append(created, path)
		}
		if writeErr != nil {
			return writeErr
		}
	}
	return nil
}
