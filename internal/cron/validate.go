package cron

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/uid"
)

// ErrInvalid identifies a malformed or unsupported portable document.
var ErrInvalid = errors.New("invalid cron document")

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

func decode(input []byte, out any, limit int) error {
	if len(input) > limit {
		return invalid("document exceeds size limit")
	}
	if err := json.Unmarshal(input, out, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// ParseJob strictly decodes and validates one bounded job document.
func ParseJob(input []byte) (JobDefinition, error) {
	var job JobDefinition
	if err := decode(input, &job, DefinitionLimit); err != nil {
		return job, err
	}
	return job, job.Validate()
}

// ParseWorkflow strictly decodes and validates one bounded workflow document.
func ParseWorkflow(input []byte) (WorkflowDefinition, error) {
	var workflow WorkflowDefinition
	if err := decode(input, &workflow, DefinitionLimit); err != nil {
		return workflow, err
	}
	return workflow, workflow.Validate()
}

// maxDurationSeconds is the largest seconds count time.Duration can represent.
const maxDurationSeconds = math.MaxInt64 / int64(time.Second)

func validUID(value string) bool { return value == strings.ToUpper(value) && uid.Valid(value) }
func portableKey(value string) bool {
	if value == "" || len(value) > 256 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.", r) {
			return false
		}
	}
	return true
}

// Validate checks the portable job contract without granting execution.
func (j JobDefinition) Validate() error {
	if j.Version != 1 {
		return invalid("unsupported job version")
	}
	if j.Kind != "job" {
		return invalid("unknown job kind")
	}
	if err := j.Trigger.Validate(); err != nil {
		return err
	}
	if j.CheckoutKey != "" && !portableKey(j.CheckoutKey) {
		return invalid("checkout key must be a portable name")
	}
	if j.Overlap != "forbid" && j.Overlap != "allow" {
		return invalid("unknown overlap policy")
	}
	if j.Catchup != "skip" && j.Catchup != "latest" && j.Catchup != "all" {
		return invalid("unknown catchup policy")
	}
	if j.GraceSeconds < 0 || j.TimeoutSeconds < 0 {
		return invalid("negative duration")
	}
	if j.GraceSeconds > maxDurationSeconds || j.TimeoutSeconds > maxDurationSeconds {
		return invalid("duration exceeds supported range")
	}
	switch j.Action.Kind {
	case "notify":
		if j.Action.Prompt != "" || j.Action.WorkflowUID != "" || j.CheckoutKey != "" {
			return invalid("notify cannot carry execution fields")
		}
		if !validRecipient(j.Action.Recipient) {
			return invalid("notify requires an exact recipient or current-owner-or-author")
		}
		if len(j.Action.Message) > notification.MessageMaxBytes {
			return invalid("notify message exceeds the inbox limit")
		}
		if strings.TrimSpace(j.Action.Message) == "" && j.Action.Recipient != "current-owner-or-author" {
			return invalid("custom notifications require a message")
		}
		if j.Trigger.Kind != "issue-scheduled" && j.Trigger.Kind != "issue-deadline" && j.Issue == nil {
			return invalid("notify requires an issue target")
		}
		if j.Issue != nil && (j.Issue.Kind != "existing" || !validUID(j.Issue.UID) || j.Issue.Title != "" || j.Issue.Body != "" || j.Issue.ScheduledOffsetSeconds != nil || j.Issue.DeadlineOffsetSeconds != nil) {
			return invalid("notify requires an existing issue target")
		}
	case "execute":
		if j.Trigger.Kind == "issue-deadline" {
			return invalid("deadline actions are notification-only")
		}
		if j.Action.Recipient != "" || j.Action.Message != "" {
			return invalid("execute cannot carry notification fields")
		}
		if j.Action.WorkflowUID != "" && j.Action.Prompt != "" || j.Action.WorkflowUID == "" && strings.TrimSpace(j.Action.Prompt) == "" {
			return invalid("execute requires exactly one prompt or workflow UID")
		}
		if j.Action.WorkflowUID != "" && !validUID(j.Action.WorkflowUID) {
			return invalid("invalid workflow UID")
		}
		if j.Issue == nil {
			return invalid("execute requires an issue policy")
		}
		switch j.Issue.Kind {
		case "existing":
			if !validUID(j.Issue.UID) || j.Issue.Title != "" || j.Issue.Body != "" || j.Issue.ScheduledOffsetSeconds != nil || j.Issue.DeadlineOffsetSeconds != nil {
				return invalid("invalid existing issue policy")
			}
		case "per-run":
			if j.Issue.UID != "" || strings.TrimSpace(j.Issue.Title) == "" {
				return invalid("invalid per-run issue policy")
			}
			for _, offset := range []*int64{j.Issue.ScheduledOffsetSeconds, j.Issue.DeadlineOffsetSeconds} {
				if offset != nil && (*offset > maxDurationSeconds || *offset < -maxDurationSeconds) {
					return invalid("relative date offset exceeds supported duration")
				}
			}
		default:
			return invalid("unknown issue policy")
		}
	default:
		return invalid("unknown action")
	}
	for key, value := range j.SecretRefs {
		if !portableKey(key) || !portableKey(value) {
			return invalid("secret references must be portable names")
		}
	}
	if err := validateOptions(j.Options); err != nil {
		return err
	}
	return checkSize(j, DefinitionLimit)
}

func validRecipient(value string) bool {
	if value == "current-owner-or-author" {
		return true
	}
	if normalized, err := notification.NormalizeRecipient(value); err != nil || normalized != value {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.TrimSpace(part) != part {
			return false
		}
		for _, r := range part {
			if unicode.IsSpace(r) || unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

// Validate checks step identities, ordering and bounded retry settings.
func (f WorkflowDefinition) Validate() error {
	if f.Version != 1 {
		return invalid("unsupported workflow version")
	}
	if len(f.Steps) == 0 {
		return invalid("workflow requires steps")
	}
	seen := map[string]bool{}
	for _, step := range f.Steps {
		if !portableKey(step.Key) || seen[step.Key] {
			return invalid("step keys must be unique portable names")
		}
		if step.Retries < 0 || step.Retries > 100 {
			return invalid("retry count must be between 0 and 100")
		}
		for _, previous := range step.After {
			if !seen[previous] {
				return invalid("dependencies must name earlier steps")
			}
		}
		switch step.Kind {
		case "command":
			if strings.TrimSpace(step.Command) == "" || step.Prompt != "" {
				return invalid("command step requires only command")
			}
		case "prompt":
			if strings.TrimSpace(step.Prompt) == "" || step.Command != "" {
				return invalid("prompt step requires only prompt")
			}
		default:
			return invalid("unknown step kind")
		}
		if err := validateOptions(step.Options); err != nil {
			return err
		}
		seen[step.Key] = true
	}
	if err := validateOptions(f.Options); err != nil {
		return err
	}
	return checkSize(f, DefinitionLimit)
}

func checkSize(value any, limit int) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(encoded) > limit {
		return invalid("document exceeds size limit")
	}
	return nil
}

// Executor options are opaque to Kata, but named credential/path fields cannot
// carry local values. Executors resolve secret_refs and checkout_key locally.
func validateOptions(input []byte) error {
	if len(input) == 0 {
		return nil
	}
	var object map[string]any
	if err := json.Unmarshal(input, &object); err != nil || object == nil {
		return invalid("options must be an object")
	}
	var walk func(any) error
	walk = func(value any) error {
		switch v := value.(type) {
		case map[string]any:
			for key, entry := range v {
				switch strings.ToLower(key) {
				case "token", "api_token", "api_key", "password", "secret", "credentials", "authorization", "cwd", "path", "checkout_path", "local_path":
					return invalid("options cannot contain local credentials or paths")
				}
				if err := walk(entry); err != nil {
					return err
				}
			}
		case []any:
			for _, entry := range v {
				if err := walk(entry); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(object)
}
