package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelete_NoForceIsValidationError(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	err := f.execute("delete", short)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "--force")
}

func TestDelete_ForceWithConfirmSoftDeletes(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	require.NoError(t, f.execute("delete", short, "--force", "--confirm", "DELETE kata#"+short))
	assert.Contains(t, f.buf.String(), "deleted")
}

func TestDelete_AgentOutput(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env, dir, _ := setupCLIWorkspace(t)
	short := createIssueViaHTTP(t, env, dir, "to be deleted")

	out := runCLI(t, env, dir, "--agent", "delete", short, "--force", "--confirm", "DELETE kata#"+short)

	assert.Regexp(t, `(?m)^OK delete \S+`, out)
	assert.Contains(t, out, "Status: deleted")
	assert.NotContains(t, out, "Status: open")
	assert.Contains(t, out, "Undo: kata restore kata#"+short+" --agent")
}

func TestDelete_AgentUndoUsesQualifiedRef(t *testing.T) { //nolint:paralleltest // swaps package var flags
	resetFlags(t)
	flags.Mode = outputAgent
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	body := []byte(`{"issue":{"short_id":"abc4","qualified_id":"other#abc4","title":"to be deleted","status":"open"},"changed":true}`)

	require.NoError(t, printDestructive(cmd, "other#abc4", "delete", body))

	out := buf.String()
	assert.Contains(t, out, "Undo: kata restore other#abc4 --agent")
	assert.NotContains(t, out, "Undo: kata restore abc4 --agent")
}

func TestDelete_AgentUndoQuotesQualifiedRefForShell(t *testing.T) { //nolint:paralleltest // swaps package var flags
	resetFlags(t)
	flags.Mode = outputAgent
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	body := []byte(`{"issue":{"short_id":"abc4","qualified_id":"Kata Tracker#abc4","title":"to be deleted","status":"open"},"changed":true}`)

	require.NoError(t, printDestructive(cmd, "Kata Tracker#abc4", "delete", body))

	out := buf.String()
	assert.Contains(t, out, "Undo: kata restore 'Kata Tracker#abc4' --agent")
	assert.NotContains(t, out, "Undo: kata restore Kata Tracker#abc4 --agent")
}

func TestDelete_ConfirmMismatchIsExit6(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	err := f.execute("delete", short, "--force", "--confirm", "DELETE wrong")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.True(t, strings.Contains(ce.Code, "confirm_mismatch"))
}

// TestDelete_NoTTYNoConfirmIsConfirmRequired pins the resolveConfirm branch
// where stdin isn't a TTY and --confirm wasn't passed.
func TestDelete_NoTTYNoConfirmIsConfirmRequired(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	stubIsTTY(t, false)
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	err := f.execute("delete", short, "--force")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, "confirm_required", ce.Code)
}
