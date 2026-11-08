package cron

import (
	"encoding/json/v2"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestRecurringDraftValidationRequiresUnresolvedNativeParent(t *testing.T) {
	var job JobDefinition
	require.NoError(t, json.Unmarshal([]byte(recurringJSON), &job))
	require.ErrorIs(t, job.ValidateBeforeParentResolution(), ErrInvalid, "already-bound documents use normal validation")
	job.Issue.ParentUID = ""
	require.NoError(t, job.ValidateBeforeParentResolution())
	job.Options = []byte(`{}`)
	require.NoError(t, job.ValidateBeforeParentResolution(), "empty native options remain valid")
	require.Empty(t, job.Issue.ParentUID)
	require.ErrorIs(t, job.Validate(), ErrInvalid, "draft validation must not weaken normal writes")
	job.Action.Kind = "execute"
	require.ErrorIs(t, job.ValidateBeforeParentResolution(), ErrInvalid)
	job.Issue = nil
	require.ErrorIs(t, job.ValidateBeforeParentResolution(), ErrInvalid)
}

func TestRecurringDraftKeepsNonParentRules(t *testing.T) {
	for name, change := range map[string]func(*JobDefinition){
		"version":           func(j *JobDefinition) { j.Version = 2 },
		"kind":              func(j *JobDefinition) { j.Kind = "unknown" },
		"manual trigger":    func(j *JobDefinition) { j.Trigger = Trigger{Kind: "manual"} },
		"execution prompt":  func(j *JobDefinition) { j.Action.Prompt = "Inspect" },
		"existing UID":      func(j *JobDefinition) { j.Issue.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAV" },
		"scheduled offset":  func(j *JobDefinition) { j.Issue.ScheduledOffsetSeconds = new(int64) },
		"deadline offset":   func(j *JobDefinition) { j.Issue.DeadlineOffsetSeconds = new(int64) },
		"secret references": func(j *JobDefinition) { j.SecretRefs = map[string]string{"key": "reference"} },
		"adapter options":   func(j *JobDefinition) { j.Options = []byte(`{"adapter":"example-adapter"}`) },
		"null options":      func(j *JobDefinition) { j.Options = []byte(`null`) },
		"malformed options": func(j *JobDefinition) { j.Options = []byte(`{`) },
		"long title":        func(j *JobDefinition) { j.Issue.Title = strings.Repeat("x", 1024) },
	} {
		t.Run(name, func(t *testing.T) {
			var job JobDefinition
			require.NoError(t, json.Unmarshal([]byte(recurringJSON), &job))
			job.Issue.ParentUID = ""
			change(&job)
			require.ErrorIs(t, job.ValidateBeforeParentResolution(), ErrInvalid)
		})
	}
}

func TestRecurringDraftValidationAgreesWithResolvedDefinition(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		var draft JobDefinition
		if err := json.Unmarshal([]byte(recurringJSON), &draft); err != nil {
			ht.Fatal(err)
		}
		draft.Issue.ParentUID = ""
		switch hegel.Draw(ht, hegel.Integers(0, 6)) {
		case 0:
			draft.Enabled = hegel.Draw(ht, hegel.Booleans())
		case 1:
			draft.Trigger.IntervalSeconds = hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		case 2:
			draft.Catchup = hegel.Draw(ht, hegel.Text())
		case 3:
			draft.Issue.Title = hegel.Draw(ht, hegel.Text())
		case 4:
			draft.Issue.Body = hegel.Draw(ht, hegel.Text())
		case 5:
			draft.GraceSeconds = hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		case 6:
			draft.Action.Recipient = hegel.Draw(ht, hegel.Text())
		}
		bound := draft
		parent := *draft.Issue
		parent.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		bound.Issue = &parent
		if (draft.ValidateBeforeParentResolution() == nil) != (bound.Validate() == nil) {
			ht.Fatal("draft and bound validation disagree on the same non-parent fields")
		}
	})
}

func TestRecurringDraftReservesResolvedParentBytes(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		var bound JobDefinition
		require.NoError(t, json.Unmarshal([]byte(recurringJSON), &bound))
		bound.Issue.Body = "b"
		raw, err := json.Marshal(bound)
		require.NoError(t, err)
		bound.Issue.Body = strings.Repeat("b", DefinitionLimit-len(raw)+1+delta)
		raw, err = json.Marshal(bound)
		require.NoError(t, err)
		require.Len(t, raw, DefinitionLimit+delta)
		draft := bound
		parent := *bound.Issue
		parent.ParentUID = ""
		draft.Issue = &parent
		require.Equal(t, delta <= 0, bound.Validate() == nil)
		require.Equal(t, delta <= 0, draft.ValidateBeforeParentResolution() == nil)
	}
}
