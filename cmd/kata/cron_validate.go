package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func (o *cronCLIOptions) validationRequestBody(cmd *cobra.Command) ([]byte, error) {
	raw, err := o.readRequest(cmd)
	if err != nil {
		return nil, err
	}
	fields, err := api.DecodeCronValidationEnvelope(raw)
	if err != nil {
		return nil, cronCLIValidation(err.Error())
	}
	if err := mergeCronUID(fields, "uid", &o.identity); err != nil {
		return nil, err
	}
	if err := mergeCronUID(fields, "expected_event_uid", &o.expectedEventUID); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (o cronCLIOptions) callValidate(a daemonAPI, c *kataclient.Client, projectID int64, raw []byte) ([]byte, error) {
	if err := requireDaemonAPIVersion(a.ctx, a.client, a.baseURL, apiVersionCronValidate, "cron validation"); err != nil {
		return nil, err
	}
	var body generated.CronValidationBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if o.resource == "job" {
		count := int64(o.count)
		response, err := c.ValidateCronJobWithResponse(a.ctx, &generated.ValidateCronJobRequestOptions{PathParams: &generated.ValidateCronJobPath{ProjectID: projectID}, Query: &generated.ValidateCronJobQuery{Count: &count, From: &o.from}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		return cronCLIResponse(response.StatusCode, response.Body, err)
	}
	response, err := c.ValidateCronWorkflowWithResponse(a.ctx, &generated.ValidateCronWorkflowRequestOptions{PathParams: &generated.ValidateCronWorkflowPath{ProjectID: projectID}, Body: &body})
	if response == nil {
		return nil, externalCLITransportError(response, err)
	}
	return cronCLIResponse(response.StatusCode, response.Body, err)
}

func emitCronValidation(cmd *cobra.Command, raw []byte, job, parentIncompatible bool) error {
	var out struct {
		Valid      *bool          `json:"valid"`
		Definition jsontext.Value `json:"definition"`
	}
	var result generated.CronValidationResult
	for _, destination := range []any{&out, &result} {
		if err := json.Unmarshal(raw, destination); err != nil {
			return fmt.Errorf("decode cron validation response: %w", err)
		}
	}
	if out.Valid == nil || len(out.Definition) == 0 || (*out.Valid && len(result.Errors) > 0) || (!*out.Valid && len(result.Errors) == 0) {
		return fmt.Errorf("incomplete cron validation response")
	}
	if err := result.Validate(); err != nil {
		return fmt.Errorf("invalid cron validation response: %w", err)
	}
	if *out.Valid && (out.Definition.Kind() != '{' || (job && result.Schedule == nil)) {
		return fmt.Errorf("incomplete cron validation response")
	}
	if *out.Valid && parentIncompatible {
		return emitCronParentValidationError(cmd, result)
	}
	if err := emitJSON(cmd.OutOrStdout(), jsontext.Value(raw)); err != nil {
		return err
	}
	if !*out.Valid {
		return cronCLIValidation("cron definition is invalid")
	}
	return nil
}

func emitCronParentValidationError(cmd *cobra.Command, result generated.CronValidationResult) error {
	result.Valid, result.Schedule = false, nil
	result.Errors = []generated.CronValidationError{{Stage: "request", Code: "validation", Message: cronParentIssueRequired}}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := emitJSON(cmd.OutOrStdout(), jsontext.Value(raw)); err != nil {
		return err
	}
	return cronCLIValidation(cronParentIssueRequired)
}
