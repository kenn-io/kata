package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// BundleLimit bounds the complete encoded portable archive, including metadata.
const BundleLimit = 16 << 20

// RFC3339 lexical checks exclude extensions accepted by time.Parse, such as
// one-digit hours, comma fractions and out-of-range numeric offsets.
var bundleTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`)

// Bundle carries independent definitions, without run history or credentials.
// Identities are source provenance; an import must allocate destination IDs.
type Bundle struct {
	Format    string             `json:"format"`
	Version   int                `json:"version"`
	Source    BundleSource       `json:"source"`
	Workflows []BundleDefinition `json:"workflows"`
	Jobs      []BundleDefinition `json:"jobs"`
}

// BundleSource identifies the project and time from which definitions were read.
type BundleSource struct {
	ProjectUID  string `json:"project_uid"`
	ProjectName string `json:"project_name"`
	ExportedAt  string `json:"exported_at"`
}

// BundleDefinition keeps the API document intact, including opaque numbers
// and historical definitions that no longer satisfy current new-write rules.
type BundleDefinition struct {
	UID                string         `json:"uid"`
	DefinitionEventUID string         `json:"definition_event_uid"`
	Name               string         `json:"name"`
	Definition         jsontext.Value `json:"definition"`
}

// ParseBundle strictly decodes one bounded archive. New-write validation belongs
// to import planning, so export remains useful for inspecting historical state.
func ParseBundle(input []byte) (Bundle, error) {
	var b Bundle
	if len(input) > BundleLimit {
		return b, invalid("bundle exceeds size limit")
	}
	if err := json.Unmarshal(input, &b, json.RejectUnknownMembers(true)); err != nil {
		return b, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(input, &members); err != nil {
		return b, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for _, key := range []string{"jobs", "workflows"} {
		if members[key].Kind() != '[' {
			return b, invalid("bundle " + key + " must be an array")
		}
	}
	return b, b.Validate()
}

// Validate checks structural documents, provenance, limits and workflow closure.
// Identity is scoped by kind: a workflow and job may share a source UID.
func (b Bundle) Validate() error {
	if b.Format != "kata.cron.bundle" || b.Version != 1 {
		return invalid("unsupported bundle format or version")
	}
	if !validUID(b.Source.ProjectUID) || strings.TrimSpace(b.Source.ProjectName) == "" {
		return invalid("invalid bundle source project")
	}
	if _, err := time.Parse(time.RFC3339, b.Source.ExportedAt); err != nil || !bundleTimestamp.MatchString(b.Source.ExportedAt) {
		return invalid("invalid bundle exported_at timestamp")
	}
	if len(b.Workflows)+len(b.Jobs) > 1000 {
		return invalid("bundle exceeds 1000 definitions")
	}
	workflows := make(map[string]bool, len(b.Workflows))
	for _, group := range []struct {
		kind    string
		entries []BundleDefinition
	}{{"workflow", b.Workflows}, {"job", b.Jobs}} {
		seen := make(map[string]bool, len(group.entries))
		for _, entry := range group.entries {
			if !validUID(entry.UID) || !validUID(entry.DefinitionEventUID) || seen[entry.UID] {
				return invalid("invalid or duplicate " + group.kind + " identity")
			}
			seen[entry.UID] = true
			if name := strings.TrimSpace(entry.Name); name == "" || len(name) > 256 {
				return invalid("bundle definition name must be 1-256 bytes")
			}
			if len(entry.Definition) > DefinitionLimit {
				return invalid("bundle definition exceeds size limit")
			}
			if entry.Definition.Kind() != '{' {
				return invalid("bundle definition must be an object")
			}
			if group.kind == "workflow" {
				if _, err := DecodeWorkflow(entry.Definition); err != nil {
					return err
				}
				workflows[entry.UID] = true
			} else {
				job, err := DecodeJob(entry.Definition)
				if err != nil {
					return err
				}
				if ref := job.Action.WorkflowUID; ref != "" && !workflows[ref] {
					return invalid("bundle is missing workflow " + ref)
				}
			}
		}
	}
	encoded, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(encoded) > BundleLimit {
		return invalid("bundle exceeds size limit")
	}
	return nil
}
