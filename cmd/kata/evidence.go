package main

import (
	"encoding/json/jsontext"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newEvidenceCmd() *cobra.Command {
	var event string
	var index int64
	var historical, content bool
	cmd := &cobra.Command{Use: "evidence <issue-ref>", Short: "retrieve or compare a retained file evidence reference", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		ctx, base, pid, issue, err := resolveIssueRefForCommand(cmd, args[0])
		if err != nil {
			return err
		}
		httpClient, err := httpClientFor(ctx, base)
		if err != nil {
			return err
		}
		clone := *httpClient
		clone.Timeout = envHTTPTimeout(30 * time.Second)
		if err := requireDaemonAPIVersion(ctx, &clone, base, apiVersionFileEvidence, "file evidence"); err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(base, &clone)
		if err != nil {
			return err
		}
		response, callErr := client.ReadFileEvidenceWithResponse(ctx, &generated.ReadFileEvidenceRequestOptions{PathParams: &generated.ReadFileEvidencePath{ProjectID: pid, Ref: issue.RefForAPI}, Query: &generated.ReadFileEvidenceQuery{EventUID: &event, Index: &index, Historical: &historical, Content: &content}})
		if err := externalCLITransportError(response, callErr); err != nil {
			return err
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		if currentOutputMode() != outputHuman {
			return emitJSON(cmd.OutOrStdout(), jsontext.Value(response.Body))
		}
		r := response.JSON200
		if r == nil {
			return fmt.Errorf("file evidence response missing")
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s:%s (event %s, file %d)\n", textsafe.Line(r.Status), textsafe.Line(r.Reference.Source), textsafe.Line(r.Reference.Path), r.EventUID, r.Index)
		if err != nil {
			return err
		}
		if r.Reference.Origin != nil {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Origin:", textsafe.Line(*r.Reference.Origin))
		}
		if err != nil {
			return err
		}
		if r.Reference.Sha256 != nil {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "SHA256:", *r.Reference.Sha256)
		}
		if err != nil {
			return err
		}
		if content && r.Content != nil {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Content (base64):", *r.Content)
		}
		return err
	}}
	cmd.Flags().StringVar(&event, "event", "", "retained close event UID; default effective current close")
	cmd.Flags().Int64Var(&index, "index", 0, "zero-based index among file evidence entries only")
	cmd.Flags().BoolVar(&historical, "historical", false, "request the recorded version; never substitute current bytes")
	cmd.Flags().BoolVar(&content, "content", false, "include complete bytes encoded as base64")
	return cmd
}
