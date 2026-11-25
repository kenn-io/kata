package federation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// quietSpokeProjects is how many projects one spoke binds at one hub in the
// benchmark below.
const quietSpokeProjects = 50

// quietTestProjects is how many projects the quiet-spoke test binds. A hub on
// one CPU holds at most 32 polls, so a poll per project stays within that.
const quietTestProjects = 32

// quietBinding is one hub project a spoke binds, with its pull token and the
// hub's current cursor for it.
type quietBinding struct {
	hubProjectID int64
	token        string
	cursor       int64
}

// newQuietSpoke binds one spoke to projects projects on one hub, all through
// the harness's address.
func newQuietSpoke(tb testing.TB, watches bool, projects int) (*hubSpokeHarness, []quietBinding) {
	tb.Helper()
	h := newHubSpokeHarness(tb, false)
	h.watches.Store(watches)
	bindings := []quietBinding{{hubProjectID: h.hubProject.ID, token: h.pullToken}}
	for i := 1; i < projects; i++ {
		name := fmt.Sprintf("quiet-project-%02d", i)
		_, hub := h.addBinding(tb, name, h.url)
		bindings = append(bindings, quietBinding{hubProjectID: hub.ID, token: name + "-pull-token"})
	}
	for i := range bindings {
		var body struct {
			NextAfterID int64 `json:"next_after_id"`
		}
		status := quietHubCall(tb, http.MethodGet, h.hubURL+"/api/v1/projects/"+strconv.FormatInt(bindings[i].hubProjectID, 10)+
			"/federation/events?after_id=0", bindings[i].token, nil, &body)
		require.Equal(tb, http.StatusOK, status)
		bindings[i].cursor = body.NextAfterID
	}
	return h, bindings
}

func quietHubCall(tb testing.TB, method, url, token string, body, out any) int {
	tb.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(tb, err)
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(tb.Context(), method, url, reader)
	require.NoError(tb, err)
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback hub
	require.NoError(tb, err)
	defer func() { _ = response.Body.Close() }()
	// Read the whole body so the client reuses its connection.
	raw, err := io.ReadAll(response.Body)
	require.NoError(tb, err)
	if out != nil && response.StatusCode == http.StatusOK {
		require.NoError(tb, json.Unmarshal(raw, out))
	}
	return response.StatusCode
}

func (f *hubFront) peakHeldRequests() int {
	f.heldMu.Lock()
	defer f.heldMu.Unlock()
	return f.peakHeld
}

func (f *hubFront) resetPeakHeld() {
	f.heldMu.Lock()
	defer f.heldMu.Unlock()
	f.peakHeld = f.held
}

// An idle spoke bound to many projects at one hub keeps one request open
// there when the hub takes watches, instead of one per project. The hub holds
// each request for one second, so the request count over the window is the
// count per hold.
func TestQuietSpokeHoldsOneRequestAtAHubThatTakesWatches(t *testing.T) {
	const window = 3 * time.Second
	measure := func(t *testing.T, watches bool) (peak int, requests, watchRequests int64) {
		h, _ := newQuietSpoke(t, watches, quietTestProjects)
		h.holdSeconds.Store(1)
		turns := make(chan int64, 2*quietTestProjects)
		h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
		for range quietTestProjects {
			select {
			case <-turns:
			case <-time.After(30 * time.Second):
				t.Fatal("the first runner pass did not finish")
			}
		}
		//nolint:kennlint // sleeptest: the watchers reach the hub over real HTTP and settle into their steady hold.
		time.Sleep(2 * time.Second)
		h.resetPeakHeld()
		before, watchesBefore := h.requests.Load(), h.watchRequests.Load()
		//nolint:kennlint // sleeptest: measures the requests a quiet spoke sends over real HTTP in a fixed window.
		time.Sleep(window)
		return h.peakHeldRequests(), h.requests.Load() - before, h.watchRequests.Load() - watchesBefore
	}
	var pollPeak, watchPeak int
	var pollRequests, watchRequests, watchesSent int64
	t.Run("held poll per project", func(t *testing.T) { pollPeak, pollRequests, _ = measure(t, false) })
	t.Run("one watch", func(t *testing.T) { watchPeak, watchRequests, watchesSent = measure(t, true) })
	t.Logf("%d projects over %s with a 1s hold: held poll per project: %d open at once, %d requests; one watch: %d open at once, %d requests",
		quietTestProjects, window, pollPeak, pollRequests, watchPeak, watchRequests)
	require.Equal(t, quietTestProjects, pollPeak, "without watches the spoke holds a poll per project")
	require.Positive(t, watchesSent, "the spoke keeps watching while its projects are quiet")
	require.GreaterOrEqual(t, watchPeak, 1, "the spoke keeps a watch open while its projects are quiet")
	require.LessOrEqual(t, watchPeak, 2, "a reissued watch may overlap the one it replaces, but never more")
	require.Less(t, watchRequests*10, pollRequests, "one watch sends far fewer requests than a poll per project")
}

// BenchmarkQuietRoundAtHub times one round of asking the hub whether any of
// 50 quiet projects has work, once through a poll per project and once through
// one watch. Neither asks the hub to wait, so the time is the round trip over
// loopback: the hub's work plus the client's.
func BenchmarkQuietRoundAtHub(b *testing.B) {
	h, bindings := newQuietSpoke(b, true, quietSpokeProjects)
	project := func(binding quietBinding) string {
		return h.hubURL + "/api/v1/projects/" + strconv.FormatInt(binding.hubProjectID, 10)
	}
	b.Run("poll per project", func(b *testing.B) {
		for b.Loop() {
			for _, binding := range bindings {
				var page struct {
					Events        []json.RawMessage `json:"events"`
					ResetRequired bool              `json:"reset_required"`
				}
				status := quietHubCall(b, http.MethodGet, project(binding)+"/federation/events?limit=1&after_id="+strconv.FormatInt(binding.cursor, 10), binding.token, nil, &page)
				require.Equal(b, http.StatusOK, status)
				require.Empty(b, page.Events)
				require.False(b, page.ResetRequired)
			}
		}
		b.ReportMetric(float64(len(bindings)), "requests/op")
	})
	b.Run("one watch", func(b *testing.B) {
		lead, others := bindings[0], make([]map[string]any, 0, len(bindings)-1)
		for _, binding := range bindings[1:] {
			others = append(others, map[string]any{"project_id": binding.hubProjectID, "after_id": binding.cursor, "token": binding.token})
		}
		body := map[string]any{"after_id": lead.cursor, "projects": others}
		for b.Loop() {
			var answer struct {
				Ready     []int64 `json:"ready"`
				Unwatched []int64 `json:"unwatched"`
			}
			status := quietHubCall(b, http.MethodPost, project(lead)+"/federation/events:watch", lead.token, body, &answer)
			require.Equal(b, http.StatusOK, status)
			require.Empty(b, answer.Ready)
			require.Empty(b, answer.Unwatched)
		}
		b.ReportMetric(1, "requests/op")
	})
}
