// Package definition decodes portable native cron definitions for the source UI.
package definition

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"unicode/utf8"
)

// Trigger describes portable schedule intent.
type Trigger struct {
	Kind            string `json:"kind"`
	Timezone        string `json:"timezone,omitempty"`
	Cron            string `json:"cron,omitempty"`
	IntervalSeconds int64  `json:"interval_seconds,omitzero"`
	At              string `json:"at,omitempty"`
	IssueUID        string `json:"issue_uid,omitempty"`
	LeadSeconds     int64  `json:"lead_seconds,omitzero"`
}

// Action selects a prompt, workflow, or notification.
type Action struct {
	Kind        string `json:"kind"`
	Prompt      string `json:"prompt,omitempty"`
	WorkflowUID string `json:"workflow_uid,omitempty"`
	Recipient   string `json:"recipient,omitempty"`
	Message     string `json:"message,omitempty"`
}

// IssuePolicy defines the native issue binding.
type IssuePolicy struct {
	Kind                   string `json:"kind"`
	ParentUID              string `json:"parent_uid,omitempty"`
	UID                    string `json:"uid,omitempty"`
	Title                  string `json:"title,omitempty"`
	Body                   string `json:"body,omitempty"`
	ScheduledOffsetSeconds *int64 `json:"scheduled_offset_seconds,omitzero"`
	DeadlineOffsetSeconds  *int64 `json:"deadline_offset_seconds,omitzero"`
}

// JobDef retains a portable job and opaque option bytes.
type JobDef struct {
	Version        int               `json:"version"`
	Kind           string            `json:"kind"`
	Enabled        bool              `json:"enabled"`
	Trigger        Trigger           `json:"trigger"`
	Action         Action            `json:"action"`
	CheckoutKey    string            `json:"checkout_key,omitempty"`
	Issue          *IssuePolicy      `json:"issue,omitempty"`
	Overlap        string            `json:"overlap"`
	Catchup        string            `json:"catchup"`
	GraceSeconds   int64             `json:"grace_seconds,omitzero"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitzero"`
	SecretRefs     map[string]string `json:"secret_refs,omitempty"`
	Options        jsontext.Value    `json:"options,omitempty"`
}

// Step defines one ordered workflow step.
type Step struct {
	Key     string         `json:"key"`
	Kind    string         `json:"kind"`
	Command string         `json:"command,omitempty"`
	Prompt  string         `json:"prompt,omitempty"`
	After   []string       `json:"after,omitempty"`
	Retries int            `json:"retries,omitempty"`
	Options jsontext.Value `json:"options,omitempty"`
}

// WorkflowDef retains the portable ordered workflow.
type WorkflowDef struct {
	About   string         `json:"about,omitempty"`
	Input   string         `json:"input,omitempty"`
	Options jsontext.Value `json:"options,omitempty"`
	Version int            `json:"version"`
	Steps   []Step         `json:"steps"`
}

// DecodeJob decodes a bounded portable job document.
func DecodeJob(b []byte) (j JobDef, err error) { err = decode(b, &j); return }

// DecodeWorkflow decodes a bounded workflow without interpreting adapter options.
func DecodeWorkflow(b []byte) (w WorkflowDef, err error) {
	if err = decode(b, &w); err != nil {
		return
	}
	var fields map[string]jsontext.Value
	if err = json.Unmarshal(b, &fields); err != nil {
		return
	}
	if raw, ok := fields["input"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		err = fmt.Errorf("workflow input must be a string")
	}
	return
}
func decode(b []byte, v any) error {
	if len(b) > 256*1024 {
		return fmt.Errorf("definition exceeds size limit")
	}
	if !utf8.Valid(b) {
		return fmt.Errorf("invalid UTF-8 in definition")
	}
	if data := bytes.TrimSpace(b); len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("definition must be an object")
	}
	return json.Unmarshal(b, v)
}
