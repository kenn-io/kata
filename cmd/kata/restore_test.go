package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRestore_ClearsDeletedAt(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "delete me")

	runCLI(t, env, dir, "delete", short, "--force", "--confirm", "DELETE kata#"+short)
	output := runCLI(t, env, dir, "restore", short)
	assert.Contains(t, output, "restored")
}

func TestRestore_AgentOutput(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "delete me")
	runCLI(t, env, dir, "delete", short, "--force", "--confirm", "DELETE kata#"+short)

	resetFlags(t)
	out := runCLI(t, env, dir, "--agent", "restore", short)

	assert.Regexp(t, `(?m)^OK restore \S+ changed=true`, out)
	assert.Contains(t, out, "Deleted: false")
}
