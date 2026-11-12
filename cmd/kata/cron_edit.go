package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/editorcmd"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kit/atomicfile"
)

var cronEditTerminal = func(cmd *cobra.Command) bool {
	in, inputFile := cmd.InOrStdin().(*os.File)
	out, outputFile := cmd.OutOrStdout().(*os.File)
	return inputFile && outputFile && isTTY(in) && isTTY(out)
}

var cronEditEditor = runCronEditEditor
var cronEditWriteTemp = (*os.File).Write

func runCronEditEditor(ctx context.Context, cmd *cobra.Command, path string) error {
	editor := editorcmd.Resolve()
	//nolint:gosec // Running the user's chosen interactive editor is intentional; no shell is used.
	process := exec.CommandContext(ctx, editor[0], append(editor[1:], path)...)
	process.Stdin, process.Stdout, process.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	return process.Run()
}

type cronEditSnapshot struct {
	body, response []byte
	event          string
	deleted        bool
}

func decodeCronEditSnapshot(resource, identity string, raw []byte) (cronEditSnapshot, error) {
	var envelope map[string]jsontext.Value
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return cronEditSnapshot{}, fmt.Errorf("invalid cron show response: %w", err)
	}
	var document struct {
		UID        string         `json:"uid"`
		Name       string         `json:"name"`
		Definition jsontext.Value `json:"definition"`
		Event      string         `json:"definition_event_uid"`
		DeletedAt  jsontext.Value `json:"deleted_at"`
	}
	if err := json.Unmarshal(envelope[resource], &document); err != nil {
		return cronEditSnapshot{}, fmt.Errorf("invalid cron definition response: %w", err)
	}
	event, err := normalizedCronCLIUID(document.Event)
	if err != nil || document.UID != identity {
		return cronEditSnapshot{}, errors.New("invalid cron definition identity or definition_event_uid")
	}
	fields := map[string]jsontext.Value{"definition": document.Definition}
	if err := setCronStringField(fields, "name", document.Name); err != nil {
		return cronEditSnapshot{}, err
	}
	body, err := json.Marshal(fields)
	return cronEditSnapshot{body: body, event: event, response: raw, deleted: len(document.DeletedAt) > 0 && document.DeletedAt.Kind() != 'n'}, err
}

func (o cronCLIOptions) edit(ctx context.Context, cmd *cobra.Command, daemon daemonAPI, client *kataclient.Client, projectID int64) (result error) {
	read := o
	read.action = "show"
	raw, err := read.callRead(daemon, client, projectID)
	if err != nil {
		return err
	}
	base, err := decodeCronEditSnapshot(o.resource, o.identity, raw)
	if err != nil {
		return err
	}
	if base.deleted {
		return cronCLIValidation("definition is deleted; restore it first")
	}
	baseFields, err := cronEditParseStored(o.resource, base.body)
	if err != nil {
		return err
	}
	base.body, err = json.Marshal(baseFields)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "kata-cron-"+o.resource+"-"+o.identity+"-*.json")
	if err != nil {
		return err
	}
	path := file.Name()
	saved := false
	recoveryEvent := base.event
	defer func() {
		if result == nil {
			err := os.Remove(path)
			if err == nil {
				return
			}
			result = err
		}
		if saved {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "definition saved; response output or draft cleanup failed")
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "draft kept: %s\n", path)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Check kata cron %s show %s. To retry this draft, remove its leading // header and use update --file with --expected-event-uid %s. A new edit reads a new base.\n", o.resource, o.identity, recoveryEvent)
	}()
	if err := file.Close(); err != nil {
		return err
	}
	draft := cronEditPretty(base.body)
	if err := writeCronEditFile(ctx, path, cronEditBuffer(o.resource, o.identity, base.event, draft, nil)); err != nil {
		return err
	}
	var retryErr error
	var previous []byte
	conflictReopen := false
	for {
		if err := cronEditEditor(ctx, cmd, path); err != nil {
			return cronCLIValidation("editor exited without saving: " + err.Error())
		}
		content, err := readCronEditFile(ctx, path)
		if err != nil {
			return err
		}
		draft, marked := cronEditSplit(content)
		if len(bytes.TrimSpace(draft)) == 0 {
			_, err := fmt.Fprintln(cmd.ErrOrStderr(), "edit aborted; nothing saved")
			return err
		}
		if conflictReopen && marked {
			return retryErr
		}
		if retryErr != nil && !conflictReopen && bytes.Equal(draft, previous) {
			return retryErr
		}
		// Restore the managed recovery header even if the editor removed it.
		// The user's body remains intact, including on an uncertain PUT.
		if err := writeCronEditFile(ctx, path, cronEditBuffer(o.resource, o.identity, base.event, draft, nil)); err != nil {
			return err
		}
		fields, validationErr := cronEditParse(o.resource, draft)
		var updated []byte
		if validationErr == nil {
			updated, err = json.Marshal(fields)
			if err != nil {
				return err
			}
			if cronEditEqual(base.body, updated) {
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), "no changes")
				return err
			}
			if err := applyCronActor(cmd, fields); err != nil {
				return err
			}
			if err := setCronStringField(fields, "expected_event_uid", base.event); err != nil {
				return err
			}
			body, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			raw, validationErr = o.writeEdit(ctx, daemon, projectID, body)
			if validationErr == nil {
				saved = true
				if _, err := decodeCronEditSnapshot(o.resource, o.identity, raw); err != nil {
					return err
				}
				return emitCronEditResult(ctx, cmd, raw)
			}
		}
		var notes []string
		conflictReopen = false
		apiErr, isAPIError := errors.AsType[*cliError](validationErr)
		switch {
		case isAPIError && apiErr.HTTPStatus == http.StatusConflict && apiErr.Code == "cron_conflict":
			current, err := o.editConflictCurrent(ctx, daemon, client, projectID, apiErr)
			if err != nil {
				return err
			}
			if current.deleted {
				return &cliError{Message: "definition was deleted while editing; restore it first", Kind: kindConflict, ExitCode: ExitConflict}
			}
			fields, err := cronEditParseStored(o.resource, current.body)
			if err != nil {
				return err
			}
			current.body, err = json.Marshal(fields)
			if err != nil {
				return err
			}
			if current.event != base.event && cronEditEqual(current.body, updated) {
				saved = true
				return emitCronEditResult(ctx, cmd, current.response)
			}
			notes = []string{strings.TrimPrefix(cronEditConflictMarker, "// ")}
			notes = append(notes, cronLineDiff(strings.Split(string(cronEditPretty(base.body)), "\n"), strings.Split(string(cronEditPretty(current.body)), "\n"))...)
			base = current
			conflictReopen = true
		case isAPIError && (apiErr.HTTPStatus == http.StatusBadRequest || apiErr.HTTPStatus == 0 && apiErr.ExitCode == ExitValidation):
			notes = []string{"error: " + validationErr.Error()}
		default:
			return validationErr
		}
		retryErr, previous = validationErr, bytes.Clone(draft)
		if err := writeCronEditFile(ctx, path, cronEditBuffer(o.resource, o.identity, base.event, draft, notes)); err != nil {
			return err
		}
		recoveryEvent = base.event
	}
}

func emitCronEditResult(ctx context.Context, cmd *cobra.Command, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if flags.Quiet && currentOutputMode() != outputJSON {
		return nil
	}
	return emitJSON(cmd.OutOrStdout(), jsontext.Value(raw))
}

func (o cronCLIOptions) editConflictCurrent(ctx context.Context, daemon daemonAPI, client *kataclient.Client, projectID int64, conflict *cliError) (cronEditSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return cronEditSnapshot{}, err
	}
	var data map[string]jsontext.Value
	if err := json.Unmarshal(conflict.Data, &data); err == nil && len(data["current"]) > 0 && data["current"].Kind() != 'n' {
		raw, err := json.Marshal(map[string]jsontext.Value{o.resource: data["current"]})
		if err != nil {
			return cronEditSnapshot{}, err
		}
		return decodeCronEditSnapshot(o.resource, o.identity, raw)
	}
	read := o
	read.action = "show"
	raw, err := read.callRead(daemon, client, projectID)
	if err != nil {
		return cronEditSnapshot{}, err
	}
	return decodeCronEditSnapshot(o.resource, o.identity, raw)
}

func (o cronCLIOptions) writeEdit(ctx context.Context, daemon daemonAPI, projectID int64, raw []byte) ([]byte, error) {
	client, err := longRunningClientForResolved(ctx, daemon.resolved)
	if err != nil {
		return nil, err
	}
	// An edit resends a complete definition and must never redirect its PUT.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/cron/%ss/%s", daemon.baseURL, projectID, o.resource, o.identity)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request) //nolint:gosec // Origin is the resolved daemon; resource is registered and identity is a validated ULID. The authenticated transport rejects redirects.
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2*cronDefinitionRequestLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 2*cronDefinitionRequestLimit {
		return nil, errors.New("cron response exceeds its byte limit")
	}
	body, err = cronCLIResponse(response.StatusCode, body, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &cliError{Message: fmt.Sprintf("cron edit was not confirmed (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, Kind: kindInternal, ExitCode: ExitInternal}
	}
	return body, nil
}

func readCronEditFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path) //nolint:gosec // Path comes from os.CreateTemp and is retained for this invocation only.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	const fileLimit = cronDefinitionRequestLimit + 64*1024
	raw, err := io.ReadAll(io.LimitReader(file, fileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > fileLimit {
		return nil, cronCLIValidation("edit buffer exceeds its byte limit; draft was not truncated")
	}
	return raw, nil
}

func writeCronEditFile(ctx context.Context, path string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Replace only after the complete private file is written, so a failed
	// recovery-header update leaves the editor's last saved draft intact.
	file, err := os.CreateTemp(filepath.Dir(path), ".kata-cron-edit-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	n, err := cronEditWriteTemp(file, raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicfile.Replace(file.Name(), path)
}
