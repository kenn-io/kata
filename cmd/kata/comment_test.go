package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestComment_AppendsToIssue(t *testing.T) {
	env, dir := setupCLIEnv(t)
	short := createIssueViaHTTP(t, env, dir, "x")

	out := runCLI(t, env, dir, "comment", short, "--body", "looks good")
	assert.True(t, strings.Contains(out, "looks good") || strings.Contains(out, "comment"))
}

func TestComment_AgentOutput(t *testing.T) {
	env, dir := setupCLIEnv(t)
	short := createIssueViaHTTP(t, env, dir, "x")

	out := runCLI(t, env, dir, "--agent", "comment", short, "--body", "looks good")

	assert.Regexp(t, `(?m)^OK comment \S+`, out)
	assert.Contains(t, out, "Comment: appended")
}
