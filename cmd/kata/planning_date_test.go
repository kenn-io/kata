package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeadlineSetsTimedDeadlineAndClearsIt(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "timed deadline issue")

	out := runCLI(t, env, dir, "deadline", ref, "2026-09-01T09:30")
	assert.Contains(t, out, "deadline_on")
	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"deadline_on":"2026-09-01T09:30"}`, string(issue.Issue.Metadata))

	out = runCLI(t, env, dir, "deadline", ref, "-")
	assert.Contains(t, out, "deadline_on")
	issue = fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

func TestDeadlineAcceptsUTCInstantAndRejectsNumericOffset(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "deadline instant issue")

	runCLI(t, env, dir, "deadline", ref, "2026-09-01T16:30:00Z")
	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"deadline_on":"2026-09-01T16:30:00Z"}`, string(issue.Issue.Metadata))

	_, stderr, err := runCLIWithErr(t, env, dir, "deadline", ref, "2026-09-01T09:30:00-07:00")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "invalid_metadata_value")
}

func TestDeadlineHonorsIfMatch(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "conditional deadline issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "deadline", "--if-match", "rev-99", ref, "2026-09-01")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Contains(t, stderr, "revision conflict")

	runCLI(t, env, dir, "deadline", "--if-match", "1", ref, "2026-09-01")
	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"deadline_on":"2026-09-01"}`, string(issue.Issue.Metadata))
}

func TestScheduleSetsTimedGateAndClearsIt(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "timed schedule issue")

	out := runCLI(t, env, dir, "schedule", ref, "2026-09-01T09:30")
	assert.Contains(t, out, "scheduled_on")
	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"scheduled_on":"2026-09-01T09:30"}`, string(issue.Issue.Metadata))

	out = runCLI(t, env, dir, "schedule", ref, "-")
	assert.Contains(t, out, "scheduled_on")
	issue = fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

func TestScheduleHonorsIfMatch(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "conditional schedule issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "schedule", "--if-match", "rev-99", ref, "2026-09-01")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Contains(t, stderr, "revision conflict")

	runCLI(t, env, dir, "schedule", "--if-match", "1", ref, "2026-09-01")
	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"scheduled_on":"2026-09-01"}`, string(issue.Issue.Metadata))
}
