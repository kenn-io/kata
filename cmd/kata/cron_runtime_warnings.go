package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cron"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func (o cronCLIOptions) runtimeNotices() bool {
	return o.resource == "job" && slices.Contains([]string{"list", "create", "update", "restore"}, o.action)
}

// warnRuntimes follows a successful result. Optional reads and stderr failures
// cannot change that result or prolong it beyond the small diagnostic budget.
func (o cronCLIOptions) warnRuntimes(ctx context.Context, cmd *cobra.Command, client *kataclient.Client, projectID int64, raw []byte) {
	if flags.Quiet || !o.runtimeNotices() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	response, err := client.ListCronRuntimesWithResponse(ctx, &generated.ListCronRuntimesRequestOptions{PathParams: &generated.ListCronRuntimesPath{ProjectID: projectID}})
	if err != nil || response == nil || response.StatusCode != 200 {
		return
	}
	var inventory cron.RuntimeInventory
	if json.Unmarshal(response.Body, &inventory) != nil || inventory.Inventory.State != "ready" {
		return
	}
	id := ""
	if o.action != "list" {
		var written struct {
			Job struct {
				UID string `json:"uid"`
			} `json:"job"`
		}
		if json.Unmarshal(raw, &written) != nil {
			return
		}
		id = written.Job.UID
	}
	for _, warning := range inventory.Warnings {
		if warning.Code != "no_live_runtime" && warning.Code != "pin_unmatched" && warning.Code != "adapter_unmatched" {
			continue
		}
		if o.action != "list" && warning.Code != "no_live_runtime" && (warning.JobUID == "" || warning.JobUID != id) {
			continue
		}
		reference := ""
		if warning.JobUID != "" {
			reference = "job " + warning.JobUID + "; "
		}
		// Diagnostics are advisory even if the stderr consumer has closed its pipe.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s (%skata cron runtime list)\n", warning.Message, reference)
	}
}
