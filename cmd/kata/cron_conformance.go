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
)

const conformanceInputLimit = 16 << 20

type conformanceHeader struct {
	Schema      string                  `json:"schema"`
	Declaration conformance.Declaration `json:"declaration"`
}

func newCronConformanceCmd() *cobra.Command {
	group := &cobra.Command{Use: "conformance", Short: "check adapter scheduling decisions offline", Long: "Export adapter conformance vectors or check decision answers. These commands require no project, daemon, network or executor. Results are JSON."}
	var dir, declarationFile, answersFile string
	export := &cobra.Command{Use: "export", Short: "export versioned decision scenarios", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if dir == "" {
			return cronCLIValidation("--dir is required")
		}
		d := conformance.Reference{}.Declare()
		if declarationFile != "" {
			d = conformance.Declaration{}
			raw, err := readConformanceFile(cmd.Context(), cmd.InOrStdin(), declarationFile)
			if err != nil {
				return cronCLIValidation(err.Error())
			}
			if err := json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)); err != nil {
				return cronCLIValidation(err.Error())
			}
		}
		if err := d.Validate(); err != nil {
			return cronCLIValidation(err.Error())
		}
		files, err := conformance.ExportFiles(cmd.Context(), d)
		if err != nil {
			return err
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
		}{conformance.Schema, dir, names})
	}}
	export.Flags().StringVar(&dir, "dir", "", "destination directory; existing files are never replaced")
	export.Flags().StringVar(&declarationFile, "declaration", "", "adapter declaration JSON file, or - for stdin")
	check := &cobra.Command{Use: "check", Short: "compare adapter answers with the declared profile", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if answersFile == "" {
			return cronCLIValidation("--answers is required")
		}
		raw, err := readConformanceFile(cmd.Context(), cmd.InOrStdin(), answersFile)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		header, answers, err := readConformanceAnswers(cmd.Context(), raw)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		report, err := conformance.Check(cmd.Context(), header.Declaration, answers)
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
	check.Flags().StringVar(&answersFile, "answers", "", "JSONL header and scenario answers file, or - for stdin")
	group.AddCommand(export, check)
	return group
}

func readConformanceFile(ctx context.Context, stdin io.Reader, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader := stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		reader = file
	}
	// CLI stdin and owned files are closable. Closing on cancellation unblocks
	// a pipe read without leaving a reader goroutine behind.
	if closer, ok := reader.(io.ReadCloser); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	raw, err := io.ReadAll(io.LimitReader(reader, conformanceInputLimit+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > conformanceInputLimit {
		return nil, fmt.Errorf("conformance input exceeds 16 MiB")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
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
			if header.Schema != conformance.Schema {
				return header, nil, fmt.Errorf("unsupported conformance schema")
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
				return header, nil, fmt.Errorf("line %d: answer requires name and outputs array", line)
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
	if err := os.MkdirAll(dir, 0755); err != nil {
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
	created := []string{}
	// Roll back only this invocation's new files, including after cancellation.
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
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		created = append(created, path)
		_, writeErr := file.Write(files[name])
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return err
		}
	}
	return nil
}
