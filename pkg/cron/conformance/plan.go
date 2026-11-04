package conformance

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// Reference is the offline decision model. A zero Policy selects the default
// declaration. Calendar enumeration has a 100,000-instant safety budget; it
// returns an error instead of silently consuming an unenumerated backlog.
type Reference struct{ Policy Declaration }

func (r Reference) Declare() Declaration {
	if r.Policy != (Declaration{}) {
		return r.Policy
	}
	return Declaration{Name: "kata-reference", CatchupLimit: 1000, FailurePolicy: "continue"}
}

func validatePlan(in PlanInput) (cron.JobDefinition, error) {
	var job cron.JobDefinition
	if in.Adapter == "" || in.Job.CreatedAt.IsZero() || in.Job.CreatedAt.Year() > 9999 || in.Job.DefinitionEventUID == "" || in.InstalledAt.IsZero() || in.Now.IsZero() || in.Now.Year() > 9999 || in.ActiveRuns < 0 {
		return job, fmt.Errorf("invalid planning identity, clock or occupancy")
	}
	var err error
	job, err = cron.ParseJob(in.Job.Definition)
	if err != nil {
		return job, err
	}
	if c := in.Checkpoint; c != nil {
		if c.Through.IsZero() || c.Through.Year() > 9999 || c.Through.Before(in.Job.CreatedAt) || c.DefinitionEventUID == "" {
			return job, fmt.Errorf("invalid checkpoint")
		}
		for _, at := range c.Pending {
			if at.IsZero() || at.After(c.Through) || !at.After(in.Job.CreatedAt) {
				return job, fmt.Errorf("invalid pending instants")
			}
		}
	}
	return job, nil
}

func (r Reference) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	out, _, err := r.planDecision(ctx, in)
	return out, err
}

// PlanWithDrops returns the plan and drop groups from one evaluation.
func (r Reference) PlanWithDrops(ctx context.Context, in PlanInput) (PlanOutput, []Drop, error) {
	return r.planDecision(ctx, in)
}

func (r Reference) planDecision(ctx context.Context, in PlanInput) (PlanOutput, []Drop, error) {
	drops := []Drop{}
	out := PlanOutput{Launch: []time.Time{}, Checkpoint: Checkpoint{Through: in.Job.CreatedAt.UTC(), DefinitionEventUID: in.Job.DefinitionEventUID, Pending: []time.Time{}}}
	if err := ctx.Err(); err != nil {
		return out, drops, err
	}
	d := r.Declare()
	if err := d.Validate(); err != nil {
		return out, drops, err
	}
	job, err := validatePlan(in)
	if err != nil {
		return out, drops, err
	}
	if in.InstalledAt.After(out.Checkpoint.Through) {
		out.Checkpoint.Through = in.InstalledAt.UTC()
	}
	if c := in.Checkpoint; c != nil {
		out.Checkpoint.Through = c.Through.UTC()
		if c.DefinitionEventUID == in.Job.DefinitionEventUID {
			out.Checkpoint.Pending = append(out.Checkpoint.Pending, c.Pending...)
		}
	}
	slices.SortFunc(out.Checkpoint.Pending, func(a, b time.Time) int { return a.Compare(b) })
	out.Checkpoint.Pending = slices.CompactFunc(out.Checkpoint.Pending, func(a, b time.Time) bool { return a.Equal(b) })
	switch {
	case in.Job.Deleted:
		out.Excluded = "deleted"
		out.Checkpoint.Pending = []time.Time{}
	case !job.Enabled:
		out.Excluded = "disabled"
	case cron.IsRecurringIssueJob(job):
		out.Excluded = "notify"
	case job.Trigger.Kind == "manual":
		out.Excluded = "manual"
	default:
		var options map[string]any
		if len(job.Options) > 0 {
			if err := json.Unmarshal(job.Options, &options); err != nil {
				return out, drops, err
			}
		}
		if target, ok := options["adapter"]; ok && target != in.Adapter {
			out.Excluded = "other_adapter"
		}
	}
	if out.Excluded != "" {
		return out, drops, nil
	}
	if job.Trigger.Kind == "issue-scheduled" || job.Trigger.Kind == "issue-deadline" {
		return out, drops, fmt.Errorf("issue-date decisions require readiness reads outside this kit")
	}
	if job.Trigger.Kind == "once" {
		at, _ := time.Parse(time.RFC3339, job.Trigger.At)
		if !at.After(in.Job.CreatedAt) {
			out.Excluded = "once_past"
			return out, drops, nil
		}
	}
	next, err := cron.AdapterSchedule(job.Trigger, in.Job.CreatedAt)
	if err != nil {
		return out, drops, err
	}
	for _, at := range out.Checkpoint.Pending {
		if !next(at.Add(-time.Nanosecond)).Equal(at) {
			return out, drops, fmt.Errorf("pending value is not a schedule instant")
		}
	}
	candidates := slices.Clone(out.Checkpoint.Pending)
	for cursor, scanned := out.Checkpoint.Through, 0; ; scanned++ {
		if err := ctx.Err(); err != nil {
			return out, drops, err
		}
		due := next(cursor)
		if due.IsZero() || due.After(in.Now) {
			break
		}
		if scanned >= 100000 {
			return out, drops, fmt.Errorf("reference schedule scan exceeds 100000 instants")
		}
		candidates = append(candidates, due.UTC())
		cursor = due
	}
	if in.Now.After(out.Checkpoint.Through) {
		out.Checkpoint.Through = in.Now.UTC()
	}
	slices.SortFunc(candidates, func(a, b time.Time) int { return a.Compare(b) })
	candidates = slices.CompactFunc(candidates, func(a, b time.Time) bool { return a.Equal(b) })
	original := slices.Clone(candidates)
	switch job.Catchup {
	case "latest":
		if len(candidates) > 1 {
			candidates = candidates[len(candidates)-1:]
		}
	case "skip":
		// Add durations separately so grace+tolerance cannot overflow.
		cutoff := in.Now.Add(-time.Duration(job.GraceSeconds) * time.Second).Add(-time.Duration(d.ToleranceSeconds) * time.Second)
		candidates = slices.DeleteFunc(candidates, func(at time.Time) bool { return at.Before(cutoff) })
	case "all":
		if d.CatchupWindowSeconds > 0 {
			cutoff := in.Now.Add(-time.Duration(d.CatchupWindowSeconds) * time.Second)
			candidates = slices.DeleteFunc(candidates, func(at time.Time) bool { return at.Before(cutoff) })
		}
		if d.CatchupLimit > 0 && len(candidates) > d.CatchupLimit {
			candidates = candidates[len(candidates)-d.CatchupLimit:]
		}
	}
	if d.DropRecords {
		drops = appendDrop(drops, "catchup_missed", removedInstants(original, candidates))
	}
	if job.Overlap == "forbid" && in.ActiveRuns > 0 && d.ForbidDeferral == "drop_after_grace" {
		cutoff := in.Now.Add(-time.Duration(job.GraceSeconds) * time.Second).Add(-time.Duration(d.ToleranceSeconds) * time.Second)
		kept := []time.Time{}
		removed := []time.Time{}
		for _, at := range candidates {
			if at.Before(cutoff) {
				removed = append(removed, at)
			} else {
				kept = append(kept, at)
			}
		}
		candidates = kept
		if d.DropRecords {
			drops = appendDrop(drops, "overlap_skipped", removed)
		}
	}
	future := []time.Time{}
	due := []time.Time{}
	for _, at := range candidates {
		if at.After(in.Now) {
			future = append(future, at)
		} else {
			due = append(due, at)
		}
	}
	out.Checkpoint.Pending = []time.Time{}
	if job.Overlap == "forbid" {
		out.Checkpoint.Pending = append(out.Checkpoint.Pending, due...)
		if in.ActiveRuns == 0 && len(due) > 0 {
			out.Launch = append(out.Launch, due[0])
			out.Checkpoint.Pending = append([]time.Time{}, due[1:]...)
		}
	} else {
		out.Launch = append(out.Launch, due...)
	}
	out.Checkpoint.Pending = append(out.Checkpoint.Pending, future...)
	return out, drops, nil
}
