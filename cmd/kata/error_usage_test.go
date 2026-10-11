package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorUsageCloseReasons(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	for _, tc := range []struct {
		name    string
		flags   []string
		kind    errKind
		exit    int
		message string
	}{
		{"missing", nil, kindUsage, ExitUsage, "close needs a reason: --done (with --commit, --pr, --test, or --reviewed), --wontfix, --duplicate-of <ref>, --superseded-by <ref>, or --audit-no-change"},
		{"multiple sugar", []string{"--done", "--wontfix"}, kindUsage, ExitUsage, "multiple reason sugar flags"},
		{"canonical and sugar", []string{"--reason", "done", "--done"}, kindUsage, ExitUsage, "corresponding sugar flag"},
		{"duplicate evidence", []string{"--done", "--commit", "abc1234", "--evidence", "commit:abc1234"}, kindUsage, ExitUsage, "duplicate evidence item"},
		{"duplicate reviewed path", []string{"--done", "--reviewed", "a", "--reviewed", "a"}, kindUsage, ExitUsage, "duplicate path"},
		{"malformed evidence", []string{"--done", "--evidence", "broken"}, kindValidation, ExitValidation, "expected <type>:<value>"},
		{"unknown evidence type", []string{"--done", "--evidence", "other:value"}, kindValidation, ExitValidation, "unknown type"},
		{"empty issue reference", []string{"--done", "--evidence", "duplicate-of:"}, kindValidation, ExitValidation, "expected issue ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"close", "abc4", "--json"}, tc.flags...)
			stdout, stderr, err := executeRootCapture(t, context.Background(), args...)
			require.Error(t, err)
			assert.Equal(t, tc.exit, exitCodeForErr(err, runEEntered))
			assert.Empty(t, stdout)
			var got struct {
				Error struct {
					Kind     string `json:"kind"`
					ExitCode int    `json:"exit_code"`
					Message  string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(stderr), &got))
			assert.Equal(t, string(tc.kind), got.Error.Kind)
			assert.Equal(t, tc.exit, got.Error.ExitCode)
			assert.Contains(t, got.Error.Message, tc.message)
		})
	}
}

func TestErrorUsageUnknownFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{"edit long", []string{"edit", "abc4", "--label", "x", "--agent"}, "kata edit"},
		{"nested long", []string{"label", "add", "abc4", "bug", "--typo", "--agent"}, "kata label add"},
		{"nested shorthand", []string{"label", "add", "abc4", "bug", "-z", "--agent"}, "kata label add"},
		{"root long", []string{"--typo", "--agent"}, "kata"},
		{"json", []string{"edit", "abc4", "--label", "x", "--json"}, "kata edit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := executeRootCapture(t, context.Background(), tc.args...)
			require.Error(t, err)
			assert.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
			assert.Empty(t, stdout)
			hint := `run "` + tc.path + ` --help" for flags`
			assert.Contains(t, err.Error(), "unknown")
			if tc.name == "json" {
				var got struct {
					Error struct {
						Hint string `json:"hint"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(stderr), &got))
				assert.Equal(t, hint, got.Error.Hint)
			} else {
				assert.Contains(t, stderr, "\nHint: "+hint+"\n")
			}
		})
	}
}

func TestErrorUsageNegativePositionKeepsAdvice(t *testing.T) {
	_, stderr, err := executeRootCapture(t, context.Background(), "show", "-1", "--agent")
	require.Error(t, err)
	assert.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
	assert.Contains(t, err.Error(), "negative numbers in positional args")
	assert.Contains(t, stderr, "kata show -- -1")
	assert.NotContains(t, stderr, "Hint:")
}
