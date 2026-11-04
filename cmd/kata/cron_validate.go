package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func (o *cronCLIOptions) validationRequestBody(cmd *cobra.Command) ([]byte, error) {
	raw, err := o.readRequest(cmd)
	if err != nil {
		return nil, err
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, cronCLIValidation("cron request must be a JSON object")
	}
	// Validate only the envelope; opaque document values stay raw for the daemon.
	for key := range fields {
		switch key {
		case "definition":
		case "name", "uid", "expected_event_uid", "actor":
			if _, err := cronStringField(fields, key); err != nil {
				return nil, err
			}
		default:
			return nil, cronCLIValidation("unknown cron request member: " + key)
		}
	}
	bodyUID, err := cronStringField(fields, "uid")
	if err != nil {
		return nil, err
	}
	if bodyUID != "" {
		bodyUID, err = normalizedCronCLIUID(bodyUID)
		if err != nil {
			return nil, err
		}
	}
	if o.identity != "" && bodyUID != "" && bodyUID != o.identity {
		return nil, cronCLIValidation("uid in body and flag must match")
	}
	if o.identity == "" {
		o.identity = bodyUID
	}
	if o.identity != "" {
		if err := setCronStringField(fields, "uid", o.identity); err != nil {
			return nil, err
		}
	}
	if err := o.applyExpectedEventUID(fields); err != nil {
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
	var status int
	var result []byte
	var callErr error
	if o.resource == "job" {
		count := strconv.Itoa(o.count)
		response, err := c.ValidateCronJobWithResponse(a.ctx, &generated.ValidateCronJobRequestOptions{PathParams: &generated.ValidateCronJobPath{ProjectID: projectID}, Query: &generated.ValidateCronJobQuery{Count: &count, From: &o.from}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		status, result, callErr = response.StatusCode, response.Body, err
	} else {
		response, err := c.ValidateCronWorkflowWithResponse(a.ctx, &generated.ValidateCronWorkflowRequestOptions{PathParams: &generated.ValidateCronWorkflowPath{ProjectID: projectID}, Body: &body})
		if response == nil {
			return nil, externalCLITransportError(response, err)
		}
		status, result, callErr = response.StatusCode, response.Body, err
	}
	if status >= 200 && status < 300 && callErr != nil {
		return nil, fmt.Errorf("decode cron validation response: %w", callErr)
	}
	return cronCLIResponse(status, result, callErr)
}

func emitCronValidation(cmd *cobra.Command, raw []byte, job bool) error {
	var out struct {
		Valid      *bool                       `json:"valid"`
		Name       *string                     `json:"name"`
		Definition jsontext.Value              `json:"definition"`
		Errors     *[]db.CronValidationError   `json:"errors"`
		Warnings   *[]db.CronValidationWarning `json:"warnings"`
		Schedule   *struct {
			Delivery *string              `json:"delivery"`
			Basis    *string              `json:"basis"`
			Zone     *string              `json:"zone"`
			From     *time.Time           `json:"from"`
			Next     *[]cron.ScheduleFire `json:"next"`
		} `json:"schedule"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode cron validation response: %w", err)
	}
	if out.Valid == nil || out.Name == nil || len(out.Definition) == 0 || out.Errors == nil || out.Warnings == nil || (*out.Valid && len(*out.Errors) > 0) || (!*out.Valid && len(*out.Errors) == 0) {
		return fmt.Errorf("incomplete cron validation response")
	}
	if *out.Valid && out.Definition.Kind() != '{' {
		return fmt.Errorf("invalid definition in cron validation response")
	}
	if *out.Valid && job {
		schedule := out.Schedule
		if schedule == nil || schedule.Delivery == nil || schedule.Basis == nil || schedule.Zone == nil || schedule.From == nil || schedule.Next == nil {
			return fmt.Errorf("incomplete schedule in cron validation response")
		}
	}
	if err := emitJSON(cmd.OutOrStdout(), jsontext.Value(raw)); err != nil {
		return err
	}
	if !*out.Valid {
		return cronCLIValidation("cron definition is invalid")
	}
	return nil
}
