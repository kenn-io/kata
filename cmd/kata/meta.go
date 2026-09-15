package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
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
	var ifValue string
	var ifAbsent bool
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
			guard, err := metaSetGuardFromFlags(cmd, args[1], ifValue, ifAbsent, jsonValue)
			if err != nil {
				return err
			}
			return runMetaPatchGuarded(cmd, args[0], args[1], value, ifMatch, "set", guard)
		},
	}
	cmd.Flags().BoolVar(&jsonValue, "json-value", false, "treat value and --if-value as raw JSON")
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected issue revision (N or rev-N)")
	cmd.Flags().StringVar(&ifValue, "if-value", "", "apply only when the key has this value")
	cmd.Flags().BoolVar(&ifAbsent, "if-absent", false, "apply only when the key does not exist")
	return cmd
}

func newMetaUnsetCmd() *cobra.Command {
	var ifMatch string
	var ifValue string
	var jsonValue bool
	cmd := &cobra.Command{
		Use:   "unset <ref> <key>",
		Short: "clear issue metadata",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateMetaIfMatchFlag(cmd, ifMatch); err != nil {
				return err
			}
			guard, err := metaUnsetGuardFromFlags(cmd, args[1], ifValue, jsonValue)
			if err != nil {
				return err
			}
			return runMetaPatchGuarded(cmd, args[0], args[1], jsontext.Value("null"), ifMatch, "unset", guard)
		},
	}
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "expected issue revision (N or rev-N)")
	cmd.Flags().StringVar(&ifValue, "if-value", "", "clear only when the key has this value")
	cmd.Flags().BoolVar(&jsonValue, "json-value", false, "treat --if-value as raw JSON")
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
// (`{"issue": {...}}`) used by meta, wait, and the attention hook: short_id,
// status, metadata, and revision are the only fields those consumers read.
// show.go keeps its own richer struct for the full `kata show` surface. Status
// is unused by meta but lets waitFetchState and liveAttnDaemon.lookup decode the
// lifecycle status through the same struct instead of anonymous copies.
type metaIssueWire struct {
	ShortID  string                    `json:"short_id"`
	Status   string                    `json:"status"`
	Metadata map[string]jsontext.Value `json:"metadata"`
	Revision int64                     `json:"revision"`
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
	Ref      string                    `json:"ref"`
	Revision int64                     `json:"revision"`
	Metadata map[string]jsontext.Value `json:"metadata"`
}

// metaGetKeyJSON is the CLI-composed --json envelope for `kata meta get
// <ref> <key>`.
type metaGetKeyJSON struct {
	Ref      string         `json:"ref"`
	Revision int64          `json:"revision"`
	Key      string         `json:"key"`
	Value    jsontext.Value `json:"value"`
}

func parseMetaSetValue(raw string, asJSON bool) (jsontext.Value, error) {
	return parseMetaValue(raw, asJSON,
		"null is not allowed with --json-value; use `kata meta unset` to clear a key")
}

func parseMetaGuardValue(raw string, asJSON bool) (jsontext.Value, error) {
	return parseMetaValue(raw, asJSON,
		"null is not allowed for --if-value; use --if-absent to match a missing key")
}

func parseMetaValue(raw string, asJSON bool, nullMessage string) (jsontext.Value, error) {
	if !asJSON {
		bs, err := json.Marshal(raw)
		return jsontext.Value(bs), err
	}
	dec := jsontext.NewDecoder(strings.NewReader(raw))
	var v jsontext.Value
	if err := json.UnmarshalDecode(dec, &v); err != nil {
		return nil, &cliError{
			Message:  "invalid JSON for --json-value: " + err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	var extra any
	if err := json.UnmarshalDecode(dec, &extra); err == nil {
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
	if v.Kind() == 'n' {
		return nil, &cliError{
			Message:  nullMessage,
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	compact := jsontext.Value(raw)
	if err := compact.Compact(); err != nil {
		return nil, &cliError{
			Message:  "invalid JSON for --json-value: " + err.Error(),
			Kind:     kindValidation,
			ExitCode: ExitValidation,
		}
	}
	return compact, nil
}

type metaPatchGuard struct {
	Key      string  `json:"key"`
	IfValue  *string `json:"if_value,omitempty"`
	IfAbsent bool    `json:"if_absent,omitempty,omitzero"`
}

func metaSetGuardFromFlags(
	cmd *cobra.Command,
	key, ifValue string,
	ifAbsent, asJSON bool,
) (*metaPatchGuard, error) {
	hasValue := cmd.Flags().Changed("if-value")
	hasAbsent := cmd.Flags().Changed("if-absent")
	if hasAbsent && !ifAbsent {
		return nil, &cliError{
			Message: "--if-absent=false is not allowed; omit --if-absent for an unconditional write",
			Kind:    kindValidation, ExitCode: ExitValidation,
		}
	}
	if hasValue && ifAbsent {
		return nil, &cliError{
			Message: "--if-value and --if-absent are mutually exclusive",
			Kind:    kindValidation, ExitCode: ExitValidation,
		}
	}
	if ifAbsent {
		return &metaPatchGuard{Key: key, IfAbsent: true}, nil
	}
	if !hasValue {
		return nil, nil
	}
	expected, err := parseMetaGuardValue(ifValue, asJSON)
	if err != nil {
		return nil, err
	}
	expectedJSON := string(expected)
	return &metaPatchGuard{Key: key, IfValue: &expectedJSON}, nil
}

func metaUnsetGuardFromFlags(
	cmd *cobra.Command,
	key, ifValue string,
	asJSON bool,
) (*metaPatchGuard, error) {
	if !cmd.Flags().Changed("if-value") {
		if asJSON {
			return nil, &cliError{
				Message: "--json-value on meta unset requires --if-value",
				Kind:    kindValidation, ExitCode: ExitValidation,
			}
		}
		return nil, nil
	}
	expected, err := parseMetaGuardValue(ifValue, asJSON)
	if err != nil {
		return nil, err
	}
	expectedJSON := string(expected)
	return &metaPatchGuard{Key: key, IfValue: &expectedJSON}, nil
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

func runMetaPatch(cmd *cobra.Command, rawRef, key string, value jsontext.Value, ifMatch, verb string) error {
	return runMetaPatchGuarded(cmd, rawRef, key, value, ifMatch, verb, nil)
}

func runMetaPatchGuarded(
	cmd *cobra.Command,
	rawRef, key string,
	value jsontext.Value,
	ifMatch, verb string,
	guard *metaPatchGuard,
) error {
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
	options := &generated.PatchIssueMetadataRequestOptions{
		PathParams: &generated.PatchIssueMetadataPath{ProjectID: pid, Ref: ref.RefForAPI},
		Body:       &generated.PatchIssueMetadataBody{Patch: map[string]any{key: value}},
	}
	if strings.TrimSpace(ifMatch) != "" {
		etag, err := normalizeMetaIfMatch(ifMatch)
		if err != nil {
			return err
		}
		options.Header = &generated.PatchIssueMetadataHeaders{IfMatch: &etag}
	}
	actor, _ := resolveActor(ctx, flags.As, nil)
	options.Body.Actor = &actor
	if guard != nil {
		encoded, err := json.Marshal(guard)
		if err != nil {
			return err
		}
		options.Body.Guard = new(generated.MetadataPatchGuard)
		if err := json.Unmarshal(encoded, options.Body.Guard); err != nil {
			return err
		}
	}
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return err
	}
	response, callErr := apiClient.PatchIssueMetadataWithResponse(ctx, options)
	if response == nil {
		return callErr
	}
	if response.StatusCode >= 400 {
		return metaAPIError(response.StatusCode, response.Body)
	}
	if callErr != nil {
		return callErr
	}
	bs := response.Body
	return printMetaPatch(cmd, bs, verb, key)
}

func fetchMetaIssue(ctx context.Context, client *http.Client, baseURL string, pid int64, ref string) (metaIssueWire, []byte, error) {
	apiClient, err := kataclient.NewWithHTTPClient(baseURL, client)
	if err != nil {
		return metaIssueWire{}, nil, err
	}
	response, callErr := apiClient.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: pid, Ref: ref},
	})
	if err := externalCLITransportError(response, callErr); err != nil {
		return metaIssueWire{}, nil, err
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return metaIssueWire{}, nil, err
	}
	bs := response.Body
	var out metaShowResponse
	if err := json.Unmarshal(bs, &out); err != nil {
		return metaIssueWire{}, nil, err
	}
	if out.Issue.Metadata == nil {
		out.Issue.Metadata = map[string]jsontext.Value{}
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
		if err := emitJSON(&buf, jsontext.Value(bs)); err != nil {
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

func printMetaValue(cmd *cobra.Command, key string, value jsontext.Value, issue metaIssueWire, mode outputMode) error {
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

func writeMetaAgentRow(cmd *cobra.Command, key string, value jsontext.Value) error {
	// Route both fields through agent quoting so a JSON value containing
	// spaces, quotes, or backslashes stays a single unambiguous token that a
	// whitespace-splitting agent parser cannot break apart.
	return writeAgentKVRow(cmd.OutOrStdout(),
		agentRowField("key", key),
		agentRowField("value", string(value)))
}

func sortedMetaKeys(values map[string]jsontext.Value) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func compactRaw(raw jsontext.Value) jsontext.Value {
	buf := raw.Clone()
	if err := buf.Compact(); err != nil {
		return raw
	}
	return buf
}
