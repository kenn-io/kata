package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

func newMetaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta",
		Short: "read and write issue metadata",
	}
	cmd.AddCommand(newMetaSetCmd(), newMetaUnsetCmd(), newMetaGetCmd())
	return cmd
}

func newMetaSetCmd() *cobra.Command {
	var jsonValue bool
	var ifMatch string
	cmd := &cobra.Command{
		Use:   "set <ref> <key> <value>",
		Short: "set issue metadata",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			value, err := parseMetaSetValue(args[2], jsonValue)
			if err != nil {
				return err
			}
			if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
				return err
			}
			return runMetaPatch(cmd, args[0], args[1], value, ifMatch, "set")
		},
	}
	cmd.Flags().BoolVar(&jsonValue, "json-value", false, "treat value as raw JSON")
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected issue revision (N or rev-N)")
	return cmd
}

func newMetaUnsetCmd() *cobra.Command {
	var ifMatch string
	cmd := &cobra.Command{
		Use:   "unset <ref> <key>",
		Short: "clear issue metadata",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
				return err
			}
			return runMetaPatch(cmd, args[0], args[1], json.RawMessage("null"), ifMatch, "unset")
		},
	}
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected issue revision (N or rev-N)")
	return cmd
}

func newMetaGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <ref> [key]",
		Short: "get issue metadata",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, args[0])
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			issue, _, err := fetchMetaIssue(ctx, client, baseURL, pid, ref.RefForAPI)
			if err != nil {
				return err
			}
			key := ""
			if len(args) == 2 {
				key = args[1]
			}
			return printMetaGet(cmd, issue, key)
		},
	}
}

// metaIssueWire is the minimal shared decode of the show-issue response body
// (`{"issue": {...}}`) used by both meta and wait: short_id, status, metadata,
// and revision are the only fields either command reads. show.go keeps its own
// richer struct for the full `kata show` surface. Status is unused by meta but
// carried here so waitFetchState can decode the lifecycle status through the
// same struct instead of a second anonymous one.
type metaIssueWire struct {
	ShortID  string                     `json:"short_id"`
	Status   string                     `json:"status"`
	Metadata map[string]json.RawMessage `json:"metadata"`
	Revision int64                      `json:"revision"`
}

type metaShowResponse struct {
	Issue metaIssueWire `json:"issue"`
}

type metaPatchResponse struct {
	Issue   metaIssueWire `json:"issue"`
	Changed bool          `json:"changed"`
}

// metaGetWholeJSON is the CLI-composed --json envelope for `kata meta get
// <ref>` (no key given). It mirrors the daemon's kata_api_version envelope
// convention (see helpers.go: emitJSON) even though this payload is
// assembled client-side rather than re-emitted from a daemon response body.
type metaGetWholeJSON struct {
	Ref      string                     `json:"ref"`
	Revision int64                      `json:"revision"`
	Metadata map[string]json.RawMessage `json:"metadata"`
}

// metaGetKeyJSON is the CLI-composed --json envelope for `kata meta get
// <ref> <key>`.
type metaGetKeyJSON struct {
	Ref      string          `json:"ref"`
	Revision int64           `json:"revision"`
	Key      string          `json:"key"`
	Value    json.RawMessage `json:"value"`
}

func parseMetaSetValue(raw string, asJSON bool) (json.RawMessage, error) {
	if !asJSON {
		bs, err := json.Marshal(raw)
		return json.RawMessage(bs), err
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &cliError{
			Message:  "invalid JSON for --json-value: " + err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, &cliError{
			Message:  "invalid JSON for --json-value: trailing data",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	} else if err != io.EOF {
		return nil, &cliError{
			Message:  "invalid JSON for --json-value: " + err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	if v == nil {
		return nil, &cliError{
			Message:  "null is not allowed with --json-value; use `kata meta unset` to clear a key",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil {
		return nil, &cliError{
			Message:  "invalid JSON for --json-value: " + err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return json.RawMessage(compact.Bytes()), nil
}

// validateMetaIfMatchFlag distinguishes an absent --if-match (unconditional
// write, the deliberate default) from a present-but-blank value, which is
// almost always a scripting bug (e.g. an unset shell variable interpolated
// into the flag) rather than an intentional unconditional request. The
// daemon already rejects a present-but-empty If-Match header the same way
// (see internal/daemon/handlers_metadata.go); this catches it client-side
// before any request is sent.
func validateMetaIfMatchFlag(cmd *cobra.Command, ifMatch string) error {
	if cmd.Flags().Changed("if-match") && strings.TrimSpace(ifMatch) == "" {
		return &cliError{
			Message:  "--if-match must not be blank; omit the flag for an unconditional write",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return nil
}

func runMetaPatch(cmd *cobra.Command, rawRef, key string, value json.RawMessage, ifMatch, verb string) error {
	if strings.TrimSpace(key) == "" {
		return &cliError{Message: "metadata key must not be empty", Kind: kindValidation, ExitCode: ExitValidation}
	}
	ctx, baseURL, pid, ref, err := resolveIssueRefForCommand(cmd, rawRef)
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	// Without --if-match the patch is deliberately unconditional:
	// last-write-wins is the intended default for convention keys like
	// work.attention, so no revision is fetched and no If-Match is sent.
	headers := map[string]string{}
	if strings.TrimSpace(ifMatch) != "" {
		etag, err := normalizeMetaIfMatch(ifMatch)
		if err != nil {
			return err
		}
		headers["If-Match"] = etag
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	status, bs, err := httpDoJSONHeaders(ctx, client, http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", baseURL, pid, url.PathEscape(ref.RefForAPI)),
		map[string]any{
			"actor": actor,
			"patch": map[string]json.RawMessage{key: value},
		},
		headers)
	if err != nil {
		return err
	}
	if status >= 400 {
		return metaAPIError(status, bs)
	}
	return printMetaPatch(cmd, bs, verb, key)
}

func fetchMetaIssue(ctx context.Context, client *http.Client, baseURL string, pid int64, ref string) (metaIssueWire, []byte, error) {
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", baseURL, pid, url.PathEscape(ref)), nil)
	if err != nil {
		return metaIssueWire{}, nil, err
	}
	if status >= 400 {
		return metaIssueWire{}, nil, apiErrFromBody(status, bs)
	}
	var out metaShowResponse
	if err := json.Unmarshal(bs, &out); err != nil {
		return metaIssueWire{}, nil, err
	}
	if out.Issue.Metadata == nil {
		out.Issue.Metadata = map[string]json.RawMessage{}
	}
	return out.Issue, bs, nil
}

func normalizeMetaIfMatch(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"`)
	raw = strings.TrimPrefix(raw, "rev-")
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return "", &cliError{
			Message:  "--if-match must be a revision like 7 or rev-7",
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return fmt.Sprintf(`"rev-%d"`, n), nil
}

func metaAPIError(status int, bs []byte) *cliError {
	err := apiErrFromBody(status, bs)
	if status == http.StatusPreconditionFailed {
		err.Message = "revision conflict: " + err.Message
	}
	if err.Code != "" && !strings.Contains(err.Message, err.Code) {
		err.Message = err.Code + ": " + err.Message
	}
	return err
}

func printMetaPatch(cmd *cobra.Command, bs []byte, verb, key string) error {
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	var out metaPatchResponse
	if err := json.Unmarshal(bs, &out); err != nil {
		return err
	}
	rev := fmt.Sprintf("rev-%d", out.Issue.Revision)
	if mode == outputAgent {
		if flags.Quiet {
			return nil
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK meta %s %s key=%s revision=%s changed=%t\n",
			textsafe.Line(verb), agentValue(out.Issue.ShortID), agentValue(key), rev, out.Changed); err != nil {
			return err
		}
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("ref", out.Issue.ShortID),
			agentRowField("key", key),
			agentRowField("revision", rev),
			agentRowField("changed", strconv.FormatBool(out.Changed)),
		)
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "meta %s %s %s %s\n",
		textsafe.Line(verb), textsafe.Line(out.Issue.ShortID), textsafe.Line(key), textsafe.Line(rev))
	return err
}

func printMetaGet(cmd *cobra.Command, issue metaIssueWire, key string) error {
	mode := currentOutputMode()
	if key != "" {
		value, ok := issue.Metadata[key]
		if !ok {
			return &cliError{
				Message:  fmt.Sprintf("metadata key not found: %s", key),
				Kind:     kindNotFound,
				ExitCode: ExitNotFound,
			}
		}
		return printMetaValue(cmd, key, value, issue, mode)
	}
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, metaGetWholeJSON{
			Ref:      issue.ShortID,
			Revision: issue.Revision,
			Metadata: issue.Metadata,
		}); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK meta get %s count=%d revision=rev-%d\n",
			agentValue(issue.ShortID), len(issue.Metadata), issue.Revision); err != nil {
			return err
		}
		for _, k := range sortedMetaKeys(issue.Metadata) {
			if err := writeMetaAgentRow(cmd, k, compactRaw(issue.Metadata[k])); err != nil {
				return err
			}
		}
		return nil
	}
	if len(issue.Metadata) == 0 {
		if !flags.Quiet {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "no metadata")
			return err
		}
		return nil
	}
	for _, k := range sortedMetaKeys(issue.Metadata) {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s = %s\n",
			textsafe.Line(k), textsafe.Line(string(compactRaw(issue.Metadata[k])))); err != nil {
			return err
		}
	}
	return nil
}

func printMetaValue(cmd *cobra.Command, key string, value json.RawMessage, issue metaIssueWire, mode outputMode) error {
	compact := compactRaw(value)
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, metaGetKeyJSON{
			Ref:      issue.ShortID,
			Revision: issue.Revision,
			Key:      key,
			Value:    compact,
		}); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK meta get %s count=1 revision=rev-%d\n",
			agentValue(issue.ShortID), issue.Revision); err != nil {
			return err
		}
		return writeMetaAgentRow(cmd, key, compact)
	}
	var s string
	if err := json.Unmarshal(compact, &s); err == nil {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), textsafe.Line(s))
		return err
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), textsafe.Line(string(compact)))
	return err
}

func writeMetaAgentRow(cmd *cobra.Command, key string, value json.RawMessage) error {
	// Route both fields through agent quoting so a JSON value containing
	// spaces, quotes, or backslashes stays a single unambiguous token that a
	// whitespace-splitting agent parser cannot break apart.
	return writeAgentKVRow(cmd.OutOrStdout(),
		agentRowField("key", key),
		agentRowField("value", string(value)))
}

func sortedMetaKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func compactRaw(raw json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(buf.Bytes())
}
