package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type inboxWatchRequest struct {
	inboxRequest
	IssueUID   string `json:"issue_uid"`
	ProjectUID string `json:"project_uid"`
	Oversized  bool   `json:"oversized,omitzero"`

	value notification.Value
}

type inboxWatchIdentity struct{ project, issue string }
type inboxWatchFingerprint struct {
	value     notification.Value
	oversized bool
}
type inboxWatchState map[inboxWatchIdentity]inboxWatchFingerprint

// inboxWatchChanges coalesces each identity to its final observation in a scan.
// Presentation fields never turn an unchanged pending request into a wakeup.
func inboxWatchChanges(previous inboxWatchState, requests []inboxWatchRequest) ([]inboxWatchRequest, inboxWatchState) {
	latest := make(map[inboxWatchIdentity]inboxWatchRequest, len(requests))
	order := make([]inboxWatchIdentity, 0, len(requests))
	for _, request := range requests {
		key := inboxWatchIdentity{request.ProjectUID, request.IssueUID}
		if _, exists := latest[key]; !exists {
			order = append(order, key)
		}
		latest[key] = request
	}
	next := make(inboxWatchState, len(latest))
	var changed []inboxWatchRequest
	for _, key := range order {
		request := latest[key]
		fingerprint := inboxWatchFingerprint{value: request.value, oversized: request.Oversized}
		if request.Oversized {
			// Identity-only rows cannot reveal notification replacements.
			fingerprint.value = notification.Value{}
		}
		next[key] = fingerprint
		if old, exists := previous[key]; !exists || old != fingerprint {
			changed = append(changed, request)
		}
	}
	return changed, next
}

func watchInbox(ctx context.Context, interval time.Duration, read func(context.Context) ([]inboxWatchRequest, error), emit func([]inboxWatchRequest) error) error {
	var previous inboxWatchState
	for {
		if ctx.Err() != nil {
			return nil
		}
		requests, err := read(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		changed, next := inboxWatchChanges(previous, requests)
		if len(changed) > 0 {
			if err := emit(changed); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
		previous = next
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func runInboxWatch(ctx context.Context, cmd *cobra.Command, recipient string, allProjects bool, interval time.Duration) error {
	if ctx.Err() != nil {
		return nil
	}
	read, closeIdle, err := newInboxWatchReader(ctx, cmd, recipient, allProjects)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer closeIdle()
	return watchInbox(ctx, interval, read, func(requests []inboxWatchRequest) error {
		return printInboxWatch(ctx, cmd, recipient, requests, allProjects)
	})
}

func newInboxWatchReader(ctx context.Context, cmd *cobra.Command, recipient string, allProjects bool) (read func(context.Context) ([]inboxWatchRequest, error), closeIdle func(), err error) {
	api, err := dialDaemon(ctx)
	if err != nil {
		return nil, nil, err
	}
	return inboxWatchReaderForAPI(api.ctx, api, cmd, recipient, allProjects)
}

func inboxWatchReaderForAPI(ctx context.Context, api daemonAPI, cmd *cobra.Command, recipient string, allProjects bool) (read func(context.Context) ([]inboxWatchRequest, error), closeIdle func(), err error) {
	defer func() {
		if err != nil {
			api.client.CloseIdleConnections()
		}
	}()
	apiClient, err := kataclient.NewWithHTTPClient(api.baseURL, api.client)
	if err != nil {
		return nil, nil, err
	}
	if err := requireDaemonAPIVersion(ctx, api.client, api.baseURL, apiVersionInboxWatch, "inbox --watch"); err != nil {
		return nil, nil, err
	}
	var pid int64
	if !allProjects {
		start, err := resolveStartPath(flags.Workspace)
		if err != nil {
			return nil, nil, err
		}
		pid, _, err = resolveProjectIDAndNameWithClient(api, start)
		if err != nil {
			return nil, nil, err
		}
	}
	key := notification.MetadataKey(recipient)
	warned := make(map[inboxWatchIdentity]string)
	return func(ctx context.Context) ([]inboxWatchRequest, error) {
		currentWarnings := make(map[inboxWatchIdentity]string)
		warn := func(ctx context.Context, request inboxWatchRequest, raw jsontext.Value, note string) {
			identity := inboxWatchIdentity{request.ProjectUID, request.IssueUID}
			value := string(raw)
			previous, repeated := warned[identity]
			current, duplicate := currentWarnings[identity]
			currentWarnings[identity] = value
			if flags.Quiet || ctx.Err() != nil || (repeated && previous == value) || (duplicate && current == value) {
				return
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s on %s\n", note, textsafe.Line(request.Ref))
		}
		cursor := ""
		visited := map[string]bool{"": true}
		var requests []inboxWatchRequest
		for {
			var body []byte
			if allProjects {
				response, callErr := apiClient.ListAllIssuesWithResponse(ctx, &generated.ListAllIssuesRequestOptions{
					Query: &generated.ListAllIssuesQuery{Status: new(generated.ListAllIssuesQueryStatus("open")), Meta: []string{key}, Compact: new(true), Sort: new(generated.ListAllIssuesQuerySort("oldest")), Limit: new(int64(100)), Cursor: &cursor},
				})
				if response == nil {
					return nil, externalCLITransportError(response, callErr)
				}
				if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
					return nil, err
				}
				body = response.Body
			} else {
				response, callErr := apiClient.ListIssuesWithResponse(ctx, &generated.ListIssuesRequestOptions{
					PathParams: &generated.ListIssuesPath{ProjectID: pid},
					Query:      &generated.ListIssuesQuery{Status: new(generated.ListIssuesQueryStatus("open")), Meta: []string{key}, Compact: new(true), Sort: new(generated.ListIssuesQuerySort("oldest")), Limit: new(int64(100)), Cursor: &cursor},
				})
				if response == nil {
					return nil, externalCLITransportError(response, callErr)
				}
				if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
					return nil, err
				}
				body = response.Body
			}
			var page struct {
				Complete   *bool  `json:"complete"`
				Truncated  *bool  `json:"truncated"`
				NextCursor string `json:"next_cursor"`
				Issues     *[]struct {
					UID         string                    `json:"uid"`
					ProjectUID  string                    `json:"project_uid"`
					ShortID     string                    `json:"short_id"`
					QualifiedID string                    `json:"qualified_id"`
					ProjectName string                    `json:"project_name"`
					Title       string                    `json:"title"`
					Oversized   bool                      `json:"oversized"`
					Metadata    map[string]jsontext.Value `json:"metadata"`
				} `json:"issues"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				return nil, err
			}
			if page.Complete == nil || page.Truncated == nil || page.Issues == nil {
				return nil, fmt.Errorf("inbox watch received an incomplete response; check daemon compatibility")
			}
			for _, issue := range *page.Issues {
				if issue.UID == "" || issue.ProjectUID == "" || issue.ShortID == "" || (allProjects && issue.QualifiedID == "") {
					return nil, fmt.Errorf("inbox watch could not identify a returned issue")
				}
				request := inboxWatchRequest{IssueUID: issue.UID, ProjectUID: issue.ProjectUID, Oversized: issue.Oversized}
				request.Ref, request.Title = issue.ShortID, issue.Title
				if allProjects {
					request.Ref, request.Project = issue.QualifiedID, issue.ProjectName
				}
				if !issue.Oversized {
					value, invalidTeammate, err := notification.DecodeValue(issue.Metadata[key])
					if err != nil {
						warn(ctx, request, issue.Metadata[key], "skipped malformed notification")
						continue
					}
					if invalidTeammate {
						warn(ctx, request, issue.Metadata[key], "ignored invalid notification teammate")
					}
					request.value = value
					request.From, request.Teammate, request.Message = value.From, value.Teammate, value.Message
					request.Re, request.Kind, request.Broadcast = value.Re, value.Kind, value.Broadcast
				}
				requests = append(requests, request)
			}
			if *page.Complete && !*page.Truncated && page.NextCursor == "" {
				warned = currentWarnings
				return requests, nil
			}
			if *page.Complete || !*page.Truncated || page.NextCursor == "" || visited[page.NextCursor] {
				return nil, fmt.Errorf("inbox watch received incomplete or inconsistent results; check daemon compatibility")
			}
			cursor = page.NextCursor
			visited[cursor] = true
		}
	}, api.client.CloseIdleConnections, nil
}

func printInboxWatch(ctx context.Context, cmd *cobra.Command, recipient string, requests []inboxWatchRequest, allProjects bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), struct {
			Recipient   string              `json:"recipient"`
			AllProjects bool                `json:"all_projects,omitzero"`
			Requests    []inboxWatchRequest `json:"requests"`
		}{recipient, allProjects, requests})
	}
	rows := make([]inboxRequest, 0, len(requests))
	for _, request := range requests {
		row := request.inboxRequest
		if request.Oversized {
			row.Title = "[oversized attention request]"
			row.From = "unknown"
			row.Message = "Notification text is unavailable; inspect this request manually."
		}
		rows = append(rows, row)
	}
	return printInbox(cmd, recipient, rows, allProjects)
}
