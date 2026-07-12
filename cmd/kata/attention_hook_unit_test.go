package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAttnDaemon struct {
	lookups            map[string]attnLookup
	lookupSequence     []attnLookup
	lookupRefs         []string
	conditionalWrites  []conditionalMetaWrite
	conditionalResults []attnWriteResult
}

type conditionalMetaWrite struct {
	ref      string
	patch    map[string]string
	revision int64
}

func (f *fakeAttnDaemon) lookup(ref string) attnLookup {
	f.lookupRefs = append(f.lookupRefs, ref)
	if call := len(f.lookupRefs) - 1; call < len(f.lookupSequence) {
		return f.lookupSequence[call]
	}
	if lookup, ok := f.lookups[ref]; ok {
		return lookup
	}
	return attnLookup{kind: lookupGone}
}

func (f *fakeAttnDaemon) setMetaIfRevision(ref string, patch map[string]string, revision int64) attnWriteResult {
	call := len(f.conditionalWrites)
	f.conditionalWrites = append(f.conditionalWrites, conditionalMetaWrite{
		ref: ref, patch: patch, revision: revision,
	})
	if call < len(f.conditionalResults) {
		return f.conditionalResults[call]
	}
	return attnWriteApplied
}

func TestAttnStart_ConditionallySetsOnlyAttentionOKForOpenIssue(t *testing.T) {
	d := &fakeAttnDaemon{lookups: map[string]attnLookup{
		"abc4": {kind: lookupOpen, hasAttn: true, attention: attnValueNeedsHuman, revision: 13},
	}}

	attnStart(d, "  abc4  ")

	assert.Equal(t, []string{"abc4"}, d.lookupRefs)
	assert.Equal(t, []conditionalMetaWrite{{
		ref: "abc4", patch: map[string]string{attentionKey: attnValueOK}, revision: 13,
	}}, d.conditionalWrites)
}

func TestAttnStart_SkipsMissingAndTransientIssues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lookup attnLookup
	}{
		{name: "missing", lookup: attnLookup{kind: lookupGone}},
		{name: "transient", lookup: attnLookup{kind: lookupTransient}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeAttnDaemon{lookups: map[string]attnLookup{"abc4": tc.lookup}}

			attnStart(d, "abc4")

			assert.Equal(t, []string{"abc4"}, d.lookupRefs)
			assert.Empty(t, d.conditionalWrites)
		})
	}
}

func TestAttnStart_RetriesOnceAfterRevisionConflict(t *testing.T) {
	d := &fakeAttnDaemon{
		lookupSequence: []attnLookup{
			{kind: lookupOpen, revision: 13},
			{kind: lookupOpen, revision: 14},
		},
		conditionalResults: []attnWriteResult{attnWriteConflict},
	}

	attnStart(d, "abc4")

	assert.Equal(t, []string{"abc4", "abc4"}, d.lookupRefs)
	assert.Equal(t, []conditionalMetaWrite{
		{ref: "abc4", patch: map[string]string{attentionKey: attnValueOK}, revision: 13},
		{ref: "abc4", patch: map[string]string{attentionKey: attnValueOK}, revision: 14},
	}, d.conditionalWrites)
}

func TestAttnEnd_AtomicallySetsHandoffForOpenOKIssue(t *testing.T) {
	d := &fakeAttnDaemon{lookups: map[string]attnLookup{
		"abc4": {kind: lookupOpen, hasAttn: true, attention: attnValueOK, revision: 17},
	}}

	attnEnd(d, " abc4 ")

	assert.Equal(t, []string{"abc4"}, d.lookupRefs)
	require.Equal(t, []conditionalMetaWrite{{
		ref: "abc4",
		patch: map[string]string{
			attentionKey:    attnValueNeedsHuman,
			attentionMsgKey: attnHandoffMsg,
		},
		revision: 17,
	}}, d.conditionalWrites)
}

func TestAttnEnd_SkipsNonActionableIssues(t *testing.T) {
	for _, tc := range []struct {
		name   string
		lookup attnLookup
	}{
		{name: "missing", lookup: attnLookup{kind: lookupGone}},
		{name: "transient", lookup: attnLookup{kind: lookupTransient}},
		{name: "attention absent", lookup: attnLookup{kind: lookupOpen}},
		{name: "attention non-ok", lookup: attnLookup{kind: lookupOpen, hasAttn: true, attention: attnValueNeedsHuman}},
		{name: "attention empty", lookup: attnLookup{kind: lookupOpen, hasAttn: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeAttnDaemon{lookups: map[string]attnLookup{"abc4": tc.lookup}}

			attnEnd(d, "abc4")

			assert.Equal(t, []string{"abc4"}, d.lookupRefs)
			assert.Empty(t, d.conditionalWrites)
		})
	}
}

func TestAttnEnd_RechecksAttentionAfterRevisionConflict(t *testing.T) {
	d := &fakeAttnDaemon{
		lookupSequence: []attnLookup{
			{kind: lookupOpen, hasAttn: true, attention: attnValueOK, revision: 17},
			{kind: lookupOpen, hasAttn: true, attention: attnValueNeedsHuman, revision: 18},
		},
		conditionalResults: []attnWriteResult{attnWriteConflict},
	}

	attnEnd(d, "abc4")

	assert.Equal(t, []string{"abc4", "abc4"}, d.lookupRefs)
	assert.Len(t, d.conditionalWrites, 1)
}

func TestAttentionHooks_IgnoreEmptyAndDashLeadingRefs(t *testing.T) {
	for _, ref := range []string{"", "   ", "-abc4", "  --project  "} {
		t.Run(ref, func(t *testing.T) {
			d := &fakeAttnDaemon{}

			attnStart(d, ref)
			attnEnd(d, ref)

			assert.Empty(t, d.lookupRefs)
			assert.Empty(t, d.conditionalWrites)
		})
	}
}

func TestAttentionHookCommand_InvalidInvocationsExitZeroWithoutDaemonActivity(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	t.Setenv("KATA_REF", "abc4")

	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"-bogus"},
		{"start", "extra"},
		{"end", "extra"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			resetFlags(t)
			cmd := newRootCmd()
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(append([]string{"attention-hook"}, args...))
			cmd.SetContext(contextWithBaseURL(context.Background(), server.URL))

			assert.NoError(t, cmd.Execute())
			assert.Empty(t, output.String())
		})
	}
	assert.Zero(t, requests.Load())
}

func TestAttnEnd_WriteFailureDoesNotRetry(t *testing.T) {
	d := &fakeAttnDaemon{
		lookups: map[string]attnLookup{
			"abc4": {kind: lookupOpen, hasAttn: true, attention: attnValueOK, revision: 23},
		},
		conditionalResults: []attnWriteResult{attnWriteFailed},
	}

	attnEnd(d, "abc4")

	assert.Equal(t, []string{"abc4"}, d.lookupRefs)
	assert.Len(t, d.conditionalWrites, 1)
}

func TestLiveAttnDaemon_ConditionalSetSendsOnlyActorPatchAndIfMatch(t *testing.T) {
	resetFlags(t)
	requestSeen := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"project": map[string]any{"id": 42, "name": "example-project"},
			}))
		case "/api/v1/projects/42/issues/abc4/metadata":
			requestSeen = true
			require.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, `"rev-17"`, r.Header.Get("If-Match"))
			var body map[string]json.RawMessage
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.ElementsMatch(t, []string{"actor", "patch"}, mapKeys(body))
			assert.JSONEq(t, `"agent-a"`, string(body["actor"]))
			var patch map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body["patch"], &patch))
			assert.ElementsMatch(t, []string{attentionKey, attentionMsgKey}, mapKeys(patch))
			assert.JSONEq(t, `"needs-human"`, string(patch[attentionKey]))
			assert.JSONEq(t, `"session ended without hand-off"`, string(patch[attentionMsgKey]))
			_, _ = w.Write([]byte(`{"issue":{}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(context.Background(), server.URL))
	flags.Project = "example-project"
	flags.As = "agent-a"
	d := &liveAttnDaemon{cmd: cmd}
	result := d.setMetaIfRevision("abc4", map[string]string{
		attentionKey:    attnValueNeedsHuman,
		attentionMsgKey: attnHandoffMsg,
	}, 17)

	assert.Equal(t, attnWriteApplied, result)
	assert.True(t, requestSeen)
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
