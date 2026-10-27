package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/metadata"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newAttachmentLinkCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{Use: "link <issue> <node-id|/path>", Short: "link an existing Docbank file version", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		input, err := parseDocbankSelector(args[1])
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("version") {
			input.VersionID = &version
		}
		ctx, base, pid, ref, err := resolveIssueRefForCommand(cmd, args[0])
		if err != nil {
			return err
		}
		hc, err := longRunningClientFor(ctx, base)
		if err != nil {
			return err
		}
		if err := requireDaemonAPIVersion(ctx, hc, base, apiVersionAttachmentLink, "linking Docbank files"); err != nil {
			return err
		}
		actor, _ := resolveActor(ctx, flags.As, nil)
		input.Actor = &actor
		out, err := linkAttachmentCLI(ctx, hc, base, pid, ref.RefForAPI, input)
		if err != nil {
			return err
		}
		if currentOutputMode() == outputJSON {
			return emitJSON(cmd.OutOrStdout(), out)
		}
		if currentOutputMode() == outputAgent {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-attachments-link attachment_id=%s name=%s media_type=%s size=%d\n", agentValue(out.AttachmentID), agentValue(out.Attachment.Name), agentValue(out.Attachment.MediaType), out.Attachment.Size)
			return err
		}
		return printAttachmentsCLI(cmd, map[string]metadata.Attachment{out.AttachmentID: {Name: out.Attachment.Name, MediaType: out.Attachment.MediaType, Size: out.Attachment.Size}})
	}}
	cmd.Flags().StringVar(&version, "version", "", "immutable content-version UUID (defaults to current version)")
	return cmd
}

func linkAttachmentCLI(ctx context.Context, hc *http.Client, base string, pid int64, ref string, input generated.LinkAttachmentBody) (*generated.LinkAttachmentResponse, error) {
	apiClient, err := kataclient.NewWithHTTPClient(base, hc)
	if err != nil {
		return nil, err
	}
	response, callErr := apiClient.LinkAttachmentWithResponse(ctx, &generated.LinkAttachmentRequestOptions{
		PathParams: &generated.LinkAttachmentPath{ProjectID: pid, Ref: ref},
		Body:       &input,
	})
	if response == nil {
		return nil, externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, fmt.Errorf("link attachment: empty response")
	}
	return response.JSON200, nil
}

func parseDocbankSelector(file string) (generated.LinkAttachmentBody, error) {
	var input generated.LinkAttachmentBody
	if strings.HasPrefix(file, "/") {
		input.Path = new(file)
	} else {
		if file == "" || strings.ContainsFunc(file, func(r rune) bool { return r < '0' || r > '9' }) {
			return input, externalCLIValidation("Docbank file must be a positive node id or absolute path")
		}
		id, err := strconv.ParseInt(file, 10, 64)
		if err != nil || id <= 0 {
			return input, externalCLIValidation("Docbank node id must be a positive int64")
		}
		input.NodeID = new(id)
	}
	return input, nil
}
