package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/mod/semver"
)

type attentionRequest struct {
	issue                                       Issue
	from, teammate, message, workAttention, key string
	re, kind                                    string
	broadcast                                   bool
	raw                                         jsontext.Value
}

type attentionAPI interface {
	ListAttentionRequests(context.Context, *int64, string) ([]attentionRequest, int, error)
	ClearAttentionRequest(context.Context, int64, string, string, jsontext.Value, string) error
}

// ListAttentionRequests uses a separate projection so attention metadata never
// enters the ordinary list cache. Filter again for older daemons ignoring queries.
//
//nolint:revive // The optional TUI-only interface deliberately keeps its request projection private.
func (c *Client) ListAttentionRequests(ctx context.Context, projectID *int64, recipient string) ([]attentionRequest, int, error) {
	apiClient, err := c.generatedClient()
	if err != nil {
		return nil, 0, err
	}
	key := notification.MetadataKey(recipient)
	var body []byte
	if projectID != nil {
		wire, callErr := apiClient.ListIssuesWithResponse(ctx, &generated.ListIssuesRequestOptions{PathParams: &generated.ListIssuesPath{ProjectID: *projectID}, Query: &generated.ListIssuesQuery{Status: new(generated.ListIssuesQueryStatus("open")), Limit: new(int64(0)), Meta: []string{key}}})
		if wire == nil {
			return nil, 0, callErr
		}
		if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
			return nil, 0, err
		}
		body = wire.Body
	} else {
		wire, callErr := apiClient.ListAllIssuesWithResponse(ctx, &generated.ListAllIssuesRequestOptions{Query: &generated.ListAllIssuesQuery{Status: new(generated.ListAllIssuesQueryStatus("open")), Limit: new(int64(0)), Meta: []string{key}}})
		if wire == nil {
			return nil, 0, callErr
		}
		if err := decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil); err != nil {
			return nil, 0, err
		}
		body = wire.Body
	}
	var list struct {
		Issues []struct {
			Issue
			Metadata map[string]jsontext.Value `json:"metadata"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, 0, err
	}
	rows := make([]attentionRequest, 0, len(list.Issues))
	skipped := 0
	for _, row := range list.Issues {
		raw, ok := row.Metadata[key]
		if !ok || row.Status != "open" {
			continue
		}
		value, _, err := notification.DecodeValue(raw)
		if err != nil {
			skipped++
			continue
		}
		var work string
		_ = json.Unmarshal(row.Metadata["work.attention"], &work)
		if work != "needs-human" && work != "stuck" && work != "ok" {
			work = ""
		}
		rows = append(rows, attentionRequest{
			issue: row.Issue, from: value.From, teammate: value.Teammate, message: value.Message,
			re: value.Re, kind: value.Kind, broadcast: value.Broadcast,
			workAttention: work, key: key, raw: raw,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if projectID == nil {
			return rows[i].issue.QualifiedID < rows[j].issue.QualifiedID
		}
		return rows[i].issue.ShortID < rows[j].issue.ShortID
	})
	return rows, skipped, nil
}

// ClearAttentionRequest acknowledges precisely the value the operator saw.
func (c *Client) ClearAttentionRequest(ctx context.Context, projectID int64, ref, key string, ifValue jsontext.Value, actor string) error {
	apiClient, err := c.generatedClient()
	if err != nil {
		return &mutationPreflightError{err}
	}
	health, err := apiClient.Health(ctx)
	if err != nil {
		return &mutationPreflightError{fmt.Errorf("clearing attention requires daemon API 0.13.0 or newer: %w", err)}
	}
	reported := ""
	if health.APISchemaVersion != nil {
		reported = *health.APISchemaVersion
	}
	if !semver.IsValid("v"+reported) || semver.Compare("v"+reported, "v0.13.0") < 0 {
		return &mutationPreflightError{fmt.Errorf("clearing attention requires daemon API 0.13.0 or newer; upgrade the daemon")}
	}
	encoded, err := json.Marshal(map[string]string{"key": key, "if_value": string(ifValue)})
	if err != nil {
		return &mutationPreflightError{err}
	}
	var guard generated.MetadataPatchGuard
	if err := json.Unmarshal(encoded, &guard); err != nil {
		return &mutationPreflightError{err}
	}
	wire, callErr := apiClient.PatchIssueMetadataWithResponse(ctx, &generated.PatchIssueMetadataRequestOptions{PathParams: &generated.PatchIssueMetadataPath{ProjectID: projectID, Ref: ref}, Body: &generated.PatchIssueMetadataBody{Actor: &actor, Patch: map[string]any{key: nil}, Guard: &guard}})
	if wire == nil {
		return callErr
	}
	return decodeGeneratedResponse(wire.HTTPResponse, wire.Body, callErr, nil)
}
