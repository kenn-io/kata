package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func drawPreviewJob(ht *hegel.T) JobDefinition {
	job := previewJob()
	job.Enabled = hegel.Draw(ht, hegel.Booleans())
	zone := hegel.Draw(ht, hegel.SampledFrom([]string{"UTC", "America/New_York", "Australia/Lord_Howe", "Pacific/Kiritimati"}))
	native := hegel.Draw(ht, hegel.Booleans())
	if native {
		job.Action = Action{Kind: "notify", Recipient: "current-owner-or-author"}
		job.Issue.ParentUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		job.Overlap = "allow"
		job.Trigger = Trigger{Kind: "interval", IntervalSeconds: hegel.Draw(ht, hegel.Integers(int64(60), maxDurationSeconds)), Timezone: zone}
		if hegel.Draw(ht, hegel.Booleans()) {
			job.Trigger = Trigger{Kind: "cron", Cron: hegel.Draw(ht, hegel.SampledFrom([]string{"0 9 * * *", "30 1 * * *", "30 2 * * *", "0 0 * * *"})), Timezone: zone}
		}
	} else {
		job.Trigger = Trigger{Kind: hegel.Draw(ht, hegel.SampledFrom([]string{"cron", "interval", "manual", "once"})), Timezone: zone}
		switch job.Trigger.Kind {
		case "cron":
			job.Trigger.Cron = hegel.Draw(ht, hegel.SampledFrom([]string{"0 9 * * *", "30 1 * * *", "@every 1h", "CRON_TZ=" + zone + " 0 0 * * *"}))
		case "interval":
			job.Trigger.IntervalSeconds = hegel.Draw(ht, hegel.Integers(int64(1), maxDurationSeconds))
		case "once":
			job.Trigger.At = "2026-11-04T09:00:00Z"
		}
	}
	job.Catchup = hegel.Draw(ht, hegel.SampledFrom([]string{"latest", "all"}))
	return job
}

func normalizationCandidate(ht *hegel.T, workflow bool) []byte {
	if hegel.Draw(ht, hegel.Booleans()) {
		return []byte(hegel.Draw(ht, hegel.SampledFrom([]string{`null`, `[]`, `1e1000`, `"text"`, `{`, `{"unknown":1}`, `{"version":1e1000}`, `{"version":null}`})))
	}
	version := hegel.Draw(ht, hegel.OneOf(hegel.Integers(math.MinInt, math.MaxInt), hegel.SampledFrom([]int{0, 1, 2})))
	var value any
	if workflow {
		kind := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"prompt", "command"})))
		value = WorkflowDefinition{Version: version, Steps: []WorkflowStep{{Key: "review", Kind: kind, Prompt: "Review"}}}
	} else {
		job := drawPreviewJob(ht)
		job.Version = version
		value = job
	}
	raw, err := json.Marshal(value)
	require.NoError(ht, err)
	return raw
}

func TestCronValidationJobParserAgreement(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := normalizationCandidate(ht, false)
		_, want := ParseJob(raw)
		_, _, got := NormalizeJob(raw)
		require.Equal(ht, want == nil, got == nil)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronValidationWorkflowParserAgreement(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := normalizationCandidate(ht, true)
		_, want := ParseWorkflow(raw)
		_, _, got := NormalizeWorkflow(raw)
		require.Equal(ht, want == nil, got == nil)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronValidationPreservesInput(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		job := drawPreviewJob(ht)
		raw, err := json.Marshal(job)
		require.NoError(ht, err)
		_, normalized, err := NormalizeJob(raw)
		require.NoError(ht, err)
		parsed, err := ParseJob(normalized)
		require.NoError(ht, err)
		require.Equal(ht, job, parsed)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronValidationOpaqueNumberSpelling(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		integer := hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64)))
		exponent := hegel.Draw(ht, hegel.Integers(-300, 300))
		literal := fmt.Sprintf(`%dE%+d`, integer, exponent)
		nested := []byte(fmt.Sprintf(`{"nested":[{"value":%s}],"decimal":0.12345678901234567890123}`, literal))
		job := drawPreviewJob(ht)
		job.Options = nested
		raw, err := json.Marshal(job)
		require.NoError(ht, err)
		// Decoded documents retain normalized fields even if a value fails
		// validation (native options or an overflowing option number).
		_, normalized, _ := NormalizeJob(raw)
		require.NotEmpty(ht, normalized)
		var fields map[string]jsontext.Value
		require.NoError(ht, json.Unmarshal(normalized, &fields))
		require.Equal(ht, jsontext.Value(nested), fields["options"])
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func drawPreviewState(ht *hegel.T) (JobDefinition, *RecurringProgress, time.Time, int) {
	job := drawPreviewJob(ht)
	from := time.Unix(hegel.Draw(ht, hegel.Integers(int64(0), int64(2000000000))), 0).UTC()
	count := hegel.Draw(ht, hegel.Integers(1, 100))
	var stored *RecurringProgress
	if IsRecurringIssueJob(job) && hegel.Draw(ht, hegel.Booleans()) {
		anchor := from.Add(-time.Duration(hegel.Draw(ht, hegel.Integers(int64(0), int64(100000000)))) * time.Second)
		through := anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(int64(0), int64(200000000)))) * time.Second)
		stored = &RecurringProgress{Version: 1, Anchor: anchor, Through: through}
	}
	return job, stored, from, count
}

func TestCronPreviewOrderedFutureInstants(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		job, stored, from, count := drawPreviewState(ht)
		out, err := PreviewSchedule(job, stored, from, count)
		require.NoError(ht, err)
		require.LessOrEqual(ht, len(out.Next), count)
		previous := from
		if stored != nil && stored.Through.After(previous) {
			previous = stored.Through
		}
		for _, fire := range out.Next {
			require.True(ht, fire.At.After(previous))
			previous = fire.At
		}
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronPreviewOccurrenceAgreement(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		job, stored, from, count := drawPreviewState(ht)
		out, err := PreviewSchedule(job, stored, from, count)
		require.NoError(ht, err)
		progress := RecurringProgress{Version: 1, Anchor: from, Through: from}
		if stored != nil {
			progress = *stored
		}
		job.Enabled = true
		want, err := RecurringOccurrences(job, progress, from, count)
		require.NoError(ht, err)
		got := make([]time.Time, 0, len(out.Next))
		for _, fire := range out.Next {
			got = append(got, fire.At)
		}
		require.Equal(ht, append([]time.Time{}, want...), got)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronPreviewPausedInvariant(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		job, stored, from, count := drawPreviewState(ht)
		job.Enabled = false
		paused, err := PreviewSchedule(job, stored, from, count)
		require.NoError(ht, err)
		job.Enabled = true
		enabled, err := PreviewSchedule(job, stored, from, count)
		require.NoError(ht, err)
		require.Equal(ht, enabled, paused)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronPreviewTimestampDomain(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		job := drawPreviewJob(ht)
		year := hegel.Draw(ht, hegel.OneOf(hegel.Integers(0, 9999), hegel.SampledFrom([]int{0, 1, 9999})))
		from := time.Date(year, time.Month(hegel.Draw(ht, hegel.Integers(1, 12))), hegel.Draw(ht, hegel.Integers(1, 28)), 0, 0, 0, 0, time.UTC)
		out, err := PreviewSchedule(job, nil, from, hegel.Draw(ht, hegel.Integers(1, 100)))
		require.NoError(ht, err)
		_, err = json.Marshal(out)
		require.NoError(ht, err)
		for _, fire := range out.Next {
			_, err := time.Parse(time.RFC3339Nano, fire.Local)
			require.NoError(ht, err)
			require.Equal(ht, fire.At.In(TriggerLocation(job.Trigger)).Format(time.RFC3339Nano), fire.Local)
		}
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}
