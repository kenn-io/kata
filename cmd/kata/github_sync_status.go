package main

import (
	"fmt"
	"io"
	"time"

	"go.kenn.io/kata/internal/textsafe"
)

type githubSyncProgressOut struct {
	Phase     string    `json:"phase"`
	Completed int       `json:"completed"`
	Total     int       `json:"total"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func githubSyncPrintAgentDetails(w io.Writer, status githubSyncStatusOut, binding *githubSyncBindingOut) error {
	if _, err := fmt.Fprintf(w, " status_sync=%s pending_count=%d", agentValue(issueStatusSyncDisplayMode(status.StatusSync)), status.PendingCount); err != nil {
		return err
	}
	if binding != nil {
		if _, err := fmt.Fprintf(w, " interval_seconds=%d title_prefix=%t", binding.IntervalSeconds, githubSyncConfigTitlePrefix(binding.Config)); err != nil {
			return err
		}
		if since := githubSyncConfigString(binding.Config, "since"); since != "" {
			if _, err := fmt.Fprintf(w, " since=%s", agentValue(since)); err != nil {
				return err
			}
		}
	}
	for _, field := range []struct {
		name  string
		value *time.Time
	}{{"sync_started_at", status.SyncStartedAt}, {"last_attempt_at", status.LastAttemptAt}, {"last_success_at", status.LastSuccessAt}, {"last_error_at", status.LastErrorAt}} {
		if field.value != nil {
			if _, err := fmt.Fprintf(w, " %s=%s", field.name, field.value.UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	}
	if status.Progress != nil {
		p := status.Progress
		if _, err := fmt.Fprintf(w, " phase=%s completed=%d total=%d progress_updated_at=%s", agentValue(p.Phase), p.Completed, p.Total, p.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, " last_created=%d last_updated=%d last_unchanged=%d last_comments=%d", status.LastCreated, status.LastUpdated, status.LastUnchanged, status.LastComments)
	return err
}

func githubSyncPrintHumanDetails(w io.Writer, body githubSyncBindingBody) error {
	if _, err := fmt.Fprintf(w, "Status sync: %s\nPending status changes: %d\n", textsafe.Line(issueStatusSyncDisplayMode(body.Status.StatusSync)), body.Status.PendingCount); err != nil {
		return err
	}
	if body.Binding != nil {
		if _, err := fmt.Fprintf(w, "Repository: %s\nInterval: %ds\nTitle prefix: %t\n", textsafe.Line(githubSyncRepoLabel(body.Binding)), body.Binding.IntervalSeconds, githubSyncConfigTitlePrefix(body.Binding.Config)); err != nil {
			return err
		}
		if since := githubSyncConfigString(body.Binding.Config, "since"); since != "" {
			if _, err := fmt.Fprintf(w, "Since: %s (updated after)\n", textsafe.Line(since)); err != nil {
				return err
			}
		}
	}
	status := body.Status
	if p := status.Progress; p != nil {
		counts := fmt.Sprintf("%d completed (total unknown)", p.Completed)
		if p.Total > 0 {
			counts = fmt.Sprintf("%d/%d completed", p.Completed, p.Total)
		}
		if _, err := fmt.Fprintf(w, "Progress: %s — %s; updated %s\n", textsafe.Line(p.Phase), counts, p.UpdatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value *time.Time
	}{{"Started", status.SyncStartedAt}, {"Last attempt", status.LastAttemptAt}, {"Last success", status.LastSuccessAt}, {"Last error at", status.LastErrorAt}} {
		if field.value != nil {
			if _, err := fmt.Fprintf(w, "%s: %s\n", field.name, field.value.UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	}
	if status.LastSuccessAt == nil {
		_, err := fmt.Fprintln(w, "No successful run yet")
		return err
	}
	_, err := fmt.Fprintf(w, "Last successful run: created=%d updated=%d unchanged=%d comments=%d\n", status.LastCreated, status.LastUpdated, status.LastUnchanged, status.LastComments)
	return err
}

func githubSyncConfigTitlePrefix(config map[string]any) bool {
	value, present := config["title_prefix"].(bool)
	return !present || value
}
