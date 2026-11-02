package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// M3: the first command is executed against the state that refused the close.
func TestVerificationRefusalHintsExecute(t *testing.T) {
	for _, kind := range []string{"missing citation", "malformed citation", "missing comment", "self confirmation", "wrong pairing"} {
		t.Run(kind, func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Completion claim")
			runCLI(t, env, dir, "meta", "set", ref, "verify.required", "true", "--json-value")
			args := []string{"close", ref, "--done", "-m", "Completed the requested change and checked the focused suite.", "--test", "go test ./example"}
			if kind != "missing citation" {
				issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
				require.NoError(t, err)
				claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Claim"})
				require.NoError(t, err)
				switch kind {
				case "malformed citation":
					args = append(args, "--confirmed", "invalid")
				case "missing comment":
					args = append(args, "--confirmed", "c:aaaaaa")
				case "self confirmation":
					args = append(args, "--confirmed", claim.UID)
				case "wrong pairing":
					args = append(args, "--refuted", claim.UID)
				}
			}
			_, _, err := runCLIWithErr(t, env, dir, args...)
			require.Error(t, err)
			cli := cliErrorForErr(err, true)
			words, parseErr := splitHelpWords(strings.Split(cli.Hint, "\n")[0])
			require.NoError(t, parseErr)
			require.NotEmpty(t, words)
			require.Equal(t, "kata", words[0])
			_, stderr, hintErr := runCLIWithErr(t, env, dir, words[1:]...)
			require.NoError(t, hintErr, stderr)
		})
	}
}

func TestVerificationWarningHintExecutesAfterClose(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Claim")
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Claim"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Independent unresolved review", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	_, stderr, err := runCLIWithErr(t, env, dir, "close", ref, "--done", "-m", "Completed the requested change and checked the focused suite.", "--test", "go test ./example")
	require.NoError(t, err)
	_, hint, found := strings.Cut(stderr, "hint: ")
	require.True(t, found, stderr)
	words, err := splitHelpWords(strings.Split(hint, "\n")[0])
	require.NoError(t, err)
	require.Equal(t, "show", words[1])
	_, errorText, hintErr := runCLIWithErr(t, env, dir, words[1:]...)
	require.NoError(t, hintErr, errorText)
}

func TestVerificationHintKeepsExplicitDaemon(t *testing.T) {
	resetFlags(t)
	flags.Daemon = "example-daemon"
	hint := selectedVerificationHint("kata show 'example-project#abc4'\nResolve first.\nkata close 'example-project#abc4' --done")
	require.Equal(t, "kata --daemon 'example-daemon' show 'example-project#abc4'", strings.Split(hint, "\n")[0])
	require.Contains(t, hint, "kata --daemon 'example-daemon' close")
}

func TestShowRequiredHintExecutes(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "Completion claim")
	runCLI(t, env, dir, "meta", "set", ref, "verify.required", "true", "--json-value")
	output := runCLI(t, env, dir, "show", ref)
	_, hint, found := strings.Cut(output, "Hint: ")
	require.True(t, found, output)
	words, err := splitHelpWords(strings.Split(hint, "\n")[0])
	require.NoError(t, err)
	require.Equal(t, "show", words[1])
	_, stderr, err := runCLIWithErr(t, env, dir, words[1:]...)
	require.NoError(t, err, stderr)
}

func TestVerificationOverruleHintKeepsMessageAndEvidence(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssueOptions(t, "Claim", func(cfg *daemon.ServerConfig) { cfg.CloseVerification = "block" })
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Claim"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Unresolved concern", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	message := `Finished the "retry fix" and independently checked its focused regression suite.`
	evidence := "go test ./example -run TestRetry"
	_, _, err = runCLIWithErr(t, env, dir, "close", ref, "--done", "--test", evidence, "-m", message)
	require.Error(t, err)
	hint := cliErrorForErr(err, true).Hint
	lines := strings.Split(hint, "\n")
	retry := lines[len(lines)-1]
	words, err := splitHelpWords(retry)
	require.NoError(t, err)
	require.Equal(t, "close", words[1])
	require.Contains(t, words, message)
	require.Contains(t, words, "test:"+evidence)
	require.Contains(t, words, "--overrule")
	_, stderr, err := runCLIWithErr(t, env, dir, words[1:]...)
	require.NoError(t, err, stderr)
}
