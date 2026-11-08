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
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/transcriptservice"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func newTranscriptCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "transcript", Short: "inspect project transcript routing"}
	cmd.AddCommand(&cobra.Command{
		Use: "target <issue-ref>", Short: "inspect the project's configured root transcript service without reading a session",
		Long: "Inspect root-owned transcript policy through the selected project's negotiated relay route.\nThis reads no archive or receiver credential and uploads nothing. Configured policy is not proof of receiver availability or indexing.",
		Args: cobra.ExactArgs(1),
		RunE: runTranscriptTarget,
	})
	return cmd
}

func runTranscriptTarget(cmd *cobra.Command, args []string) error {
	ctx, base, pid, ref, err := resolveIssueRefForCommand(cmd, args[0])
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, base)
	if err != nil {
		return err
	}
	issue, _, err := fetchMetaIssue(ctx, client, base, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	if issue.ProjectID <= 0 || issue.UID == "" {
		return errors.New("transcript target: resolved issue identity unavailable")
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
		return errors.New("transcript target: project identity unavailable")
	}
	discovery, err := fetchTranscriptTarget(ctx, client, base, issue.ProjectID, project.Project.UID)
	if err != nil {
		return err
	}
	current, _, err := fetchMetaIssue(ctx, client, base, pid, ref.RefForAPI)
	if err != nil {
		return err
	}
	if current.UID != issue.UID || current.ProjectID != issue.ProjectID {
		return errors.New("transcript target: issue project changed; inspect again")
	}
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), struct {
			State  string                      `json:"state"`
			Target transcriptservice.Discovery `json:"target"`
		}{"configured_only", discovery})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Project: %s\nRoot: %s\nService: %s\nLocator: %s\nRoute nodes: %d\nConfigured policy only; no transcript read, upload, or indexing proof.\n", project.Project.UID, discovery.Descriptor.AuthorityUID, discovery.Descriptor.ServiceID, discovery.Descriptor.LocatorBase, len(discovery.Route))
	return err
}

func fetchTranscriptTarget(ctx context.Context, client *http.Client, base string, pid int64, project string) (transcriptservice.Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint, err := httpurl.AppendHTTPBaseURLPath(base, strings.Replace(api.TranscriptServicePath, "{project_id}", strconv.FormatInt(pid, 10), 1))
	if err != nil {
		return transcriptservice.Discovery{}, errors.New("transcript target: invalid selected daemon")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return transcriptservice.Discovery{}, err
	}
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := bounded.Do(req) //nolint:gosec // selected daemon client retains its credential-pinning transport
	if err != nil {
		return transcriptservice.Discovery{}, errors.New("transcript target: selected daemon unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return transcriptservice.Discovery{}, fmt.Errorf("transcript target: service unavailable (HTTP %d); no fallback", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, transcriptservice.MaxDiscoveryBytes+1))
	if err != nil {
		return transcriptservice.Discovery{}, errors.New("transcript target: incomplete service response")
	}
	return transcriptservice.ParseDiscovery(raw, project, nil, time.Now())
}
