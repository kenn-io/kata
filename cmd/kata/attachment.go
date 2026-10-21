package main

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kit/atomicfile"
)

const (
	attachmentReadTimeout     = 36 * time.Minute
	attachmentTransferTimeout = 2*attachmentReadTimeout + 3*time.Minute
)

func newAttachmentCmd() *cobra.Command {
	root := &cobra.Command{Use: "attachments", Short: "attach Docbank files and read pinned content"}
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
			cmd.Flags().BoolVar(&force, "force", false, "replace the destination without following symlinks")
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
			copyClient := *hc
			// Uploads and database-token-authorized downloads can traverse two
			// sequential transfer legs. Reserve both windows plus processing
			// overhead.
			copyClient.Timeout = attachmentReadTimeout
			if action == "add" || action == "get" {
				copyClient.Timeout = attachmentTransferTimeout
			}
			hc = &copyClient
			client, err := kataclient.NewWithHTTPClient(base, hc)
			if err != nil {
				return err
			}
			if action == "add" {
				return addAttachmentsCLI(ctx, cmd, client, pid, ref.RefForAPI, args[1:])
			}
			issue, _, err := fetchMetaIssue(ctx, hc, base, pid, ref.RefForAPI)
			if err != nil {
				return err
			}
			m, err := metadata.DecodeAttachments(issue.Metadata["attachments"])
			if err != nil {
				return err
			}
			if action == "ls" {
				switch currentOutputMode() {
				case outputJSON:
					return printAttachmentsCLI(cmd.OutOrStdout(), m, true)
				case outputAgent:
					return printAttachmentsAgentCLI(cmd.OutOrStdout(), m)
				default:
					return printAttachmentsCLI(cmd.OutOrStdout(), m, false)
				}
			}
			ids := map[string]bool{}
			for id := range m {
				ids[id] = true
			}
			id, err := resolveAttachmentSuffix(ids, args[1])
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/attachments/%s", pid, url.PathEscape(ref.RefForAPI), url.PathEscape(id))
			switch action {
			case "rm":
				actor, _ := resolveActor(ctx, flags.As, nil)
				response, callErr := client.RemoveAttachmentWithResponse(ctx, &generated.RemoveAttachmentRequestOptions{PathParams: &generated.RemoveAttachmentPath{ProjectID: pid, Ref: ref.RefForAPI, AttachmentID: id}, Query: &generated.RemoveAttachmentQuery{Actor: &actor}})
				if err := externalCLITransportError(response, callErr); err != nil {
					return err
				}
				if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
					return err
				}
				switch currentOutputMode() {
				case outputJSON:
					return emitJSON(cmd.OutOrStdout(), map[string]any{"attachment_id": id, "removed": true})
				case outputAgent:
					if flags.Quiet {
						return nil
					}
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-attachments-rm attachment_id=%s\n", agentValue(id))
					return err
				}
				if flags.Quiet {
					return nil
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "removed "+id)
				return err
			case "get":
				e, err := m.Lookup(id)
				if err != nil {
					return externalCLIValidation(err.Error())
				}
				if output == "" {
					output = e.Name
				}
				if err := downloadAttachmentCLI(ctx, hc, base+path+"/content", e, output, force); err != nil {
					return err
				}
				return printAttachmentDownloadCLI(cmd, id, e, output)
			}
			return nil
		}
		root.AddCommand(cmd)
	}
	parent := &cobra.Command{Use: "docbank", Short: "work with Docbank files"}
	parent.AddCommand(root)
	return parent
}
func resolveAttachmentSuffix(ids map[string]bool, suffix string) (string, error) {
	suffix = strings.ToUpper(strings.TrimSpace(suffix))
	if suffix == "" {
		return "", externalCLIValidation("attachment id is empty")
	}
	if ids[suffix] {
		return suffix, nil
	}
	match := ""
	for id := range ids {
		if strings.HasSuffix(id, suffix) {
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
func printAttachmentsCLI(w io.Writer, m metadata.Attachments, structured bool) error {
	if structured {
		visible := metadata.Attachments{}
		for id := range m {
			if _, err := m.Lookup(id); err == nil {
				visible[id] = m[id]
			}
		}
		return emitJSON(w, map[string]any{"attachments": visible})
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		e, err := m.Lookup(id)
		if err != nil {
			continue
		}
		suffix := id
		if len(suffix) > 8 {
			suffix = suffix[len(suffix)-8:]
		}
		if _, err = fmt.Fprintf(w, "%s  %s  %d bytes  %s\n", textsafe.Line(e.Name), textsafe.Line(e.MediaType), e.Size, suffix); err != nil {
			return err
		}
	}
	return nil
}

type attachmentAddOutcome struct {
	File         string `json:"file"`
	AttachmentID string `json:"attachment_id"`
	Error        string `json:"error,omitempty"`
}

func addAttachmentsCLI(ctx context.Context, cmd *cobra.Command, c *kataclient.Client, pid int64, ref string, files []string) error {
	actor, _ := resolveActor(ctx, flags.As, nil)
	out := make([]attachmentAddOutcome, 0, len(files))
	var failures []error
	for _, file := range files {
		id, err := uid.New()
		if err != nil {
			return err
		}
		result := attachmentAddOutcome{File: file, AttachmentID: id}
		bo := backoff.NewExponentialBackOff()
		bo.InitialInterval = 500 * time.Millisecond
		bo.MaxInterval = time.Second
		_, err = backoff.Retry(ctx, func() (struct{}, error) {
			retry, err := uploadAttachmentCLI(ctx, c, pid, ref, actor, id, file)
			if err != nil && !retry {
				return struct{}{}, backoff.Permanent(err)
			}
			return struct{}{}, err
		}, backoff.WithBackOff(bo), backoff.WithMaxTries(2), backoff.WithMaxElapsedTime(0))
		if err != nil {
			err = backoff.AsRetryError(err).LastErr
		}
		if err != nil {
			result.Error = err.Error()
			failures = append(failures, err)
		}
		out = append(out, result)
		if currentOutputMode() == outputHuman && !flags.Quiet {
			state := id
			if err != nil {
				state = "failed: " + err.Error()
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", textsafe.Line(file), textsafe.Line(state)); err != nil {
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
		if len(failures) == 0 && !flags.Quiet {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-attachments-add count=%d\n", len(out)); err != nil {
				return err
			}
		}
		for _, result := range out {
			line := fmt.Sprintf("- file=%s attachment_id=%s", agentValue(result.File), agentValue(result.AttachmentID))
			if result.Error != "" {
				line += " error=" + agentValue(result.Error)
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
				return err
			}
		}
	}
	return errors.Join(failures...)
}

func printAttachmentsAgentCLI(w io.Writer, m metadata.Attachments) error {
	ids := make([]string, 0, len(m))
	for id := range m {
		if _, err := m.Lookup(id); err == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if _, err := fmt.Fprintf(w, "OK docbank-attachments-ls count=%d\n", len(ids)); err != nil {
		return err
	}
	for _, id := range ids {
		entry, err := m.Lookup(id)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "- id=%s name=%s media_type=%s size=%d\n", agentValue(id), agentValue(entry.Name), agentValue(entry.MediaType), entry.Size); err != nil {
			return err
		}
	}
	return nil
}

func printAttachmentDownloadCLI(cmd *cobra.Command, id string, e metadata.Attachment, path string) error {
	switch currentOutputMode() {
	case outputJSON:
		return emitJSON(cmd.OutOrStdout(), map[string]any{
			"downloaded":    true,
			"attachment_id": id,
			"path":          path,
			"size":          e.Size,
			"sha256":        e.Docbank.ContentSHA256,
		})
	case outputAgent:
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-attachments-get attachment_id=%s path=%s size=%d sha256=%s\n", agentValue(id), agentValue(path), e.Size, e.Docbank.ContentSHA256)
		return err
	default:
		return nil
	}
}

func uploadAttachmentCLI(ctx context.Context, c *kataclient.Client, pid int64, ref, actor, id, filename string) (bool, error) {
	file, err := os.Open(filename) //nolint:gosec // G304: the operator explicitly selects the upload source.
	if err != nil {
		return false, externalCLIValidation(err.Error())
	}
	defer func() { _ = file.Close() }()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	mw := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		err := mw.WriteField("attachment_id", id)
		var part io.Writer
		if err == nil {
			part, err = mw.CreateFormFile("file", metadata.CleanAttachmentName(filename))
		}
		if err == nil {
			_, err = io.Copy(part, file)
		}
		if err == nil {
			err = mw.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	resp, callErr := c.AddAttachmentWithResponse(ctx, &generated.AddAttachmentRequestOptions{
		PathParams: &generated.AddAttachmentPath{ProjectID: pid, Ref: ref},
		Query:      &generated.AddAttachmentQuery{Actor: &actor},
	}, func(_ context.Context, r *http.Request) error {
		r.Body = reader
		r.ContentLength = -1
		r.Header.Set("Content-Type", mw.FormDataContentType())
		return nil
	})
	_ = reader.Close()
	writeErr := <-done
	if resp == nil {
		return true, externalCLITransportError(resp, callErr)
	}
	if err = externalCLIResponseError(resp.StatusCode, resp.Body, callErr); err != nil {
		return resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500, err
	}
	return false, writeErr
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
	if resp.StatusCode != 200 {
		if resp.StatusCode >= http.StatusBadRequest {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			return externalCLIResponseError(resp.StatusCode, b, nil)
		}
		return &cliError{
			HTTPStatus: resp.StatusCode,
			Message:    fmt.Sprintf("unexpected HTTP status %d downloading attachment", resp.StatusCode),
			Kind:       kindInternal,
			ExitCode:   ExitInternal,
		}
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

// Used by show's human view; structured output retains the original metadata.
func printIssueAttachments(w io.Writer, raw jsontext.Value) error {
	m, err := metadata.DecodeAttachments(raw)
	if err != nil {
		return nil
	}
	visible := false
	for id := range m {
		if _, err := m.Lookup(id); err == nil {
			visible = true
			break
		}
	}
	if !visible {
		return nil
	}
	if _, err = fmt.Fprintln(w, "\n--- attachments ---"); err != nil {
		return err
	}
	return printAttachmentsCLI(w, m, false)
}
