package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestSmoke_ExportImport(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")

	firstOut := runCLI(t, env, dir, "--json", "create", "export smoke one", "--body", "orchid body", "--label", "bug")
	var first struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(firstOut), &first))

	runCLI(t, env, dir, "create", "export smoke two", "--body", "blocks first", "--blocks", first.Issue.ShortID)
	runCLI(t, env, dir, "comment", first.Issue.ShortID, "--body", "watermelon note")

	before := runCLI(t, env, dir, "--json", "list")
	beforeShow := runCLI(t, env, dir, "--json", "show", first.Issue.ShortID)
	assert.Contains(t, beforeShow, "watermelon note")
	exportPath := filepath.Join(env.Home, "smoke.jsonl")
	runCLI(t, env, dir, "export", "--output", exportPath)
	targetPath := filepath.Join(t.TempDir(), "imported.db")
	runCLI(t, env, dir, "import", "--input", exportPath, "--target", targetPath)

	imported := testenv.NewFromDB(t, targetPath)
	after := runCLI(t, imported, dir, "--json", "list")
	afterShow := runCLI(t, imported, dir, "--json", "show", first.Issue.ShortID)

	assert.JSONEq(t, before, after)
	assert.JSONEq(t, beforeShow, afterShow)
}
