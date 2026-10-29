package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func seedRecurrence(t *testing.T, env *testenv.Env, projectID int64, rule, dtstart, tz, title string) db.Recurrence {
	t.Helper()
	rec, _, err := env.DB.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{
		ProjectID: projectID, Actor: "tester",
		Rule: rule, DTStart: dtstart, Timezone: tz,
		Template: db.RecurrenceTemplate{Title: title},
	})
	require.NoError(t, err)
	return rec
}

func TestPostRecurrence_HappyPath(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")

	body := `{
		"actor":"tester",
		"rrule":"FREQ=WEEKLY",
		"dtstart":"2026-05-15",
		"timezone":"America/New_York",
		"template":{"title":"Weekly review"}
	}`
	resp := doPost(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, p.ID), body)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusCreated, resp.StatusCode, "body: %s", raw)

	var out struct {
		Recurrence struct {
			UID string `json:"uid"`
		} `json:"recurrence"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Len(t, out.Recurrence.UID, 26)
}

func TestPostRecurrence_AttachesInitialIssueAndAdvancesOnClose(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Weekly review", Author: "user-a",
	})
	require.NoError(t, err)

	body := fmt.Sprintf(`{
		"actor":"user-a",
		"initial_issue_ref":%q,
		"rrule":"FREQ=WEEKLY",
		"dtstart":"2026-08-03",
		"timezone":"UTC",
		"template":{"title":"Weekly review"}
	}`, issue.ShortID)
	resp := doPost(t, env,
		fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, project.ID), body)
	raw := readClose(t, resp)
	require.Equalf(t, http.StatusCreated, resp.StatusCode, "body: %s", raw)

	attached, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotNil(t, attached.RecurrenceID)
	require.NotNil(t, attached.OccurrenceKey)
	assert.Equal(t, "2026-08-03", *attached.OccurrenceKey)

	_, events, changed, err := env.DB.CloseIssueWithEvents(
		t.Context(), issue.ID, "done", "user-a", "Completed the scheduled review and recorded its outcome.", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.GreaterOrEqual(t, len(events), 3)
	assert.Equal(t, "issue.created", events[len(events)-2].Type)
	assert.Equal(t, "recurrence.materialized", events[len(events)-1].Type)
	created, err := env.DB.IssueByID(t.Context(), *events[len(events)-2].IssueID)
	require.NoError(t, err)
	require.NotNil(t, created.OccurrenceKey)
	assert.Equal(t, "2026-08-10", *created.OccurrenceKey)
}

func TestPostRecurrence_InitialIssueRequiresFederatedClaim(t *testing.T) {
	for _, tc := range []struct {
		name        string
		claimHolder string
		wantStatus  int
	}{
		{name: "other actor", claimHolder: "user-b", wantStatus: http.StatusConflict},
		{name: "current actor", claimHolder: "user-a", wantStatus: http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			project := createFederatedHubProject(t, env, "example-project")
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: project.ID, Title: "Weekly review", Author: "user-a",
			})
			require.NoError(t, err)
			_, err = env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
				ProjectID: project.ID,
				IssueRef:  issue.ShortID,
				Principal: db.ClaimPrincipal{
					HolderInstanceUID: env.DB.InstanceUID(),
					Holder:            tc.claimHolder,
				},
				ClaimKind: "hard",
				Purpose:   "edit recurring task",
				Now:       time.Now().UTC(),
			})
			require.NoError(t, err)

			body := fmt.Sprintf(`{
				"actor":"user-a",
				"initial_issue_ref":%q,
				"rrule":"FREQ=WEEKLY",
				"dtstart":"2026-08-03",
				"timezone":"UTC",
				"template":{"title":"Weekly review"}
			}`, issue.ShortID)
			resp := doPost(t, env,
				fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, project.ID), body)
			raw := readClose(t, resp)
			if tc.wantStatus == http.StatusConflict {
				assertAPIError(t, resp.StatusCode, raw, tc.wantStatus, "claim_denied")
			} else {
				assert.Equal(t, tc.wantStatus, resp.StatusCode, string(raw))
			}
		})
	}
}

func TestPatchRecurrence_RequiresIfMatch(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "old")

	// No If-Match header — handler must reject with 400.
	resp := doPatch(t, env,
		fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID),
		`{"actor":"tester","template":{"title":"new"}}`, "")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPatchRecurrence_HappyPathReturnsNewETag(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "old")

	resp := doPatch(t, env,
		fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID),
		`{"actor":"tester","template":{"title":"new"}}`, `"rev-1"`)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, `"rev-2"`, resp.Header.Get("ETag"))
}

func TestPatchRecurrence_ClearsTemplateOwnerAndPriority(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	owner := "user-a"
	priority := int64(2)
	recurrence, _, err := env.DB.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{
		ProjectID: project.ID, Actor: "tester",
		Rule: "FREQ=WEEKLY", DTStart: "2026-05-15", Timezone: "UTC",
		Template: db.RecurrenceTemplate{
			Title: "Weekly review", Owner: &owner, Priority: &priority,
		},
	})
	require.NoError(t, err)

	response := doPatch(t, env,
		fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, project.ID, recurrence.UID),
		`{"actor":"tester","template":{"clear_owner":true,"clear_priority":true}}`, `"rev-1"`)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)

	updated, err := env.DB.GetRecurrenceByID(t.Context(), recurrence.ID)
	require.NoError(t, err)
	assert.Nil(t, updated.TemplateOwner)
	assert.Nil(t, updated.TemplatePriority)
}

func TestDeleteRecurrence_SoftDeletes(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "x")

	req, err := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s?actor=tester", env.URL, p.ID, rec.UID), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("If-Match", `"rev-1"`)

	resp, err := env.HTTP.Do(req) //nolint:gosec // G704: test server URL, not user-controlled
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Listing should be empty now.
	list, err := env.DB.ListRecurrencesByProject(t.Context(), p.ID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestDeleteRecurrence_StaleIfMatchReturns412(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	recurrence := seedRecurrence(t, env, project.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "old")
	recurrenceURL := fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, project.ID, recurrence.UID)

	patched := doPatch(t, env, recurrenceURL,
		`{"actor":"user-a","template":{"title":"new"}}`, `"rev-1"`)
	require.Equal(t, http.StatusOK, patched.StatusCode)
	require.NoError(t, patched.Body.Close())

	response := doReqEnv(t, env, http.MethodDelete, recurrenceURL+"?actor=user-a", "", `"rev-1"`)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusPreconditionFailed, response.StatusCode)

	active, err := env.DB.GetRecurrenceByID(t.Context(), recurrence.ID)
	require.NoError(t, err)
	assert.Nil(t, active.DeletedAt)
	assert.Equal(t, int64(2), active.Revision)
}

func TestShowRecurrence_AfterDeleteReturns404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "p")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "x")
	recURL := fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID)

	// Delete the recurrence.
	resp := doDeleteWithIfMatch(t, env, recURL+"?actor=tester", `"rev-1"`)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// GET after soft-delete must return 404.
	resp2 := doGet(t, env, recURL)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
}

func TestPatchRecurrence_AfterDeleteReturns404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "p")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "x")
	recURL := fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID)

	// Delete the recurrence.
	resp := doDeleteWithIfMatch(t, env, recURL+"?actor=tester", `"rev-1"`)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// PATCH after soft-delete must return 404, not 500.
	resp2 := doPatch(t, env, recURL, `{"actor":"tester","template":{"title":"new"}}`, `"rev-2"`)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
}

func TestDeleteRecurrence_AfterDeleteReturns404(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "p")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "x")
	recURL := fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID)

	// First delete.
	resp := doDeleteWithIfMatch(t, env, recURL+"?actor=tester", `"rev-1"`)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Second delete must return 404, not 500.
	resp2 := doDeleteWithIfMatch(t, env, recURL+"?actor=tester", `"rev-1"`)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp2.StatusCode)
}

func TestListRecurrences_ByProject(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")
	_ = seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "a")
	_ = seedRecurrence(t, env, p.ID, "FREQ=MONTHLY", "2026-05-01", "UTC", "b")

	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, p.ID), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")

	resp, err := env.HTTP.Do(req) //nolint:gosec // G704: test server URL, not user-controlled
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Recurrences []db.Recurrence `json:"recurrences"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Len(t, out.Recurrences, 2)
}

func TestCreateRecurrence_InvalidLabelReturns400(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")

	body := `{
		"actor":"tester",
		"rrule":"FREQ=WEEKLY",
		"dtstart":"2026-05-15",
		"timezone":"UTC",
		"template":{"title":"t","labels":["hello world"]}
	}`
	resp := doPost(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, p.ID), body)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPatchRecurrence_InvalidLabelReturns400(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "t")

	body := `{"actor":"tester","template":{"labels":["hello world"]}}`
	resp := doPatch(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID),
		body, `"rev-1"`)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestCreateRecurrence_InvalidInputsReturn400 covers the
// ErrInvalidRecurrence → 400 mapping on POST. Without these mappings each
// case would surface as a 500 — only ErrLabelInvalid was previously bridged
// to 400.
func TestCreateRecurrence_InvalidInputsReturn400(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad_timezone", `{"actor":"tester","rrule":"FREQ=WEEKLY","dtstart":"2026-05-15","timezone":"Not/AZone","template":{"title":"t"}}`},
		{"lowercase_local", `{"actor":"tester","rrule":"FREQ=WEEKLY","dtstart":"2026-05-15","timezone":"local","template":{"title":"t"}}`},
		{"padded_local", `{"actor":"tester","rrule":"FREQ=WEEKLY","dtstart":"2026-05-15","timezone":" Local ","template":{"title":"t"}}`},
		{"bad_rrule", `{"actor":"tester","rrule":"NOT-A-VALID-RRULE","dtstart":"2026-05-15","timezone":"UTC","template":{"title":"t"}}`},
		{"blank_title", `{"actor":"tester","rrule":"FREQ=WEEKLY","dtstart":"2026-05-15","timezone":"UTC","template":{"title":"   "}}`},
		{"null_metadata", `{"actor":"tester","rrule":"FREQ=WEEKLY","dtstart":"2026-05-15","timezone":"UTC","template":{"title":"t","metadata":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			p := seedProject(t, env, "src")
			resp := doPost(t, env,
				fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, p.ID), tc.body)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestPatchRecurrence_InvalidInputsReturn400 covers the patch-side
// ErrInvalidRecurrence → 400 mapping: the effective (rrule, dtstart, tz)
// triple plus template invariants (non-blank title, object metadata) are
// validated before write so unparseable state can't persist and explode at
// materialization time.
func TestPatchRecurrence_InvalidInputsReturn400(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"bad_timezone", `{"actor":"tester","timezone":"Not/AZone"}`},
		{"lowercase_local", `{"actor":"tester","timezone":"local"}`},
		{"padded_local", `{"actor":"tester","timezone":" Local "}`},
		{"bad_rrule", `{"actor":"tester","rrule":"NOT-A-VALID-RRULE"}`},
		{"blank_title", `{"actor":"tester","template":{"title":"   "}}`},
		{"non_object_metadata", `{"actor":"tester","template":{"metadata":[1,2,3]}}`},
		// "metadata":null is intentionally NOT tested here: the patch
		// shape uses *jsontext.Value, and encoding/json decodes a JSON
		// null into a nil pointer — so the daemon treats it as "no
		// metadata patch supplied" and the validation branch never runs.
		// The create-side equivalent IS rejected (see the create test
		// table) because that field is a value-type jsontext.Value.
		// TestPatchRecurrence_NullMetadata_NoOp pins the no-op contract.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			p := seedProject(t, env, "src")
			rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "t")
			resp := doPatch(t, env,
				fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID),
				tc.body, `"rev-1"`)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestPatchRecurrence_NullMetadata_NoOp pins the null-as-absent semantics
// for the *jsontext.Value Metadata field on RecurrenceTemplateUpdateInput.
// Sending `{"template":{"metadata":null}}` decodes the pointer to nil, which
// the handler treats identically to omitting the field — the row is not
// touched and the revision is not bumped. Codified so a future tri-state
// refactor cannot silently flip the meaning to "clear the metadata blob".
func TestPatchRecurrence_NullMetadata_NoOp(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "src")
	seeded := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-05-15", "UTC", "t")

	resp := doPatch(t, env,
		fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, seeded.UID),
		`{"actor":"tester","template":{"metadata":null}}`, `"rev-1"`)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
	assert.Equal(t, `"rev-1"`, resp.Header.Get("ETag"))
	assert.Contains(t, string(raw), `"changed":false`)

	got, err := env.DB.GetRecurrenceByUID(t.Context(), seeded.UID)
	require.NoError(t, err)
	assert.Equal(t, seeded.Revision, got.Revision)
	assert.Equal(t, seeded.TemplateMetadata, got.TemplateMetadata)
}

// The admission contract forbids introducing Local, without mutating any state.
func TestPostRecurrenceRejectsLocal(t *testing.T) {
	for _, attach := range []bool{false, true} {
		t.Run(fmt.Sprintf("attach=%t", attach), func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			p := seedProject(t, env, "example-project")
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: p.ID, Title: "Weekly review", Author: "tester",
			})
			require.NoError(t, err)
			ref := ""
			if attach {
				ref = issue.ShortID
			}
			before := recurrenceWriteCursor(t, env)
			body := fmt.Sprintf(`{"actor":"tester","initial_issue_ref":%q,"rrule":"FREQ=WEEKLY","dtstart":"2026-08-03","timezone":"Local","template":{"title":"Weekly review"}}`, ref)
			resp := doPost(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, p.ID), body)
			assertLocalTimezoneError(t, resp.StatusCode, readClose(t, resp))
			recs, err := env.DB.ListRecurrencesByProject(t.Context(), p.ID)
			require.NoError(t, err)
			assert.Empty(t, recs)
			afterIssue, err := env.DB.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			assert.Equal(t, issue, afterIssue)
			assert.Equal(t, before, recurrenceWriteCursor(t, env))
		})
	}
}

func TestPatchRecurrenceRejectsLocalTimezoneChange(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "example-project")
	rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-08-03", "UTC", "Weekly review")
	before := recurrenceWriteCursor(t, env)
	resp := doPatch(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID),
		`{"actor":"tester","timezone":"Local","rrule":"FREQ=DAILY","template":{"title":"Changed"}}`, `"rev-1"`)
	assertLocalTimezoneError(t, resp.StatusCode, readClose(t, resp))
	after, err := env.DB.GetRecurrenceByUID(t.Context(), rec.UID)
	require.NoError(t, err)
	assert.Equal(t, rec, after)
	assert.Equal(t, before, recurrenceWriteCursor(t, env))
}

func TestPatchRecurrenceLocalStaleRevision(t *testing.T) {
	for _, originalZone := range []string{"UTC", "Local"} {
		t.Run(originalZone, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			p := seedProject(t, env, "example-project")
			rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-08-03", originalZone, "Weekly review")
			url := fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID)
			// Another writer switches away from legacy Local (or updates UTC).
			resp := doPatch(t, env, url, `{"actor":"tester","timezone":"UTC","template":{"title":"Updated"}}`, `"rev-1"`)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(readClose(t, resp)))
			current, err := env.DB.GetRecurrenceByUID(t.Context(), rec.UID)
			require.NoError(t, err)
			before := recurrenceWriteCursor(t, env)
			for _, revision := range []int64{math.MinInt64, -1, 0, 1, 3, math.MaxInt64} {
				resp = doPatch(t, env, url, `{"actor":"tester","timezone":"Local"}`, fmt.Sprintf(`"rev-%d"`, revision))
				assertAPIError(t, resp.StatusCode, readClose(t, resp), http.StatusPreconditionFailed, "revision_conflict")
			}
			after, err := env.DB.GetRecurrenceByUID(t.Context(), rec.UID)
			require.NoError(t, err)
			assert.Equal(t, current, after)
			assert.Equal(t, before, recurrenceWriteCursor(t, env))
		})
	}
}

// Track both the event log and browser snapshot cursor, so rejected writes
// cannot silently invalidate snapshots or leave an event behind.
func recurrenceWriteCursor(t require.TestingT, env *testenv.Env) [2]int64 {
	event, err := env.DB.MaxEventID(context.Background())
	require.NoError(t, err)
	cursor, err := env.DB.UIEventCursor(context.Background())
	require.NoError(t, err)
	return [2]int64{event, cursor}
}

func assertLocalTimezoneError(t require.TestingT, status int, raw []byte) {
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	var out struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, "validation", out.Error.Code)
	for _, word := range []string{"timezone", "Local", "host", "IANA"} {
		require.Contains(t, out.Error.Message, word)
	}
}

// Generate valid schedules and nonblank Unicode titles. The date/interval
// subset keeps recurrence evaluation cheap; text is capped at materialization.
func drawRecurrenceFields(ht *hegel.T) (rule, date, title string) {
	date = hegel.Draw(ht, hegel.Dates().
		Min(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).
		Max(time.Date(2100, 12, 31, 0, 0, 0, 0, time.UTC))).Format("2006-01-02")
	frequency := hegel.Draw(ht, hegel.SampledFrom([]string{"DAILY", "WEEKLY", "MONTHLY", "YEARLY"}))
	interval := hegel.Draw(ht, hegel.Integers(1, 30))
	rule = fmt.Sprintf("FREQ=%s;INTERVAL=%d", frequency, interval)
	text := []rune(hegel.Draw(ht, hegel.Text()))
	if len(text) > 128 {
		text = text[:128]
	}
	return rule, date, "Review " + string(text)
}

func TestPatchRecurrenceLocalCompatibilityProperty(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "example-project")
	hegel.Test(t, func(ht *hegel.T) {
		rule, date, title := drawRecurrenceFields(ht)
		for _, echo := range []bool{false, true} {
			rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "1999-01-01", "Local", "Old title")
			body := map[string]any{"actor": "tester", "rrule": rule, "dtstart": date, "template": map[string]any{"title": title}}
			if echo {
				body["timezone"] = "Local"
			}
			raw, err := json.Marshal(body)
			require.NoError(ht, err)
			url := fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID)
			resp := doPatch(t, env, url, string(raw), `"rev-1"`)
			require.Equal(ht, http.StatusOK, resp.StatusCode, string(readClose(t, resp)))
			require.Equal(ht, `"rev-2"`, resp.Header.Get("ETag"))
			// Read via HTTP too: historical values remain visible unchanged.
			resp = doGet(t, env, url)
			var out struct {
				Recurrence db.Recurrence `json:"recurrence"`
			}
			require.Equal(ht, http.StatusOK, resp.StatusCode)
			require.NoError(ht, json.Unmarshal(readClose(t, resp), &out))
			require.Equal(ht, "Local", out.Recurrence.Timezone)
			require.Equal(ht, rule, out.Recurrence.RRule)
			require.Equal(ht, date, out.Recurrence.DTStart)
			require.Equal(ht, title, out.Recurrence.TemplateTitle)
			require.Equal(ht, int64(2), out.Recurrence.Revision)
			require.Equal(ht, &date, out.Recurrence.NextOccurrenceKey)
		}
	}, hegel.WithTestCases(24), hegel.WithDatabase(t.TempDir()))
}

func TestPostRecurrenceAcceptedTimezoneProperty(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "example-project")
	hegel.Test(t, func(ht *hegel.T) {
		rule, date, title := drawRecurrenceFields(ht)
		// Enumerate the supported value classes; generate their schedules.
		for _, zone := range []string{"UTC", "America/New_York", "Europe/London", "Asia/Kathmandu", "US/Eastern", "Etc/GMT+5", ""} {
			body, err := json.Marshal(map[string]any{"actor": "tester", "rrule": rule, "dtstart": date, "timezone": zone, "template": map[string]any{"title": title}})
			require.NoError(ht, err)
			resp := doPost(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, p.ID), string(body))
			var out struct {
				Recurrence db.Recurrence `json:"recurrence"`
			}
			raw := readClose(t, resp)
			require.Equal(ht, http.StatusCreated, resp.StatusCode, string(raw))
			require.NoError(ht, json.Unmarshal(raw, &out))
			require.Equal(ht, zone, out.Recurrence.Timezone)
			require.Equal(ht, rule, out.Recurrence.RRule)
			require.Equal(ht, date, out.Recurrence.DTStart)
			require.Equal(ht, title, out.Recurrence.TemplateTitle)
			require.Equal(ht, &date, out.Recurrence.NextOccurrenceKey)
		}
	}, hegel.WithTestCases(24), hegel.WithDatabase(t.TempDir()))
}

func TestPatchRecurrenceLocalAdvancement(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "example-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Weekly review", Author: "tester"})
	require.NoError(t, err)
	legacy, err := env.DB.CreateRecurrenceForIssue(t.Context(), db.CreateRecurrenceForIssueIn{
		IssueID:    issue.ID,
		Recurrence: db.CreateRecurrenceIn{ProjectID: p.ID, Actor: "tester", Rule: "FREQ=WEEKLY", DTStart: "2026-08-03", Timezone: "Local", Template: db.RecurrenceTemplate{Title: "Weekly review"}},
	})
	require.NoError(t, err)
	// A schedule edit must still recompute from the last materialized issue.
	resp := doPatch(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, legacy.Recurrence.UID),
		`{"actor":"tester","rrule":"FREQ=DAILY;INTERVAL=2","timezone":"Local"}`, fmt.Sprintf(`"rev-%d"`, legacy.Recurrence.Revision))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(readClose(t, resp)))
	current, err := env.DB.GetRecurrenceByUID(t.Context(), legacy.Recurrence.UID)
	require.NoError(t, err)
	require.NotNil(t, current.NextOccurrenceKey)
	assert.Equal(t, "2026-08-05", *current.NextOccurrenceKey)
	resp = doPost(t, env, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, p.ID, issue.ShortID),
		`{"actor":"tester","reason":"done","message":"Completed the scheduled review and recorded its outcome.","evidence":[{"type":"reviewed-paths","paths":["review.md"]}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(readClose(t, resp)))
	current, err = env.DB.GetRecurrenceByUID(t.Context(), legacy.Recurrence.UID)
	require.NoError(t, err)
	require.NotNil(t, current.LastMaterializedUID)
	next, err := env.DB.IssueByUID(t.Context(), *current.LastMaterializedUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NotNil(t, next.OccurrenceKey)
	assert.Equal(t, "2026-08-05", *next.OccurrenceKey)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(next.Metadata), &metadata))
	assert.Equal(t, "Local", metadata["timezone"])
	assert.Equal(t, "2026-08-05", metadata["scheduled_on"])
	require.NotNil(t, current.NextOccurrenceKey)
	assert.Equal(t, "2026-08-07", *current.NextOccurrenceKey)
}

func TestPatchRecurrenceLocalSwitchAwayAndBackProperty(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "example-project")
	hegel.Test(t, func(ht *hegel.T) {
		rule, date, title := drawRecurrenceFields(ht)
		for _, zone := range []string{"UTC", "America/New_York", "Europe/London", "Asia/Kathmandu", "US/Eastern", "Etc/GMT+5", ""} {
			rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "1999-01-01", "Local", "Old title")
			body, err := json.Marshal(map[string]any{"actor": "tester", "rrule": rule, "dtstart": date, "timezone": zone, "template": map[string]any{"title": title}})
			require.NoError(ht, err)
			resp := doPatch(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID), string(body), `"rev-1"`)
			var out struct {
				Recurrence db.Recurrence `json:"recurrence"`
			}
			raw := readClose(t, resp)
			require.Equal(ht, http.StatusOK, resp.StatusCode, string(raw))
			require.NoError(ht, json.Unmarshal(raw, &out))
			require.Equal(ht, zone, out.Recurrence.Timezone)
			require.Equal(ht, rule, out.Recurrence.RRule)
			require.Equal(ht, date, out.Recurrence.DTStart)
			require.Equal(ht, title, out.Recurrence.TemplateTitle)
			require.Equal(ht, &date, out.Recurrence.NextOccurrenceKey)
			before := recurrenceWriteCursor(ht, env)
			resp = doPatch(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID),
				`{"actor":"tester","timezone":"Local"}`, fmt.Sprintf(`"rev-%d"`, out.Recurrence.Revision))
			assertLocalTimezoneError(ht, resp.StatusCode, readClose(t, resp))
			after, err := env.DB.GetRecurrenceByUID(t.Context(), rec.UID)
			require.NoError(ht, err)
			require.Equal(ht, out.Recurrence, after)
			require.Equal(ht, before, recurrenceWriteCursor(ht, env))
		}
	}, hegel.WithTestCases(24), hegel.WithDatabase(t.TempDir()))
}

func TestPostRecurrenceRejectsLocalBeforeClaimRefresh(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project, issue := createShowClaimSpokeProject(t, env)
	now := time.Now().UTC()
	cachedAt := now.Add(-time.Minute)
	err := env.DB.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			Holder:            "cached-holder",
			ClientKind:        "cli",
		},
		Claim:  showIssueCachedClaim(issue, "cached-holder", cachedAt),
		HubNow: cachedAt,
	})
	require.NoError(t, err)
	var calls int
	hubNow := now.Add(time.Second)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "Bearer claim-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/v1/projects/42/issues/"+issue.ShortID+"/lease", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimStatusBody{
			Held: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Holder:            "hub-holder",
				ClientKind:        "cli",
			},
			Claim:  showIssueClaimOut(issue, "hub-holder", hubNow),
			HubNow: hubNow,
		}))
	}))
	t.Cleanup(hub.Close)
	writeShowClaimSpokeBinding(t, env, project, hub.URL, "claim-token", "claim")
	binding, err := env.DB.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	binding.PushEnabled = true
	binding.Actor = "tester"
	_, err = env.DB.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	beforeClaim, err := env.DB.ClaimStatusReadOnly(t.Context(), project.ID, issue.ShortID, now)
	require.NoError(t, err)
	before := recurrenceWriteCursor(t, env)
	body := fmt.Sprintf(`{"actor":"tester","initial_issue_ref":%q,"rrule":"FREQ=WEEKLY","dtstart":"2026-08-03","timezone":"Local","template":{"title":"Weekly review"}}`, issue.ShortID)
	resp := doPost(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences", env.URL, project.ID), body)
	raw := readClose(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	if resp.StatusCode == http.StatusBadRequest {
		assertLocalTimezoneError(t, resp.StatusCode, raw)
	}
	afterClaim, err := env.DB.ClaimStatusReadOnly(t.Context(), project.ID, issue.ShortID, now)
	require.NoError(t, err)
	assert.Equal(t, 0, calls)
	assert.Equal(t, beforeClaim, afterClaim)
	afterIssue, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, issue, afterIssue)
	recs, err := env.DB.ListRecurrencesByProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.Empty(t, recs)
	assert.Equal(t, before, recurrenceWriteCursor(t, env))
}

// Return a snapshot taken before another writer commits. This deterministically
// places that commit between the handler's read and storage's revision check.
type recurrenceReadInterleaveStorage struct {
	db.Storage
	once      sync.Once
	afterRead func(context.Context, db.Recurrence) error
}

func (s *recurrenceReadInterleaveStorage) GetRecurrenceByUID(ctx context.Context, uid string) (db.Recurrence, error) {
	rec, err := s.Storage.GetRecurrenceByUID(ctx, uid)
	if err == nil {
		s.once.Do(func() { err = s.afterRead(ctx, rec) })
	}
	return rec, err
}

func TestPatchRecurrenceLocalFutureRevisionInterleaving(t *testing.T) {
	for _, originalZone := range []string{"UTC", "Local"} {
		t.Run(originalZone, func(t *testing.T) {
			var storage *recurrenceReadInterleaveStorage
			env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) {
				storage = &recurrenceReadInterleaveStorage{Storage: cfg.DB}
				cfg.DB = storage
			})
			p := seedProject(t, env, "example-project")
			rec := seedRecurrence(t, env, p.ID, "FREQ=WEEKLY", "2026-08-03", originalZone, "Weekly review")
			type committedState struct {
				rec    db.Recurrence
				cursor [2]int64
			}
			committed := make(chan committedState, 1)
			storage.afterRead = func(ctx context.Context, read db.Recurrence) error {
				zone, title := "UTC", "Concurrent edit"
				result, err := storage.PatchRecurrence(ctx, db.PatchRecurrenceIn{
					RecurrenceID: read.ID, IfMatchRev: read.Revision, Actor: "other-writer",
					Update: db.RecurrenceUpdate{Timezone: &zone, TemplateTitle: &title},
				})
				if err != nil {
					return err
				}
				event, err := env.DB.MaxEventID(ctx)
				if err != nil {
					return err
				}
				cursor, err := env.DB.UIEventCursor(ctx)
				if err != nil {
					return err
				}
				committed <- committedState{result.Recurrence, [2]int64{event, cursor}}
				return nil
			}
			resp := doPatch(t, env, fmt.Sprintf("%s/api/v1/projects/%d/recurrences/%s", env.URL, p.ID, rec.UID),
				`{"actor":"tester","timezone":"Local"}`, `"rev-2"`)
			assertAPIError(t, resp.StatusCode, readClose(t, resp), http.StatusPreconditionFailed, "revision_conflict")
			want := <-committed
			after, err := env.DB.GetRecurrenceByUID(t.Context(), rec.UID)
			require.NoError(t, err)
			assert.Equal(t, "UTC", after.Timezone)
			assert.Equal(t, int64(2), after.Revision)
			assert.Equal(t, want.rec, after)
			assert.Equal(t, want.cursor, recurrenceWriteCursor(t, env))
		})
	}
}
