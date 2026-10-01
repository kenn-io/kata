package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kata/pkg/client/generated"
)

func issueSyncPrintBinding(w io.Writer, raw []byte, provider, action string) error {
	if provider == "github" {
		return githubSyncPrintBindingBody(w, raw, action)
	}
	if currentOutputMode() == outputJSON {
		return emitJSON(w, jsontext.Value(raw))
	}
	var body generated.IssueSyncBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	return issueSyncPrintStatus(w, provider, action, body.Status, body.Binding)
}

func issueSyncPrintOnce(w io.Writer, raw []byte, provider string) error {
	if provider == "github" {
		return githubSyncPrintOnceBody(w, raw)
	}
	if currentOutputMode() == outputJSON {
		return emitJSON(w, jsontext.Value(raw))
	}
	var body generated.RunIssueSyncOnceResponseBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	if currentOutputMode() == outputAgent {
		return issueSyncPrintStatus(w, provider, "once", body.Status, &body.Binding)
	}
	_, err := fmt.Fprintf(w, "%s sync ran: created=%d updated=%d unchanged=%d\n", issueSyncLabel(provider), body.Import.Created, body.Import.Updated, body.Import.Unchanged)
	return err
}

func issueSyncPrintStatus(w io.Writer, provider, action string, status generated.IssueSyncStatusOut, binding *generated.IssueSyncBindingOut) error {
	// Buffer once so write failures cannot be lost among optional detail fields.
	var out strings.Builder
	agent := currentOutputMode() == outputAgent
	if agent {
		fmt.Fprintf(&out, "OK %s-sync action=%s state=%s enabled=%t", provider, agentValue(action), agentValue(status.State), status.Enabled)
	} else {
		label := status.State
		if action != "status" {
			label = action
		}
		fmt.Fprintf(&out, "%s sync %s\n", issueSyncLabel(provider), textsafe.Line(label))
	}
	if binding != nil {
		if agent {
			fmt.Fprintf(&out, " source=%s interval_seconds=%d", agentValue(binding.DisplayName), binding.IntervalSeconds)
		} else {
			fmt.Fprintf(&out, "Source: %s\nInterval: %ds\n", textsafe.Line(binding.DisplayName), binding.IntervalSeconds)
		}
		fields := []struct{ key, label string }{{"data_source_id", "Data source"}, {"database_id", "Database"}, {"title_property_id", "Title property"}, {"status_property_id", "Status property"}, {"assignee_property_id", "Assignee property"}, {"done_status_ids", "Completed options"}, {"since", "Since"}, {"title_prefix", "Title prefix"}}
		if provider == "plane" {
			fields = []struct{ key, label string }{{"api_origin", "API origin"}, {"web_origin", "Web origin"}, {"workspace", "Workspace"}, {"project_id", "Plane project"}, {"since", "Since"}, {"title_prefix", "Title prefix"}}
		}
		for _, field := range fields {
			value := issueSyncConfigValue(binding.Config, field.key)
			if value == "" {
				continue
			}
			if agent {
				fmt.Fprintf(&out, " %s=%s", field.key, agentValue(value))
			} else {
				fmt.Fprintf(&out, "%s: %s\n", field.label, textsafe.Line(value))
			}
		}
	}
	for _, field := range []struct {
		key, label string
		value      *time.Time
	}{{"sync_started_at", "Started", status.SyncStartedAt}, {"last_attempt_at", "Last attempt", status.LastAttemptAt}, {"last_success_at", "Last success", status.LastSuccessAt}, {"last_error_at", "Last error at", status.LastErrorAt}} {
		if field.value == nil {
			continue
		}
		value := field.value.UTC().Format(time.RFC3339Nano)
		if agent {
			fmt.Fprintf(&out, " %s=%s", field.key, value)
		} else {
			fmt.Fprintf(&out, "%s: %s\n", field.label, value)
		}
	}
	if p := status.Progress; p != nil {
		if agent {
			fmt.Fprintf(&out, " phase=%s completed=%d total=%d progress_updated_at=%s", agentValue(p.Phase), p.Completed, p.Total, p.UpdatedAt.UTC().Format(time.RFC3339Nano))
		} else {
			counts := fmt.Sprintf("%d completed (total unknown)", p.Completed)
			if p.Total > 0 {
				counts = fmt.Sprintf("%d/%d completed", p.Completed, p.Total)
			}
			fmt.Fprintf(&out, "Progress: %s — %s; updated %s\n", textsafe.Line(p.Phase), counts, p.UpdatedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	if agent {
		fmt.Fprintf(&out, " last_created=%d last_updated=%d last_unchanged=%d", status.LastCreated, status.LastUpdated, status.LastUnchanged)
	} else if status.LastSuccessAt != nil {
		fmt.Fprintf(&out, "Last successful run: created=%d updated=%d unchanged=%d\n", status.LastCreated, status.LastUpdated, status.LastUnchanged)
	} else {
		out.WriteString("No successful run yet\n")
	}
	if status.LastError != nil && *status.LastError != "" {
		if agent {
			fmt.Fprintf(&out, " last_error=%s", agentValue(*status.LastError))
		} else {
			fmt.Fprintf(&out, "Last error: %s\n", textsafe.Line(*status.LastError))
		}
	}
	if agent {
		out.WriteByte('\n')
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func issueSyncConfigValue(config map[string]any, key string) string {
	if key == "title_prefix" {
		value, ok := config[key].(bool)
		return strconv.FormatBool(!ok || value)
	}
	if value, ok := config[key].(string); ok {
		return value
	}
	if values, ok := config[key].([]any); ok {
		var labels []string
		for _, value := range values {
			if label, ok := value.(string); ok {
				labels = append(labels, label)
			}
		}
		return strings.Join(labels, ",")
	}
	return ""
}
