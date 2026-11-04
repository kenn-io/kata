package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

func bundlePlanFixture(t *testing.T) Bundle {
	t.Helper()
	b := testBundle()
	job := JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: Trigger{Kind: "issue-scheduled", IssueUID: bundleTestUID}, Action: Action{Kind: "execute", WorkflowUID: bundleWorkflowUID}, Issue: &IssuePolicy{Kind: "existing", UID: bundleTestUID}, Overlap: "forbid", Catchup: "latest", CheckoutKey: "checkout", Options: jsontext.Value(`{"source":"` + bundleTestUID + `","large":9007199254740993}`)}
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	b.Jobs = []BundleDefinition{{UID: bundleTestUID, DefinitionEventUID: bundleTestUID, Name: "Inspect", Definition: raw}}
	return b
}

func drawBundlePlanCase(t *testing.T, ht *hegel.T) (Bundle, BundleTarget, string) {
	b := bundlePlanFixture(t)
	sourceSeed := hegel.Draw(ht, hegel.Text())
	targetSeed := hegel.Draw(ht, hegel.Text())
	issueSeed := hegel.Draw(ht, hegel.Text())
	millis := hegel.Draw(ht, hegel.Integers[int64](0, (1<<48)-1))
	stamp := time.UnixMilli(millis)
	makeUID := func(seed string) string {
		id, err := uid.FromStableSeed([]byte(seed), stamp)
		require.NoError(ht, err)
		return id
	}
	sourceIssue := makeUID("source issue:" + sourceSeed)
	targetIssue := makeUID("target issue:" + issueSeed)
	b.Source.ProjectUID = makeUID("source project:" + sourceSeed)
	b.Workflows[0].UID = makeUID("workflow:" + sourceSeed)
	b.Jobs[0].UID = makeUID("job:" + sourceSeed)
	if hegel.Draw(ht, hegel.Booleans()) {
		b.Jobs[0].UID = b.Workflows[0].UID
	}
	workflow, err := ParseWorkflow(b.Workflows[0].Definition)
	require.NoError(ht, err)
	workflow.Steps[0].Command = "echo " + sourceIssue
	b.Workflows[0].Definition, err = json.Marshal(workflow)
	require.NoError(ht, err)
	job, err := ParseJob(b.Jobs[0].Definition)
	require.NoError(ht, err)
	number := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
	job.Options = []byte(fmt.Sprintf(`{"source":%q,"large":%d}`, sourceIssue, number))
	job.Trigger.IssueUID = sourceIssue
	job.Issue.UID = sourceIssue
	job.Action.WorkflowUID = b.Workflows[0].UID
	switch hegel.Draw(ht, hegel.Integers(0, 2)) {
	case 1:
		job.Trigger = Trigger{Kind: "interval", IntervalSeconds: 3600}
		job.Action = Action{Kind: "notify", Recipient: "current-owner-or-author", Message: sourceIssue}
		job.Issue = &IssuePolicy{Kind: "per-run", ParentUID: sourceIssue, Title: sourceIssue}
		job.CheckoutKey = ""
		job.Options = nil
		job.Overlap = "allow"
	case 2:
		job.Trigger = Trigger{Kind: "issue-deadline", IssueUID: sourceIssue}
		job.Action = Action{Kind: "notify", Recipient: "operator", Message: sourceIssue}
		job.CheckoutKey = ""
	}
	b.Jobs[0].Definition, err = json.Marshal(job, json.Deterministic(true))
	require.NoError(ht, err)
	return b, BundleTarget{ProjectUID: makeUID("target project:" + targetSeed), IssueMappings: map[string]string{sourceIssue: targetIssue}}, targetIssue
}

func TestBundlePlanRemapsOnlyReferencesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b, target, issue := drawBundlePlanCase(t, ht)
		source, err := ParseJob(b.Jobs[0].Definition)
		require.NoError(ht, err)
		plan, err := PlanBundle(b, target)
		require.NoError(ht, err)
		job, err := ParseJob(plan.Jobs[0].Definition)
		require.NoError(ht, err)
		if source.Action.WorkflowUID != "" {
			require.Equal(ht, plan.Workflows[0].UID, job.Action.WorkflowUID)
		} else {
			require.Empty(ht, job.Action.WorkflowUID)
		}
		if source.Trigger.IssueUID != "" {
			require.Equal(ht, issue, job.Trigger.IssueUID)
		}
		if source.Issue.UID != "" {
			require.Equal(ht, issue, job.Issue.UID)
		}
		if source.Issue.ParentUID != "" {
			require.Equal(ht, issue, job.Issue.ParentUID)
		}
		require.Equal(ht, uid.MustTime(b.Jobs[0].UID), uid.MustTime(plan.Jobs[0].UID))
	}, hegel.WithTestCases(100))
}

func TestBundlePlanPreservesOpaqueContentProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b, target, _ := drawBundlePlanCase(t, ht)
		source, err := ParseJob(b.Jobs[0].Definition)
		require.NoError(ht, err)
		plan, err := PlanBundle(b, target)
		require.NoError(ht, err)
		job, err := ParseJob(plan.Jobs[0].Definition)
		require.NoError(ht, err)
		require.Equal(ht, source.Options, job.Options)
		require.Equal(ht, source.Action.Message, job.Action.Message)
		require.Equal(ht, source.Issue.Title, job.Issue.Title)
		workflow, err := ParseWorkflow(plan.Workflows[0].Definition)
		require.NoError(ht, err)
		original, err := ParseWorkflow(b.Workflows[0].Definition)
		require.NoError(ht, err)
		require.Equal(ht, original.Steps, workflow.Steps)
	}, hegel.WithTestCases(100))
}

func TestBundlePlanInputImmutabilityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b, target, _ := drawBundlePlanCase(t, ht)
		before, err := json.Marshal(b)
		require.NoError(ht, err)
		mappings, err := json.Marshal(target.IssueMappings)
		require.NoError(ht, err)
		_, err = PlanBundle(b, target)
		require.NoError(ht, err)
		after, err := json.Marshal(b)
		require.NoError(ht, err)
		require.Equal(ht, before, after)
		afterMappings, err := json.Marshal(target.IssueMappings)
		require.NoError(ht, err)
		require.Equal(ht, mappings, afterMappings)
	}, hegel.WithTestCases(100))
}

func TestBundlePlanDeterminismAndDomainProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b := bundlePlanFixture(t)
		seed := hegel.Draw(ht, hegel.Text())
		a, err := uid.FromStableSeed([]byte("a:"+seed), time.Unix(1700000000, 0))
		require.NoError(ht, err)
		z, err := uid.FromStableSeed([]byte("z:"+seed), time.Unix(1700000000, 0))
		require.NoError(ht, err)
		target := BundleTarget{ProjectUID: a, IssueMappings: map[string]string{bundleTestUID: bundleWorkflowUID}}
		first, err := PlanBundle(b, target)
		require.NoError(ht, err)
		again, err := PlanBundle(b, target)
		require.NoError(ht, err)
		require.Equal(ht, first, again)
		target.ProjectUID = z
		other, err := PlanBundle(b, target)
		require.NoError(ht, err)
		require.NotEqual(ht, first.Jobs[0].UID, other.Jobs[0].UID)
		require.NotEqual(ht, first.Workflows[0].UID, other.Workflows[0].UID)
	}, hegel.WithTestCases(100))
}

func TestBundlePlanMissingMappingsAndSameProject(t *testing.T) {
	b := bundlePlanFixture(t)
	_, err := PlanBundle(b, BundleTarget{ProjectUID: bundleWorkflowUID})
	require.ErrorContains(t, err, bundleTestUID)
	plan, err := PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID})
	require.NoError(t, err)
	job, err := ParseJob(plan.Jobs[0].Definition)
	require.NoError(t, err)
	require.Equal(t, bundleTestUID, job.Issue.UID)
	// Explicit mappings override retention in the same project.
	plan, err = PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID, IssueMappings: map[string]string{bundleTestUID: bundleWorkflowUID}})
	require.NoError(t, err)
	job, err = ParseJob(plan.Jobs[0].Definition)
	require.NoError(t, err)
	require.Equal(t, bundleWorkflowUID, job.Issue.UID)
	require.NotEmpty(t, plan.Review)
	b.Workflows[0].Definition = []byte(`{"version":99,"steps":[]}`)
	_, err = PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID})
	require.Error(t, err, "import enforces current write rules")
}

func TestBundlePlanNativeParentAndMissingList(t *testing.T) {
	b := bundlePlanFixture(t)
	b.Jobs[0].Definition = []byte(fmt.Sprintf(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":%q,"title":"Inspect"},"overlap":"allow","catchup":"all"}`, bundleTestUID))
	plan, err := PlanBundle(b, BundleTarget{ProjectUID: bundleWorkflowUID, IssueMappings: map[string]string{bundleTestUID: bundleWorkflowUID}})
	require.NoError(t, err)
	job, err := ParseJob(plan.Jobs[0].Definition)
	require.NoError(t, err)
	require.Equal(t, bundleWorkflowUID, job.Issue.ParentUID)
	require.False(t, job.Enabled)
}

func TestBundlePlanNormalizesWriteDefaults(t *testing.T) {
	b := bundlePlanFixture(t)
	b.Workflows[0].Definition = []byte(`{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status","after":[],"retries":0}]}`)
	plan, err := PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID})
	require.NoError(t, err)
	require.NotContains(t, string(plan.Workflows[0].Definition), `"after":[]`, "the API omits empty optional fields; normalized plans must compare unchanged on retry")
	require.NotContains(t, string(plan.Workflows[0].Definition), `"retries":0`)
}

func TestBundlePlanStableSeedGolden(t *testing.T) {
	b := bundlePlanFixture(t)
	plan, err := PlanBundle(b, BundleTarget{ProjectUID: bundleWorkflowUID, IssueMappings: map[string]string{bundleTestUID: bundleWorkflowUID}})
	require.NoError(t, err)
	// Independently computed from the ULID timestamp, SHA-256 seed bytes and Crockford encoding.
	require.Equal(t, "01ARZ3NDEKWS85Y1WRYKMDFE5G", plan.Jobs[0].UID)
	require.Equal(t, uid.MustTime(bundleTestUID), uid.MustTime(plan.Jobs[0].UID))
}

func TestBundlePlanEnabledTruthTableProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		b := bundlePlanFixture(t)
		enabled, keep := hegel.Draw(ht, hegel.Booleans()), hegel.Draw(ht, hegel.Booleans())
		job, err := ParseJob(b.Jobs[0].Definition)
		require.NoError(ht, err)
		job.Enabled = enabled
		b.Jobs[0].Definition, err = json.Marshal(job)
		require.NoError(ht, err)
		plan, err := PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID, KeepEnabled: keep})
		require.NoError(ht, err)
		actual, err := ParseJob(plan.Jobs[0].Definition)
		require.NoError(ht, err)
		require.Equal(ht, enabled && keep, actual.Enabled)
	}, hegel.WithTestCases(100))
}

func TestBundlePlanCompleteMissingMappingList(t *testing.T) {
	b := bundlePlanFixture(t)
	second := b.Jobs[0]
	second.UID = bundleWorkflowUID
	var job JobDefinition
	require.NoError(t, json.Unmarshal(second.Definition, &job))
	job.Trigger.IssueUID = bundleWorkflowUID
	job.Issue.UID = bundleWorkflowUID
	raw, err := json.Marshal(job)
	require.NoError(t, err)
	second.Definition = raw
	b.Jobs = append(b.Jobs, second)
	_, err = PlanBundle(b, BundleTarget{ProjectUID: bundleWorkflowUID})
	require.ErrorContains(t, err, bundleTestUID)
	require.ErrorContains(t, err, bundleWorkflowUID)
}

func TestBundlePlanOutputBuffersAndMappingAreIndependent(t *testing.T) {
	b := bundlePlanFixture(t)
	mapping := map[string]string{bundleTestUID: bundleWorkflowUID}
	before, err := json.Marshal(b)
	require.NoError(t, err)
	plan, err := PlanBundle(b, BundleTarget{ProjectUID: bundleWorkflowUID, IssueMappings: mapping})
	require.NoError(t, err)
	plan.Jobs[0].Definition[0] = '['
	plan.Workflows[0].Definition[0] = '['
	after, err := json.Marshal(b)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, bundleWorkflowUID, mapping[bundleTestUID])
}

func TestBundlePlanFinalDefinitionLimit(t *testing.T) {
	b := bundlePlanFixture(t)
	job, err := ParseJob(b.Jobs[0].Definition)
	require.NoError(t, err)
	job.Options = []byte(`{"padding":""}`)
	raw, err := json.Marshal(job, json.Deterministic(true))
	require.NoError(t, err)
	job.Options = []byte(`{"padding":"` + strings.Repeat("x", DefinitionLimit-len(raw)) + `"}`)
	raw, err = json.Marshal(job, json.Deterministic(true))
	require.NoError(t, err)
	require.Len(t, raw, 262144)
	b.Jobs[0].Definition = raw
	_, err = PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID})
	require.ErrorContains(t, err, "size")
	plan, err := PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID, KeepEnabled: true})
	require.NoError(t, err)
	require.Len(t, plan.Jobs[0].Definition, 262144)
}

func TestBundlePlanReviewFieldsAndOrder(t *testing.T) {
	b := bundlePlanFixture(t)
	var workflow WorkflowDefinition
	require.NoError(t, json.Unmarshal(b.Workflows[0].Definition, &workflow))
	workflow.Options = []byte(`{}`)
	workflow.Steps[0].Options = []byte(`{"large":9007199254740993}`)
	raw, err := json.Marshal(workflow)
	require.NoError(t, err)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &fields))
	fields["options"] = []byte(`{}`) // Preserve explicit empty options in the input bundle fixture.
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	b.Workflows[0].Definition = raw
	var job JobDefinition
	require.NoError(t, json.Unmarshal(b.Jobs[0].Definition, &job))
	job.SecretRefs = map[string]string{"service": "credential"}
	raw, err = json.Marshal(job)
	require.NoError(t, err)
	b.Jobs[0].Definition = raw
	native := BundleDefinition{UID: bundleWorkflowUID, DefinitionEventUID: bundleTestUID, Name: "Native", Definition: []byte(`{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"interval","interval_seconds":3600},"action":{"kind":"notify","recipient":"current-owner-or-author"},"issue":{"kind":"per-run","parent_uid":"` + bundleWorkflowUID + `","title":"Inspect"},"overlap":"allow","catchup":"all"}`)}
	b.Jobs = append(b.Jobs, native)
	target := BundleTarget{ProjectUID: b.Source.ProjectUID, IssueMappings: map[string]string{bundleTestUID: bundleWorkflowUID, bundleWorkflowUID: bundleTestUID}}
	plan, err := PlanBundle(b, target)
	require.NoError(t, err)
	require.Equal(t, []BundleReview{{Kind: "workflow", SourceUID: bundleWorkflowUID, Fields: []string{"options", "steps[0].options"}}, {Kind: "job", SourceUID: bundleTestUID, Fields: []string{"checkout_key", "secret_refs", "options"}}, {Kind: "job", SourceUID: bundleWorkflowUID, Fields: []string{"action.recipient"}}}, plan.Review)
	first, err := ParseJob(plan.Jobs[0].Definition)
	require.NoError(t, err)
	require.Equal(t, bundleWorkflowUID, first.Issue.UID)
	second, err := ParseJob(plan.Jobs[1].Definition)
	require.NoError(t, err)
	require.Equal(t, bundleTestUID, second.Issue.ParentUID)
	b.Jobs[0], b.Jobs[1] = b.Jobs[1], b.Jobs[0]
	reordered, err := PlanBundle(b, target)
	require.NoError(t, err)
	require.Equal(t, plan, reordered)
}

func TestBundlePlanRejectsInvalidTargetAndUnusedMappings(t *testing.T) {
	for _, target := range []BundleTarget{{ProjectUID: ""}, {ProjectUID: strings.ToLower(bundleTestUID)}, {ProjectUID: bundleTestUID, IssueMappings: map[string]string{"bad": bundleTestUID}}, {ProjectUID: bundleTestUID, IssueMappings: map[string]string{bundleTestUID: "bad"}}} {
		b := testBundle()
		b.Workflows = nil
		_, err := PlanBundle(b, target)
		require.ErrorIs(t, err, ErrInvalid)
	}
	for _, source := range []string{"00000000000000000000000000", "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"} {
		b := bundlePlanFixture(t)
		b.Jobs[0].UID = source
		plan, err := PlanBundle(b, BundleTarget{ProjectUID: b.Source.ProjectUID})
		require.NoError(t, err)
		require.Equal(t, uid.MustTime(source), uid.MustTime(plan.Jobs[0].UID))
	}
}
