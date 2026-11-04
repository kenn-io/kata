package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"slices"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func (o cronCLIOptions) callHistory(ctx context.Context, client *kataclient.Client, projectID int64) ([]byte, error) {
	raw, err := o.historyPage(ctx, client, projectID, o.limit, o.beforeEventUID)
	if err != nil || !o.diff {
		return raw, err
	}
	var page db.CronVersionPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	var older *db.CronDefinitionVersion
	if page.NextBeforeEventUID != "" {
		raw, err := o.historyPage(ctx, client, projectID, 1, page.NextBeforeEventUID)
		if err != nil {
			return nil, err
		}
		var lookahead db.CronVersionPage
		if err := json.Unmarshal(raw, &lookahead); err != nil {
			return nil, err
		}
		if len(page.Versions) == 0 || len(lookahead.Versions) != 1 {
			return nil, &cliError{Message: "definition history changed while reading the previous version; retry history", Kind: kindInternal, ExitCode: ExitInternal}
		}
		older = &lookahead.Versions[0]
		page.SkippedEventUIDs = historySkippedEvents(page.SkippedEventUIDs, lookahead.SkippedEventUIDs)
	}
	type versionWithChanges struct {
		db.CronDefinitionVersion
		Changes jsontext.Value `json:"changes"`
	}
	out := struct {
		Versions           []versionWithChanges `json:"versions"`
		NextBeforeEventUID string               `json:"next_before_event_uid,omitempty"`
		SkippedEventUIDs   []string             `json:"skipped_event_uids,omitempty"`
	}{Versions: make([]versionWithChanges, 0, len(page.Versions)), NextBeforeEventUID: page.NextBeforeEventUID, SkippedEventUIDs: page.SkippedEventUIDs}
	for i, v := range page.Versions {
		previous := older
		if i+1 < len(page.Versions) {
			previous = &page.Versions[i+1]
		}
		changes := jsontext.Value(`null`)
		if previous != nil {
			before, err := cronHistoryDocument(*previous)
			if err != nil {
				return nil, err
			}
			after, err := cronHistoryDocument(v)
			if err != nil {
				return nil, err
			}
			fields, err := cron.DiffDocuments(before, after)
			if err != nil {
				return nil, err
			}
			changes, err = json.Marshal(fields)
			if err != nil {
				return nil, err
			}
		}
		out.Versions = append(out.Versions, versionWithChanges{CronDefinitionVersion: v, Changes: changes})
	}
	return json.Marshal(out)
}

func cronHistoryDocument(v db.CronDefinitionVersion) (jsontext.Value, error) {
	return json.Marshal(struct {
		Name       string         `json:"name"`
		Definition jsontext.Value `json:"definition"`
		DeletedAt  *time.Time     `json:"deleted_at,omitempty"`
	}{v.Name, v.Definition, v.DeletedAt})
}

func historySkippedEvents(first, second []string) []string {
	values := append(slices.Clone(first), second...)
	slices.Sort(values)
	values = slices.Compact(values)
	return values[:min(20, len(values))]
}

func (o cronCLIOptions) historyPage(ctx context.Context, client *kataclient.Client, projectID, limit int64, before string) ([]byte, error) {
	if o.resource == "job" {
		response, err := client.ListCronJobVersionsWithResponse(ctx, &generated.ListCronJobVersionsRequestOptions{PathParams: &generated.ListCronJobVersionsPath{ProjectID: projectID, CronUID: o.identity}, Query: &generated.ListCronJobVersionsQuery{Limit: &limit, BeforeEventUID: &before}})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronHistoryResponse(response.StatusCode, response.Body, err)
	}
	response, err := client.ListCronWorkflowVersionsWithResponse(ctx, &generated.ListCronWorkflowVersionsRequestOptions{PathParams: &generated.ListCronWorkflowVersionsPath{ProjectID: projectID, CronUID: o.identity}, Query: &generated.ListCronWorkflowVersionsQuery{Limit: &limit, BeforeEventUID: &before}})
	if response == nil {
		return nil, externalCLITransportError(response, err)
	}
	return cronHistoryResponse(response.StatusCode, response.Body, err)
}

func cronHistoryResponse(status int, raw []byte, err error) ([]byte, error) {
	if status == http.StatusNotFound {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) != nil || envelope.Error.Code == "" {
			return nil, &cliError{Message: "daemon does not support cron definition history; upgrade it", HTTPStatus: status, Kind: kindValidation, ExitCode: ExitValidation}
		}
	}
	return cronCLIResponse(status, raw, err)
}
