package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/agentsviewservice"
	"go.kenn.io/kata/internal/api"
	daemonclient "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/httpurl"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newAgentsViewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agentsview",
		Short: "inspect an AgentsView target through Kata's relay route",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "target <issue-ref>",
		Short: "inspect Kata's configured root AgentsView target (Kata-only command)",
		Long:  "This command is available only in Kata. Inspect the selected issue's root AgentsView destination. It reads no session or archive, contacts no receiver, and uploads nothing. Configured policy is not proof of receiver availability or indexing.",
		Args:  cobra.ExactArgs(1),
		RunE:  runAgentsViewTarget,
	})
	return cmd
}

func runAgentsViewTarget(cmd *cobra.Command, args []string) error {
	ctx, base, pid, ref, daemon, err := resolveIssueRefForReadOnlyCommand(cmd, args[0])
	if err != nil {
		return err
	}
	recordCLIUseTarget(daemon.resolved)
	client, err := httpClientForResolvedWithOpts(ctx, daemon.resolved, daemonclient.Opts{
		Timeout:                envHTTPTimeout(defaultHTTPTimeout),
		ResponseHeaderTimeout:  2 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		DisableCompression:     true,
	})
	if err != nil {
		return err
	}
	issue, _, err := fetchMetaIssue(ctx, client, base, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	if issue.ProjectID <= 0 || issue.UID == "" {
		return errors.New("AgentsView target: resolved issue identity unavailable")
	}
	apiClient, err := kataclient.NewWithHTTPClient(base, client)
	if err != nil {
		return err
	}
	response, callErr := apiClient.ShowProjectWithResponse(ctx, &generated.ShowProjectRequestOptions{PathParams: &generated.ShowProjectPath{ProjectID: issue.ProjectID}})
	if response == nil {
		return externalCLITransportError(response, callErr)
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return err
	}
	var project struct {
		Project struct {
			UID string `json:"uid"`
		} `json:"project"`
	}
	if err := json.Unmarshal(response.Body, &project); err != nil {
		return errors.New("AgentsView target: project identity unavailable")
	}
	discovery, err := fetchAgentsViewTarget(ctx, client, base, issue.ProjectID, project.Project.UID)
	if err != nil {
		return err
	}
	current, _, err := fetchMetaIssue(ctx, client, base, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	if current.UID != issue.UID || current.ProjectID != issue.ProjectID {
		return errors.New("AgentsView target: issue project changed; inspect again")
	}
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), struct {
			State  string                      `json:"state"`
			Target agentsviewservice.Discovery `json:"target"`
		}{"configured_only", discovery})
	}
	_, err = fmt.Fprint(cmd.OutOrStdout(), formatAgentsViewTarget(discovery))
	return err
}

func formatAgentsViewTarget(discovery agentsviewservice.Discovery) string {
	return fmt.Sprintf("Locator: %s\nMaximum bytes: %d\nExpires: %s\nConfigured policy only; no transcript read, upload, or indexing proof.\n",
		discovery.Descriptor.LocatorBase,
		discovery.Descriptor.MaxBytes,
		time.Unix(discovery.Descriptor.ExpiresAt, 0).UTC().Format(time.RFC3339),
	)
}

func fetchAgentsViewTarget(ctx context.Context, client *http.Client, base string, pid int64, project string) (agentsviewservice.Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint, err := httpurl.AppendHTTPBaseURLPath(base, strings.Replace(api.AgentsViewServicePath, "{project_id}", strconv.FormatInt(pid, 10), 1))
	if err != nil {
		return agentsviewservice.Discovery{}, errors.New("AgentsView target: invalid selected daemon")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return agentsviewservice.Discovery{}, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	bounded := *client
	bounded.Timeout = 10 * time.Second
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req) //nolint:gosec // selected daemon client retains its credential-pinning transport
	if err != nil {
		return agentsviewservice.Discovery{}, errors.New("AgentsView target: selected daemon unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return agentsviewservice.Discovery{}, fmt.Errorf("AgentsView target: service unavailable (HTTP %d); no fallback", resp.StatusCode)
	}
	encodings := resp.Header.Values("Content-Encoding")
	if resp.Uncompressed || len(encodings) > 1 || (len(encodings) == 1 && !strings.EqualFold(strings.TrimSpace(encodings[0]), "identity")) {
		return agentsviewservice.Discovery{}, errors.New("AgentsView target: compressed service response rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, agentsviewservice.MaxDiscoveryBytes+1))
	if err != nil {
		return agentsviewservice.Discovery{}, errors.New("AgentsView target: incomplete service response")
	}
	return agentsviewservice.ParseDiscovery(raw, project, nil, time.Now())
}
