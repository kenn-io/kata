package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"

	"go.kenn.io/kata/internal/cronhistory"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func (o cronCLIOptions) callHistory(ctx context.Context, client *kataclient.Client, projectID int64) ([]byte, error) {
	raw, err := o.historyPage(ctx, client, projectID, o.limit, o.beforeEventUID)
	if err != nil || !o.diff {
		return raw, err
	}
	enriched, err := cronhistory.WithDiff(ctx, raw, func(ctx context.Context, cursor string) ([]byte, error) {
		return o.historyPage(ctx, client, projectID, 1, cursor)
	})
	if errors.Is(err, cronhistory.ErrChanged) {
		return nil, &cliError{Message: err.Error(), Kind: kindInternal, ExitCode: ExitInternal}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(enriched)
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
