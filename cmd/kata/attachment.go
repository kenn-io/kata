package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kit/atomicfile"
)

func newAttachmentCmd() *cobra.Command {
	root := &cobra.Command{Use: "attachments", Short: "attach and download issue files"}
	for _, action := range []string{"add", "ls", "get", "rm"} {
		var output string
		var force bool
		cmd := &cobra.Command{Use: action + " <issue> <attachment>", Short: action + " issue attachments", Args: cobra.ExactArgs(2)}
		if action == "add" {
			cmd.Use = "add <issue> <file>..."
			cmd.Args = cobra.MinimumNArgs(2)
		}
		if action == "ls" {
			cmd.Use = "ls <issue>"
			cmd.Args = cobra.ExactArgs(1)
		}
		if action == "get" {
			cmd.Flags().StringVarP(&output, "output", "o", "", "destination (defaults to attachment name)")
			cmd.Flags().BoolVar(&force, "force", false, "replace the destination")
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			ctx, base, pid, ref, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			hc, err := longRunningClientFor(ctx, base)
			if err != nil {
				return err
			}
			if action == "add" {
				return addAttachmentsCLI(ctx, cmd, hc, base, pid, ref.RefForAPI, args[1:])
			}
			issue, _, err := fetchMetaIssue(ctx, hc, base, pid, ref.RefForAPI)
			if err != nil {
				return err
			}
			entries := map[string]metadata.Attachment{}
			var ids []string
			for key, raw := range issue.Metadata {
				if id, ok := strings.CutPrefix(key, metadata.AttachmentPrefix); ok {
					ids = append(ids, id)
					if e, valid := metadata.Valid(raw); valid {
						entries[id] = e
					}
				}
			}
			if action == "ls" {
				return printAttachmentsCLI(cmd, entries)
			}
			id, err := resolveAttachmentSuffix(ids, args[1])
			if err != nil {
				return err
			}
			if action == "rm" {
				return runMetaPatch(cmd, args[0], metadata.AttachmentPrefix+id, jsontext.Value("null"), "", "unset")
			}
			e, valid := entries[id]
			if !valid {
				return externalCLIValidation("attachment_invalid")
			}
			if output == "" {
				output = e.Name
			}
			endpoint := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/attachments/%s/content", base, pid, url.PathEscape(ref.RefForAPI), url.PathEscape(id))
			if err := downloadAttachmentCLI(ctx, hc, endpoint, e, output, force); err != nil {
				return err
			}
			switch currentOutputMode() {
			case outputJSON:
				return emitJSON(cmd.OutOrStdout(), map[string]any{"attachment_id": id, "path": output, "size": e.Size, "downloaded": true})
			case outputAgent:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-attachments-get attachment_id=%s path=%s size=%d\n", agentValue(id), agentValue(output), e.Size)
			}
			return err
		}
		root.AddCommand(cmd)
	}
	parent := &cobra.Command{Use: "docbank", Short: "work with Docbank files"}
	parent.AddCommand(root)
	return parent
}
func resolveAttachmentSuffix(ids []string, suffix string) (string, error) {
	suffix = strings.ToUpper(strings.TrimSpace(suffix))
	if suffix == "" {
		return "", externalCLIValidation("attachment id is empty")
	}
	for _, id := range ids {
		if strings.ToUpper(id) == suffix {
			return id, nil
		}
	}
	match := ""
	for _, id := range ids {
		if strings.HasSuffix(strings.ToUpper(id), suffix) {
			if match != "" {
				return "", &cliError{Message: "ambiguous attachment suffix", Kind: kindConflict, ExitCode: ExitConflict}
			}
			match = id
		}
	}
	if match == "" {
		return "", &cliError{Message: "attachment not found", Kind: kindNotFound, ExitCode: ExitNotFound}
	}
	return match, nil
}
func printAttachmentsCLI(cmd *cobra.Command, entries map[string]metadata.Attachment) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"attachments": entries})
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if currentOutputMode() == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-attachments-ls count=%d\n", len(ids)); err != nil {
			return err
		}
	}
	for _, id := range ids {
		e := entries[id]
		var err error
		if currentOutputMode() == outputAgent {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "- id=%s name=%s media_type=%s size=%d\n", agentValue(id), agentValue(e.Name), agentValue(e.MediaType), e.Size)
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %d bytes  %s\n", textsafe.Line(e.Name), textsafe.Line(e.MediaType), e.Size, id)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func addAttachmentsCLI(ctx context.Context, cmd *cobra.Command, c *http.Client, base string, pid int64, ref string, files []string) error {
	actor, _ := resolveActor(ctx, flags.As, nil)
	type outcome struct {
		File  string `json:"file"`
		ID    string `json:"attachment_id"`
		Error string `json:"error,omitempty"`
	}
	out := make([]outcome, 0, len(files))
	var failures []error
	for _, file := range files {
		id, err := uploadAttachmentCLI(ctx, c, base, pid, ref, actor, file)
		result := outcome{File: file, ID: id}
		if err != nil {
			result.Error = err.Error()
			failures = append(failures, err)
		}
		out = append(out, result)
		if currentOutputMode() == outputHuman && !flags.Quiet {
			state := id
			if err != nil {
				state = err.Error()
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", textsafe.Line(file), textsafe.Line(state)); err != nil {
				return err
			}
		}
	}
	switch currentOutputMode() {
	case outputJSON:
		if err := emitJSON(cmd.OutOrStdout(), map[string]any{"results": out}); err != nil {
			return err
		}
	case outputAgent:
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-attachments-add count=%d\n", len(out)-len(failures)); err != nil {
			return err
		}
		for _, result := range out {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "- file=%s attachment_id=%s error=%s\n", agentValue(result.File), agentValue(result.ID), agentValue(result.Error)); err != nil {
				return err
			}
		}
	}
	return errors.Join(failures...)
}
func uploadAttachmentCLI(ctx context.Context, c *http.Client, base string, pid int64, ref, actor, filename string) (string, error) {
	file, err := os.Open(filename) //nolint:gosec // The operator selects the source file.
	if err != nil {
		return "", externalCLIValidation(err.Error())
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/attachments?", base, pid, url.PathEscape(ref)) + url.Values{"name": {filepath.Base(filename)}, "actor": {actor}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, file)
	if err != nil {
		return "", err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if err := externalCLIResponseError(resp.StatusCode, raw, nil); err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"attachment_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}
func downloadAttachmentCLI(ctx context.Context, c *http.Client, endpoint string, e metadata.Attachment, dest string, force bool) error {
	if !force {
		if _, err := os.Lstat(dest); err == nil {
			return &cliError{Message: "destination already exists (use --force)", Kind: kindConflict, ExitCode: ExitConflict}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		unexpected := fmt.Errorf("unexpected HTTP status %d downloading attachment", resp.StatusCode)
		return externalCLIResponseError(resp.StatusCode, b, unexpected)
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), ".kata-attachment-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	defer func() { _ = temp.Close() }()
	n, err := io.Copy(temp, io.LimitReader(resp.Body, e.Size+1))
	if err != nil {
		return err
	}
	if n != e.Size {
		return errors.New("attachment integrity failure")
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if force {
		return atomicfile.Replace(temp.Name(), dest)
	}
	return atomicfile.PublishNoReplace(temp.Name(), dest)
}
