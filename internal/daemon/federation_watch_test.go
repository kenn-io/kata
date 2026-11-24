package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
)

// watchFixture is a hub with two federated projects, each with its own
// project-scoped pull enrollment, as one spoke bound to both holds.
type watchFixture struct {
	longPollFixture
	other           db.Project
	otherEnrollment db.CreatedFederationEnrollment
}

const watchOtherToken = "watch-other-token" //nolint:gosec // test-only bearer token

func newWatchFixture(t *testing.T) watchFixture {
	t.Helper()
	f := watchFixture{longPollFixture: newLongPollFixture(t)}
	f.other = createFederatedHubProject(t, f.env, "other-hub-project")
	var err error
	f.otherEnrollment, err = f.env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		Token:            watchOtherToken,
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &f.other.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)
	return f
}

type watchEntryForTest struct {
	ProjectID int64  `json:"project_id"`
	AfterID   int64  `json:"after_id"`
	Token     string `json:"token"`
}

type watchResultForTest struct {
	err     error
	status  int
	header  http.Header
	raw     []byte
	elapsed time.Duration
	body    struct {
		Ready     []int64 `json:"ready"`
		Unwatched []int64 `json:"unwatched"`
	}
}

// otherCursor reads the other project's current cursor with its own token.
func (f watchFixture) otherCursor(t *testing.T) int64 {
	t.Helper()
	resp, raw := envDoRaw(t, f.env, http.MethodGet, projectPath(f.other.ID)+"/federation/events?after_id=0", nil, bearer(watchOtherToken))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var body pollBodyForTest
	require.NoError(t, json.Unmarshal(raw, &body))
	return body.NextAfterID
}

// startWatch sends a watch led by the fixture's project from its own
// goroutine, and returns once the hub has finished reads reads of it.
func (f watchFixture) startWatch(t *testing.T, afterID int64, entries []watchEntryForTest, wait string, reads int) <-chan watchResultForTest {
	t.Helper()
	before := f.reads.readsReturned()
	done := make(chan watchResultForTest, 1)
	url := fmt.Sprintf("%s%s/federation/events:watch?wait=%s", f.env.URL, projectPath(f.project.ID), wait)
	body := map[string]any{"after_id": afterID, "projects": entries}
	go func() { done <- sendWatch(t.Context(), f.env.HTTP, url, body) }()
	f.reads.waitForReads(t, before+reads)
	return done
}

func (f watchFixture) watch(t *testing.T, afterID int64, entries []watchEntryForTest, wait string) watchResultForTest {
	t.Helper()
	url := fmt.Sprintf("%s%s/federation/events:watch?wait=%s", f.env.URL, projectPath(f.project.ID), wait)
	result := sendWatch(t.Context(), f.env.HTTP, url, map[string]any{"after_id": afterID, "projects": entries})
	require.NoError(t, result.err)
	return result
}

func sendWatch(ctx context.Context, client *http.Client, url string, body any) watchResultForTest {
	raw, err := json.Marshal(body)
	if err != nil {
		return watchResultForTest{err: err}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return watchResultForTest{err: err}
	}
	request.Header.Set("Authorization", "Bearer long-poll-token")
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := client.Do(request) //nolint:gosec // test-owned loopback daemon
	if err != nil {
		return watchResultForTest{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	out := watchResultForTest{status: response.StatusCode, header: response.Header}
	out.raw, out.err = io.ReadAll(response.Body)
	out.elapsed = time.Since(started)
	if out.err == nil && response.StatusCode == http.StatusOK {
		out.err = json.Unmarshal(out.raw, &out.body)
	}
	return out
}

func awaitWatch(t *testing.T, done <-chan watchResultForTest) watchResultForTest {
	t.Helper()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("the held watch did not return")
		return watchResultForTest{}
	}
}

func TestFederationPollAdvertisesWatch(t *testing.T) {
	f := newLongPollFixture(t)
	result := f.poll(t, 0, "")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Equal(t, "256", result.header.Get("X-Kata-Federation-Watch-Max"))
}

func TestFederationWatchAnswersWhenAnyWatchedProjectCommits(t *testing.T) {
	f := newWatchFixture(t)
	cursor, otherCursor := f.cursor(t), f.otherCursor(t)
	done := f.startWatch(t, cursor, []watchEntryForTest{{ProjectID: f.other.ID, AfterID: otherCursor, Token: watchOtherToken}}, "30", 2)
	committed := time.Now()
	createIssueAs(t, f.env, f.other.ID, "tester", "Wakes the watch")
	result := awaitWatch(t, done)
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Equal(t, []int64{f.other.ID}, result.body.Ready)
	assert.Empty(t, result.body.Unwatched)
	assert.Equal(t, "45", result.header.Get("X-Kata-Federation-Poll-Wait-Max"))
	assert.Less(t, time.Since(committed), 5*time.Second)
}

func TestFederationWatchAnswersAtOnceWhenAProjectHasEvents(t *testing.T) {
	f := newWatchFixture(t)
	cursor := f.cursor(t)
	createIssueAs(t, f.env, f.other.ID, "tester", "Already committed")
	result := f.watch(t, cursor, []watchEntryForTest{{ProjectID: f.other.ID, AfterID: 0, Token: watchOtherToken}}, "30")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Equal(t, []int64{f.other.ID}, result.body.Ready)
	assert.Less(t, result.elapsed, 5*time.Second)
}

func TestFederationWatchReturnsNothingReadyWhenTheWaitExpires(t *testing.T) {
	f := newWatchFixture(t)
	cursor, otherCursor := f.cursor(t), f.otherCursor(t)
	result := f.watch(t, cursor, []watchEntryForTest{{ProjectID: f.other.ID, AfterID: otherCursor, Token: watchOtherToken}}, "1")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Ready)
	assert.Empty(t, result.body.Unwatched)
	assert.GreaterOrEqual(t, result.elapsed, time.Second)
}

// Each project a watch names is authorized by the token sent with it. The
// route bearer and a token for another project grant nothing.
func TestFederationWatchChecksEachProjectAgainstItsOwnToken(t *testing.T) {
	f := newWatchFixture(t)
	third := createFederatedHubProject(t, f.env, "third-hub-project")
	cursor := f.cursor(t)
	createIssueAs(t, f.env, f.other.ID, "tester", "Pending in other")
	createIssueAs(t, f.env, third.ID, "tester", "Pending in third")
	result := f.watch(t, cursor, []watchEntryForTest{
		{ProjectID: f.other.ID, AfterID: 0, Token: "long-poll-token"},
		{ProjectID: third.ID, AfterID: 0, Token: watchOtherToken},
	}, "1")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Ready, "a project was reported without its own credential")
	assert.ElementsMatch(t, []int64{f.other.ID, third.ID}, result.body.Unwatched)
}

func TestFederationWatchReportsARevokedProjectAsUnwatched(t *testing.T) {
	f := newWatchFixture(t)
	cursor, otherCursor := f.cursor(t), f.otherCursor(t)
	done := f.startWatch(t, cursor, []watchEntryForTest{{ProjectID: f.other.ID, AfterID: otherCursor, Token: watchOtherToken}}, "2", 2)
	require.NoError(t, f.env.DB.RevokeFederationEnrollment(t.Context(), f.otherEnrollment.Enrollment.ID))
	createIssueAs(t, f.env, f.other.ID, "tester", "Committed after revocation")
	result := awaitWatch(t, done)
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Ready)
	assert.Equal(t, []int64{f.other.ID}, result.body.Unwatched)
}

func TestFederationWatchRejectsInvalidProjectLists(t *testing.T) {
	f := newWatchFixture(t)
	tooMany := make([]watchEntryForTest, 256)
	for i := range tooMany {
		tooMany[i] = watchEntryForTest{ProjectID: int64(1000 + i), Token: watchOtherToken}
	}
	for name, entries := range map[string][]watchEntryForTest{
		"route project":     {{ProjectID: f.project.ID, Token: watchOtherToken}},
		"duplicate project": {{ProjectID: f.other.ID, Token: watchOtherToken}, {ProjectID: f.other.ID, Token: watchOtherToken}},
		"negative cursor":   {{ProjectID: f.other.ID, AfterID: -1, Token: watchOtherToken}},
		"missing token":     {{ProjectID: f.other.ID}},
		"too many":          tooMany,
	} {
		t.Run(name, func(t *testing.T) {
			result := f.watch(t, 0, entries, "0")
			assert.Equal(t, http.StatusBadRequest, result.status, "%s", result.raw)
		})
	}
}

// A signed watch proves only the route credential's signing key, so the hub
// watches no other project for it, even with a valid token.
func TestSignedFederationWatchWatchesOnlyTheRouteProject(t *testing.T) {
	f := newSignedPollFixture(t)
	other, err := f.store.CreateProject(t.Context(), "other-hub-project")
	require.NoError(t, err)
	_, err = f.store.EnableProjectFederation(t.Context(), other.ID, "admin")
	require.NoError(t, err)
	_, err = f.store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		Token: watchOtherToken, SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &other.ID, Capabilities: "pull", Actor: "admin",
	})
	require.NoError(t, err)
	_, _, err = f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Pending in other", Author: "admin"})
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		h := f.serve(t, 0)
		cursor := h.cursor(t)
		raw, err := json.Marshal(map[string]any{
			"after_id": cursor,
			"projects": []watchEntryForTest{{ProjectID: other.ID, AfterID: 0, Token: watchOtherToken}},
		})
		require.NoError(t, err)
		url := fmt.Sprintf("https://hub.example%s/federation/events:watch?wait=0", projectPath(h.project.ID))
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+h.enrollment.Token)
		request.Header.Set("Content-Type", "application/json")
		require.NoError(t, federationsigning.Sign(request, h.source))
		response := h.send(request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var body watchResultForTest
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body.body))
		assert.Empty(t, body.body.Ready)
		assert.Equal(t, []int64{other.ID}, body.body.Unwatched)
	})
}
