package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestMetaSetStoresJSONStringAndGetReturnsValue(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "metadata string issue")

	out := runCLI(t, env, dir, "meta", "set", ref, "work.attention", "needs-human")
	assert.Contains(t, out, "set")
	assert.Contains(t, out, ref)
	assert.Contains(t, out, "work.attention")
	assert.Contains(t, out, "rev-2")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"work.attention":"needs-human"}`, string(issue.Issue.Metadata))

	getOut := runCLI(t, env, dir, "meta", "get", ref, "work.attention")
	assert.Equal(t, "needs-human", getOut)
}

func TestMetaSetJSONValueObjectRoundTrips(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "metadata object issue")

	runCLI(t, env, dir, "meta", "set", "--json-value", ref, "work.state", `{"attention":"ok","count":2}`)

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"work.state":{"attention":"ok","count":2}}`, string(issue.Issue.Metadata))

	out := runCLI(t, env, dir, "meta", "get", "--json", ref, "work.state")
	assert.JSONEq(t, fmt.Sprintf(
		`{"kata_api_version":1,"ref":%q,"revision":2,"key":"work.state","value":{"attention":"ok","count":2}}`,
		ref), out)
}

func TestMetaSetJSONValueRejectsInvalidJSONClientSide(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "invalid json metadata issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "set", "--json-value", ref, "work.state", `{"broken"`)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "invalid JSON")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	assert.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

func TestMetaSetJSONValueRejectsNullWithUnsetHint(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "null metadata issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "set", "--json-value", ref, "work.state", `null`)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "use `kata meta unset`")
}

func TestMetaUnsetRemovesKey(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "unset metadata issue")

	runCLI(t, env, dir, "meta", "set", ref, "work.branch", "feature/example")
	out := runCLI(t, env, dir, "meta", "unset", ref, "work.branch")
	assert.Contains(t, out, "unset")
	assert.Contains(t, out, "work.branch")
	assert.Contains(t, out, "rev-3")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	assert.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

func TestMetaGetEmptyAndMissingKey(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "empty metadata issue")

	out := runCLI(t, env, dir, "meta", "get", ref)
	assert.Contains(t, out, "no metadata")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "get", ref, "work.missing")
	ce := requireCLIError(t, err, ExitNotFound)
	assert.Equal(t, kindNotFound, ce.Kind)
	assert.Contains(t, stderr, "metadata key not found")
}

func TestMetaReservedKeyRejectionSurfacesValidation(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "reserved metadata issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "set", ref, "scheduled_on", "not-a-date")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "invalid_metadata_value")
}

func TestMetaIfMatchStaleRevisionConflictsAndCorrectRevisionSucceeds(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "if match metadata issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "set", "--if-match", "rev-99", ref, "work.attention", "ok")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Contains(t, stderr, "revision conflict")

	out := runCLI(t, env, dir, "meta", "set", "--if-match", "1", ref, "work.attention", "ok")
	assert.Contains(t, out, "rev-2")
}

func TestMetaSetIfMatchEmptyValueRejectedAsMalformed(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "if match empty value issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "set", "--if-match", "", ref, "work.attention", "ok")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "--if-match")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	assert.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

func TestMetaSetIfMatchWhitespaceValueRejectedAsMalformed(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "if match whitespace value issue")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "set", "--if-match", "   ", ref, "work.attention", "ok")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "--if-match")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	assert.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

func TestMetaUnsetIfMatchEmptyValueRejectedAsMalformed(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "if match empty value unset issue")
	runCLI(t, env, dir, "meta", "set", ref, "work.branch", "feature/example")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "unset", "--if-match", "", ref, "work.branch")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Equal(t, kindValidation, ce.Kind)
	assert.Contains(t, stderr, "--if-match")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"work.branch":"feature/example"}`, string(issue.Issue.Metadata))
}

func TestMetaSetIfMatchAbsentStillUnconditional(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "if match absent issue")

	out := runCLI(t, env, dir, "meta", "set", ref, "work.attention", "ok")
	assert.Contains(t, out, "rev-2")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"work.attention":"ok"}`, string(issue.Issue.Metadata))
}

func TestMetaSetAndGetAgentOutput(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "agent metadata issue")

	setOut := runCLI(t, env, dir, "--agent", "meta", "set", ref, "work.attention", "stuck")
	assert.Contains(t, setOut, "OK meta set")
	assert.Contains(t, setOut, "key=work.attention")
	assert.Contains(t, setOut, "revision=rev-2")

	getOut := runCLI(t, env, dir, "--agent", "meta", "get", ref)
	assert.Contains(t, getOut, "OK meta get")
	assert.Contains(t, getOut, "key=work.attention")
	assert.Contains(t, getOut, `value="\"stuck\""`)
}

// TestMetaGetAgentQuotesValueWithSpaces: a metadata value containing spaces
// must be emitted as a single agent-quoted token so a whitespace-splitting
// parser cannot break the value= field apart. textsafe.Line alone leaves the
// space bare; agent quoting wraps the whole JSON value.
func TestMetaGetAgentQuotesValueWithSpaces(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "agent metadata spaces")

	runCLI(t, env, dir, "meta", "set", ref, "work.note", "hello world")

	getOut := runCLI(t, env, dir, "--agent", "meta", "get", ref)
	assert.Contains(t, getOut, "OK meta get")
	assert.Contains(t, getOut, "key=work.note")
	assert.Contains(t, getOut, `value="\"hello world\""`)
}

func TestMetaSetAndGetJSONOutput(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "json metadata issue")

	setOut := runCLI(t, env, dir, "--json", "meta", "set", ref, "work.attention", "ok")
	var setPayload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(setOut), &setPayload))
	assert.Contains(t, string(setPayload["issue"]), `"short_id":"`+ref+`"`)
	assert.Contains(t, string(setPayload["issue"]), `"revision":2`)

	getOut := runCLI(t, env, dir, "--json", "meta", "get", ref)
	assert.JSONEq(t, fmt.Sprintf(
		`{"kata_api_version":1,"ref":%q,"revision":2,"metadata":{"work.attention":"ok"}}`,
		ref), getOut)
}

func TestMetaUnsetIfMatchStaleRevisionConflictsAndCorrectRevisionSucceeds(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "if match unset metadata issue")

	runCLI(t, env, dir, "meta", "set", ref, "work.branch", "feature/example")

	_, stderr, err := runCLIWithErr(t, env, dir, "meta", "unset", "--if-match", "rev-99", ref, "work.branch")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Contains(t, stderr, "revision conflict")

	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	require.JSONEq(t, `{"work.branch":"feature/example"}`, string(issue.Issue.Metadata))

	out := runCLI(t, env, dir, "meta", "unset", "--if-match", "2", ref, "work.branch")
	assert.Contains(t, out, "unset")
	assert.Contains(t, out, "work.branch")
	assert.Contains(t, out, "rev-3")

	issue = fetchMetaIssueViaHTTP(t, env, pid, ref)
	assert.JSONEq(t, `{}`, string(issue.Issue.Metadata))
}

type metaIssueResponse struct {
	Issue struct {
		ShortID  string          `json:"short_id"`
		Metadata json.RawMessage `json:"metadata"`
		Revision int64           `json:"revision"`
	} `json:"issue"`
}

func fetchMetaIssueViaHTTP(t *testing.T, env *testenv.Env, pid int64, ref string) metaIssueResponse {
	t.Helper()
	issue := getJSON[metaIssueResponse](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues/"+ref)
	if len(strings.TrimSpace(string(issue.Issue.Metadata))) == 0 {
		issue.Issue.Metadata = json.RawMessage(`{}`)
	}
	return issue
}
