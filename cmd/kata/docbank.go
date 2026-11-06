package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func docbankBrowseAPI(ctx context.Context) (daemonAPI, int64, error) {
	ctx, base, err := ensureDaemonContext(ctx)
	if err != nil {
		return daemonAPI{}, 0, err
	}
	client, err := httpClientFor(ctx, base)
	if err != nil {
		return daemonAPI{}, 0, err
	}
	a := daemonAPI{ctx: ctx, baseURL: base, client: client}
	// Gate before project resolution or either new browse route.
	if err := requireDaemonAPIVersion(ctx, client, base, apiVersionDocbankBrowse, "Docbank browsing"); err != nil {
		return a, 0, err
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return a, 0, err
	}
	pid, _, err := resolveProjectIDAndNameWithClient(a, start)
	return a, pid, err
}

func newDocbankListCmd() *cobra.Command {
	var limit, offset int64
	cmd := &cobra.Command{Use: "ls [path]", Short: "list one page from the current project's Docbank folder", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if limit < 1 || limit > 200 || offset < 0 {
			return externalCLIValidation("limit must be 1–200 and offset must be nonnegative")
		}
		a, pid, err := docbankBrowseAPI(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		query := &generated.DocbankChildrenQuery{Limit: &limit, Offset: &offset}
		if len(args) == 1 {
			query.Path = new(args[0])
		}
		response, callErr := client.DocbankChildrenWithResponse(a.ctx, &generated.DocbankChildrenRequestOptions{PathParams: &generated.DocbankChildrenPath{ProjectID: pid}, Query: query})
		if response == nil {
			return externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		if response.JSON200 == nil {
			return fmt.Errorf("docbank listing: empty response")
		}
		page := response.JSON200
		if currentOutputMode() == outputJSON {
			return emitJSON(cmd.OutOrStdout(), page)
		}
		if currentOutputMode() == outputAgent {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-ls origin=%s path=%s total=%d limit=%d offset=%d count=%d\n", agentValue(page.Origin), agentValue(page.Directory.Path), page.Total, page.Limit, page.Offset, len(page.Items))
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s (%d entries; offset %d)\n", textsafe.Line(page.Directory.Path), page.Total, page.Offset)
		}
		if err != nil {
			return err
		}
		for _, node := range page.Items {
			if err := printDocbankBrowseNode(cmd.OutOrStdout(), node); err != nil {
				return err
			}
		}
		return nil
	}}
	cmd.Flags().Int64Var(&limit, "limit", 100, "maximum entries in one page (1–200)")
	cmd.Flags().Int64Var(&offset, "offset", 0, "number of entries to skip")
	return cmd
}

func newDocbankResolveCmd() *cobra.Command {
	return &cobra.Command{Use: "resolve <path>", Short: "resolve a file or directory in the current project's Docbank folder", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, pid, err := docbankBrowseAPI(cmd.Context())
		if err != nil {
			return err
		}
		client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
		if err != nil {
			return err
		}
		response, callErr := client.DocbankResolveWithResponse(a.ctx, &generated.DocbankResolveRequestOptions{PathParams: &generated.DocbankResolvePath{ProjectID: pid}, Query: &generated.DocbankResolveQuery{Path: args[0]}})
		if response == nil {
			return externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return err
		}
		if response.JSON200 == nil {
			return fmt.Errorf("docbank resolution: empty response")
		}
		if currentOutputMode() == outputJSON {
			return emitJSON(cmd.OutOrStdout(), response.JSON200)
		}
		if currentOutputMode() == outputAgent {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK docbank-resolve origin=%s\n", agentValue(response.JSON200.Origin)); err != nil {
				return err
			}
		}
		return printDocbankBrowseNode(cmd.OutOrStdout(), response.JSON200.Node)
	}}
}

func printDocbankBrowseNode(out io.Writer, node generated.DocbankBrowseNode) error {
	if currentOutputMode() == outputAgent {
		version := ""
		if node.CurrentVersionID != nil {
			version = node.CurrentVersionID.String()
		}
		_, err := fmt.Fprintf(out, "- id=%d kind=%s path=%s size=%d current_version_id=%s\n", node.ID, agentValue(string(node.Kind)), agentValue(node.Path), node.Size, agentValue(version))
		return err
	}
	_, err := fmt.Fprintf(out, "%s  %s  %d bytes\n", textsafe.Line(string(node.Kind)), textsafe.Line(node.Path), node.Size)
	return err
}
