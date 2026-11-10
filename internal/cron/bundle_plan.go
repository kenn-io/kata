package cron

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/uid"
)

// BundleTarget fixes the destination and explicit issue mappings before writes.
// Explicit mappings override retention when copying within the source project.
type BundleTarget struct {
	ProjectUID    string
	IssueMappings map[string]string
	KeepEnabled   bool
}

// PlannedDefinition pairs source provenance with an independent destination UID.
type PlannedDefinition struct {
	SourceUID string `json:"source_uid"`
	BundleDefinition
}

// BundleReview names runtime-local fields whose meaning may differ at the target.
type BundleReview struct {
	Kind      string   `json:"kind"`
	SourceUID string   `json:"source_uid"`
	Fields    []string `json:"fields"`
}

// ImportPlan contains current-rule-validated definitions in stable write order.
type ImportPlan struct {
	Workflows []PlannedDefinition
	Jobs      []PlannedDefinition
	Review    []BundleReview
}

// PlanBundle remaps only modeled references and disables jobs unless requested.
// It never changes the input, consults runtime state, or grants write authority.
func PlanBundle(b Bundle, target BundleTarget) (ImportPlan, error) {
	plan := ImportPlan{Workflows: []PlannedDefinition{}, Jobs: []PlannedDefinition{}, Review: []BundleReview{}}
	if err := b.Validate(); err != nil {
		return plan, err
	}
	if !validUID(target.ProjectUID) {
		return plan, invalid("invalid target project UID")
	}
	for from, to := range target.IssueMappings {
		if !validUID(from) || !validUID(to) {
			return plan, invalid("invalid issue mapping UID")
		}
	}
	destination := func(source string) (string, error) {
		return uid.FromStableSeed([]byte("kata-cron-copy\x00"+source+"\x00"+target.ProjectUID), uid.MustTime(source))
	}
	workflows := map[string]string{}
	ordered := func(entries []BundleDefinition) []BundleDefinition {
		entriesCopy := slices.Clone(entries)
		slices.SortFunc(entriesCopy, func(a, b BundleDefinition) int { return strings.Compare(a.UID, b.UID) })
		return entriesCopy
	}
	for _, entry := range ordered(b.Workflows) {
		workflow, err := ParseWorkflow(entry.Definition)
		if err != nil {
			return plan, err
		}
		id, err := destination(entry.UID)
		if err != nil {
			return plan, err
		}
		workflows[entry.UID] = id
		item := PlannedDefinition{SourceUID: entry.UID, BundleDefinition: entry}
		item.UID = id
		item.Name = strings.TrimSpace(entry.Name)
		item.Definition, err = json.Marshal(workflow, json.Deterministic(true))
		if err != nil {
			return plan, err
		}
		if len(item.Definition) > DefinitionLimit {
			return plan, invalid("planned workflow exceeds size limit")
		}
		plan.Workflows = append(plan.Workflows, item)
		fields := []string{}
		if len(workflow.Options) > 0 {
			fields = append(fields, "options")
		}
		for i, step := range workflow.Steps {
			if len(step.Options) > 0 {
				fields = append(fields, fmt.Sprintf("steps[%d].options", i))
			}
		}
		if len(fields) > 0 {
			plan.Review = append(plan.Review, BundleReview{"workflow", entry.UID, fields})
		}
	}
	missing := map[string]bool{}
	remapIssue := func(ref *string) {
		if *ref == "" {
			return
		}
		if mapped, ok := target.IssueMappings[*ref]; ok {
			*ref = mapped
			return
		}
		if target.ProjectUID != b.Source.ProjectUID {
			missing[*ref] = true
		}
	}
	for _, entry := range ordered(b.Jobs) {
		// Decode allocates independent pointers, maps and raw option buffers.
		job, err := ParseJob(entry.Definition)
		if err != nil {
			return plan, err
		}
		if job.Action.WorkflowUID != "" {
			job.Action.WorkflowUID = workflows[job.Action.WorkflowUID]
		}
		remapIssue(&job.Trigger.IssueUID)
		if job.Issue != nil {
			remapIssue(&job.Issue.UID)
			remapIssue(&job.Issue.ParentUID)
		}
		job.Enabled = job.Enabled && target.KeepEnabled
		if err := job.Validate(); err != nil {
			return plan, err
		}
		raw, err := json.Marshal(job, json.Deterministic(true))
		if err != nil {
			return plan, err
		}
		if len(raw) > DefinitionLimit {
			return plan, invalid("planned job exceeds size limit")
		}
		id, err := destination(entry.UID)
		if err != nil {
			return plan, err
		}
		item := PlannedDefinition{SourceUID: entry.UID, BundleDefinition: entry}
		item.UID = id
		item.Name = strings.TrimSpace(entry.Name)
		item.Definition = raw
		plan.Jobs = append(plan.Jobs, item)
		fields := []string{}
		if job.Action.Recipient != "" {
			fields = append(fields, "action.recipient")
		}
		if job.CheckoutKey != "" {
			fields = append(fields, "checkout_key")
		}
		if len(job.SecretRefs) > 0 {
			fields = append(fields, "secret_refs")
		}
		if len(job.Options) > 0 {
			fields = append(fields, "options")
		}
		if len(fields) > 0 {
			plan.Review = append(plan.Review, BundleReview{"job", entry.UID, fields})
		}
	}
	if len(missing) > 0 {
		refs := make([]string, 0, len(missing))
		for ref := range missing {
			refs = append(refs, ref)
		}
		slices.Sort(refs)
		return plan, invalid("missing issue mappings: " + strings.Join(refs, ", "))
	}
	return plan, nil
}
