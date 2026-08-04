package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		{"bad_rrule", `{"actor":"tester","rrule":"NOT-A-VALID-RRULE"}`},
		{"blank_title", `{"actor":"tester","template":{"title":"   "}}`},
		{"non_object_metadata", `{"actor":"tester","template":{"metadata":[1,2,3]}}`},
		// "metadata":null is intentionally NOT tested here: the patch
		// shape uses *json.RawMessage, and encoding/json decodes a JSON
		// null into a nil pointer — so the daemon treats it as "no
		// metadata patch supplied" and the validation branch never runs.
		// The create-side equivalent IS rejected (see the create test
		// table) because that field is a value-type json.RawMessage.
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
// for the *json.RawMessage Metadata field on RecurrenceTemplateUpdateInput.
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
