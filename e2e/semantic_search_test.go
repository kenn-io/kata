//go:build !windows

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestE2E_SemanticSearch_HybridAndDegraded boots a real `kata daemon`
// subprocess configured with [search.embeddings] pointing at an in-test
// deterministic embedder, and exercises the whole semantic-search path that
// only exists in the daemon's process wiring (startEmbeddingReconciler: the
// reconciler goroutine, the broadcaster nudge, the initial backfill, and the
// /health backlog report). All assertions are on client-observable behavior —
// HTTP responses and /health — never on sidecar internals (table names,
// generation state, fingerprint shape):
//
//  1. create an issue → it is found lexically *immediately*, before any embed;
//  2. a paraphrase that shares no salient tokens first misses lexically, then —
//     once the reconciler embeds the issue — is found by polling hybrid search
//     until the vector leg surfaces it (mode=hybrid, "semantic" in
//     matched_in), and an explicit mode=semantic request for the same query
//     returns the same hit;
//  3. /health reports the embeddings block with backlog back at 0 once the
//     reconciler has drained;
//  4. editing the issue onto an unrelated topic makes it findable under a new
//     paraphrase and no longer findable under the old one, proving the edit
//     was actually re-embedded rather than served from a stale vector;
//  5. kill the embedder → an auto search returns results with mode=lexical and
//     degraded=true, and an explicit --hybrid request is rejected 503.
//
// The deterministic embedder maps inputs to fixed unit vectors by topic, so a
// paraphrase lands on the same axis as its source (cosine 1.0, clearing the
// 0.3 floor) while an unrelated topic is orthogonal (cosine 0.0, filtered).
// This is the e2e from the design note's testing plan ("create → instant
// lexical hit; after reconcile → paraphrase found semantically; embedder
// killed → degraded lexical") with neutral placeholder data.
func TestE2E_SemanticSearch_HybridAndDegraded(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tests are slow")
	}

	embedder := newFixtureEmbedder(t)

	dirs := newE2EDirs(t)
	writeEmbeddingsConfig(t, dirs.home, embedder.URL(), fixtureModelV1)

	bin := buildKataBinary(t)
	env := dirs.env()

	daemonStderr := startDaemon(t, bin, env)
	baseURL, client := connectDaemon(t, dirs, daemonStderr)

	pid := initProjectE2E(t, client, baseURL, dirs.repoDir)
	pidStr := strconv.FormatInt(pid, 10)

	// 1. Create an authentication-topic issue and assert it is searchable
	// lexically the instant it is committed — before the reconciler has had a
	// chance to embed anything. Lexical freshness is same-commit (FTS triggers
	// fire in the writing transaction); semantic freshness is eventual.
	short := createIssueWithBody(t, client, baseURL, pid,
		"Login callback double-submits on Safari",
		"A redirect race condition fires the auth callback twice.")

	lexical := searchHybrid(t, client, baseURL, pidStr, "callback", "lexical")
	require.Equal(t, "lexical", lexical.Mode)
	require.True(t, containsIssue(lexical, short),
		"freshly created issue must be found lexically before reconcile: %+v", lexical)

	// 2. A paraphrase whose tokens appear nowhere in the issue's title or body
	// ("credential loop on returning users" vs "Login callback double-submits
	// on Safari" / "A redirect race condition fires the auth callback twice")
	// can only surface via the vector leg. First confirm lexical search finds
	// nothing, isolating the semantic contribution.
	const paraphrase = "credential loop on returning users"
	lexMiss := searchHybrid(t, client, baseURL, pidStr, paraphrase, "lexical")
	require.Falsef(t, containsIssue(lexMiss, short),
		"paraphrase must NOT match lexically (isolates the semantic leg): %+v", lexMiss)

	// 3. Poll the actual hybrid search until the reconciler has embedded the
	// issue and the vector leg surfaces it. Asserting on the search behavior
	// (rather than waiting for /health backlog==0) is immune to gauge timing:
	// the reconciler's initial backlog==0 can read clean before the post-create
	// wake has embedded anything, so a health wait can race ahead of the index.
	hybrid := waitForSemanticHit(t, client, baseURL, pidStr, paraphrase, short, daemonStderr)
	require.Equal(t, "hybrid", hybrid.Mode, "explicit hybrid must run the vector leg: %+v", hybrid)
	require.False(t, hybrid.Degraded, "embedder is up; hybrid must not be degraded: %+v", hybrid)
	hit, ok := findHit(hybrid, short)
	require.Truef(t, ok, "paraphrase must surface the issue via the vector leg: %+v", hybrid)
	require.Containsf(t, hit.MatchedIn, "semantic",
		"vector leg must be credited in matched_in: %+v", hit)

	// 3b. mode=semantic isolates the vector leg entirely (no lexical fallback
	// mixed in) and must return the same nearest issue.
	semantic := searchHybrid(t, client, baseURL, pidStr, paraphrase, "semantic")
	require.Equal(t, "semantic", semantic.Mode)
	semHit, ok := findHit(semantic, short)
	require.Truef(t, ok, "mode=semantic must surface the semantically-nearest issue: %+v", semantic)
	require.Containsf(t, semHit.MatchedIn, "semantic",
		"mode=semantic hit must be credited to the vector leg: %+v", semHit)

	// 3c. Once the reconciler has caught up, /health must report the backlog
	// gauge back at 0 — the operator-visible signal that nothing is pending.
	waitForBacklogZero(t, client, baseURL, daemonStderr)

	// 4. Edit the issue onto an unrelated topic (no shared keywords with the
	// original auth-flavored content) and confirm the new content is what's
	// searchable now: a paraphrase of the *new* topic finds it via the vector
	// leg, and the *original* paraphrase — which used to match — no longer
	// does. The second half is what proves this is a real re-embed and not
	// just an additional vector alongside a stale one.
	editIssueE2E(t, client, baseURL, pid, short,
		"Export button produces malformed CSV",
		"Large date ranges truncate rows silently during report export.")

	const editedParaphrase = "broken CSV output for big date spans"
	edited := waitForSemanticHit(t, client, baseURL, pidStr, editedParaphrase, short, daemonStderr)
	require.Equal(t, "hybrid", edited.Mode)
	editedHit, ok := findHit(edited, short)
	require.Truef(t, ok, "edited issue must be searchable under its new content: %+v", edited)
	require.Containsf(t, editedHit.MatchedIn, "semantic",
		"re-embedded content must be credited to the vector leg: %+v", editedHit)

	staleCheck := searchHybrid(t, client, baseURL, pidStr, paraphrase, "semantic")
	require.Falsef(t, containsIssue(staleCheck, short),
		"issue must no longer match its old topic after being re-embedded onto a new one: %+v", staleCheck)

	// 5. Kill the embedder. An auto/hybrid request can no longer embed the
	// query, so the vector leg fails. auto degrades to labeled lexical; an
	// explicit hybrid request is an honest 503 rather than a silent downgrade.
	embedder.Close()

	degraded := searchHybrid(t, client, baseURL, pidStr, editedParaphrase, "")
	require.Equal(t, "lexical", degraded.Mode,
		"auto must fall back to lexical when the embedder is down: %+v", degraded)
	require.True(t, degraded.Degraded,
		"the fallback must be labeled degraded, not silent: %+v", degraded)
	require.NotEmpty(t, degraded.DegradedReason, "degraded responses must carry a reason")

	status, body := searchStatus(t, client, baseURL, pidStr, editedParaphrase, "hybrid")
	require.Equalf(t, http.StatusServiceUnavailable, status,
		"explicit hybrid with a dead embedder must be 503, not a downgrade: %s", body)
}

// TestE2E_SemanticSearch_ModelChangeCutover proves an embedding model change
// (the [search.embeddings].model config key) does not disrupt search: after a
// daemon restart with a different model, the reconciler builds a new
// generation in the background and, once it is fully filled and cut over,
// search keeps answering with the same response shape. The cutover is
// invisible to a client that only ever calls /search and /health.
//
// The fixture embedder's vectorForModel swaps which axis represents the
// "auth" topic whenever the configured model isn't the baseline, so this is
// not a trivial no-op check: if the daemon kept serving the OLD generation's
// vectors after the restart (no re-embed, or a cutover that never completes),
// the live query embedding — always computed under the *current* client's
// model — would land on the axis opposite the stale stored vectors, cosine
// similarity would collapse to 0 (below the 0.3 floor), and the poll below
// would time out instead of passing.
func TestE2E_SemanticSearch_ModelChangeCutover(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tests are slow")
	}

	embedder := newFixtureEmbedder(t)
	dirs := newE2EDirs(t)
	writeEmbeddingsConfig(t, dirs.home, embedder.URL(), fixtureModelV1)

	bin := buildKataBinary(t)
	env := dirs.env()

	cmd, stderr := startDaemonCmd(t, bin, env)
	baseURL, client := connectDaemon(t, dirs, stderr)

	pid := initProjectE2E(t, client, baseURL, dirs.repoDir)
	pidStr := strconv.FormatInt(pid, 10)

	short := createIssueWithBody(t, client, baseURL, pid,
		"Login callback double-submits on Safari",
		"A redirect race condition fires the auth callback twice.")

	const paraphrase = "credential loop on returning users"
	baseline := waitForSemanticHit(t, client, baseURL, pidStr, paraphrase, short, stderr)
	require.Equal(t, "hybrid", baseline.Mode, "baseline search before the model change: %+v", baseline)

	// Restart the daemon on the same KATA_HOME with a changed model. The
	// sidecar vector database (kata.vectors.db) lives next to kata.db and
	// survives the restart, so the generation built under fixtureModelV1 is
	// still on disk when the new daemon process opens the index — this is
	// what the new generation must eventually cut over from.
	stopDaemon(cmd)
	writeEmbeddingsConfig(t, dirs.home, embedder.URL(), fixtureModelV2)
	_, stderr2 := startDaemonCmd(t, bin, env)
	baseURL2, client2 := connectDaemon(t, dirs, stderr2)

	after := waitForSemanticHit(t, client2, baseURL2, pidStr, paraphrase, short, stderr2)
	require.Equal(t, "hybrid", after.Mode,
		"response shape must be unchanged after the model cutover: %+v", after)
	require.False(t, after.Degraded,
		"embedder is up under the new model; must not be degraded: %+v", after)
	hit, ok := findHit(after, short)
	require.Truef(t, ok,
		"paraphrase must still surface the issue once the new generation is filled and active: %+v", after)
	require.Containsf(t, hit.MatchedIn, "semantic",
		"vector leg must be credited in matched_in after cutover: %+v", hit)
}

// fixtureModelV1 and fixtureModelV2 name two distinct embedding models for
// the fixture embedder. Any model name other than fixtureModelV1 flips
// vectorForModel's axis mapping, so a config model change always yields
// different (but still deterministic) vectors for the same text.
const (
	fixtureModelV1 = "fixture-embed"
	fixtureModelV2 = "fixture-embed-v2"
)

// fixtureEmbedder is a deterministic OpenAI-compatible /embeddings endpoint
// for the e2e. It returns one fixed unit vector per (model, input) pair,
// chosen by topic so paraphrases of the same concept collide and unrelated
// text is orthogonal, and flipped per model so a model change is observable.
type fixtureEmbedder struct {
	srv    *httptest.Server
	closed atomic.Bool
}

// newFixtureEmbedder starts the fake embedder on a loopback listener. Loopback
// HTTP needs no trust_private_network and (because the fixture takes no API
// key) never touches the bearer safety ladder.
func newFixtureEmbedder(t *testing.T) *fixtureEmbedder {
	t.Helper()
	f := &fixtureEmbedder{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type vec struct {
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []vec `json:"data"`
		}{Data: make([]vec, len(req.Input))}
		for i, in := range req.Input {
			out.Data[i] = vec{Embedding: vectorForModel(req.Model, in)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fixtureEmbedder) URL() string { return f.srv.URL + "/v1" }

// Close shuts the embedder down. Idempotent so both the explicit kill in the
// test and the t.Cleanup registration are safe.
func (f *fixtureEmbedder) Close() {
	if f.closed.CompareAndSwap(false, true) {
		f.srv.Close()
	}
}

// topicAxis classifies text onto the authentication axis (0) or everything
// else (1), by keyword.
func topicAxis(text string) int {
	lower := strings.ToLower(text)
	for _, kw := range []string{"auth", "login", "callback", "redirect", "sign-in", "credential"} {
		if strings.Contains(lower, kw) {
			return 0
		}
	}
	return 1
}

// vectorForModel maps text to a deterministic 2-D unit vector by topic axis
// (see topicAxis), then flips the axis whenever model isn't fixtureModelV1.
// Two phrasings of the same concept under the *same* model therefore have
// cosine 1.0 (well above the 0.3 floor); the same phrasing embedded under
// different models lands on opposite axes, so a stale vector from the wrong
// model is orthogonal (cosine 0.0, filtered) to a live query embedded under
// the current model.
func vectorForModel(model, text string) []float32 {
	axis := topicAxis(text)
	if model != fixtureModelV1 {
		axis = 1 - axis
	}
	if axis == 0 {
		return []float32{1, 0}
	}
	return []float32{0, 1}
}

// writeEmbeddingsConfig drops a config.toml under home enabling semantic
// search against baseURL under the given model. The daemon subprocess reads
// this from KATA_HOME at startup, so the full startEmbeddingReconciler wiring
// runs; a later call (e.g. after stopping the daemon) with a different model
// is how the model-change e2e exercises a config change across a restart.
func writeEmbeddingsConfig(t *testing.T, home, baseURL, model string) {
	t.Helper()
	body := fmt.Sprintf(`[search.embeddings]
base_url = %q
model = %q
dims = 2
`, baseURL, model)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
}

// searchHitRow is the decoded subset of one /search result row.
type searchHitRow struct {
	Issue struct {
		ShortID string `json:"short_id"`
	} `json:"issue"`
	MatchedIn []string `json:"matched_in"`
}

// searchResult is the decoded subset of the /search envelope the e2e asserts on.
type searchResult struct {
	Mode           string         `json:"mode"`
	Degraded       bool           `json:"degraded"`
	DegradedReason string         `json:"degraded_reason"`
	Results        []searchHitRow `json:"results"`
}

// searchHybrid issues a search and requires a 200, returning the decoded
// envelope. mode "" exercises the auto path.
func searchHybrid(t *testing.T, client *http.Client, baseURL, pidStr, query, mode string) searchResult {
	t.Helper()
	status, body := searchStatus(t, client, baseURL, pidStr, query, mode)
	require.Equalf(t, http.StatusOK, status, "search %q mode=%q: %s", query, mode, body)
	var res searchResult
	require.NoErrorf(t, json.Unmarshal(body, &res), "decode search response: %s", body)
	return res
}

// searchStatus issues GET /search and returns the status code + raw body so
// callers can assert non-200 paths (the explicit-hybrid 503).
func searchStatus(t *testing.T, client *http.Client, baseURL, pidStr, query, mode string) (int, []byte) {
	t.Helper()
	q := url.Values{"q": {query}}
	if mode != "" {
		q.Set("mode", mode)
	}
	u := baseURL + "/api/v1/projects/" + pidStr + "/search?" + q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	require.NoError(t, err)
	resp, err := client.Do(req) //nolint:gosec // G704: test-only unix socket, fixed URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func findHit(res searchResult, shortID string) (searchHitRow, bool) {
	for _, r := range res.Results {
		if r.Issue.ShortID == shortID {
			return r, true
		}
	}
	return searchHitRow{}, false
}

func containsIssue(res searchResult, shortID string) bool {
	_, ok := findHit(res, shortID)
	return ok
}

// createIssueWithBody posts an issue with a title and body and returns its
// short_id. The hooks harness's createIssueE2E hardcodes the title and returns
// nothing; the semantic e2e needs distinct content and the resulting ref.
func createIssueWithBody(t *testing.T, client *http.Client, baseURL string, pid int64, title, body string) string {
	t.Helper()
	u := baseURL + "/api/v1/projects/" + strconv.FormatInt(pid, 10) + "/issues"
	payload, err := json.Marshal(map[string]any{"actor": "tester", "title": title, "body": body})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) //nolint:gosec // G704: test-only unix socket, fixed URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "create issue: %s", raw)
	var parsed struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoErrorf(t, json.Unmarshal(raw, &parsed), "create response: %s", raw)
	require.NotEmptyf(t, parsed.Issue.ShortID, "short_id missing: %s", raw)
	return parsed.Issue.ShortID
}

// editIssueE2E PATCHes an issue's title and body and requires a 200. Used to
// prove an edited issue is re-embedded: the daemon must notice the content
// change (via the mirror's content_revision) and run the new text through the
// embedder rather than serving the original vector forever.
func editIssueE2E(t *testing.T, client *http.Client, baseURL string, pid int64, ref, title, body string) {
	t.Helper()
	u := baseURL + "/api/v1/projects/" + strconv.FormatInt(pid, 10) + "/issues/" + ref
	payload, err := json.Marshal(map[string]any{"actor": "tester", "title": title, "body": body})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch, u, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) //nolint:gosec // G704: test-only unix socket, fixed URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "edit issue: %s", raw)
}

// waitForSemanticHit polls the real mode=hybrid search until the paraphrase
// surfaces the target issue via the vector leg — the hit must credit
// "semantic" in matched_in, since a query sharing tokens with the issue can
// surface it through the lexical leg before the reconciler has (re-)embedded
// it — then returns that response. It asserts on observable search behavior
// rather than the /health backlog gauge, so it is immune to gauge timing: the
// reconciler's pre-embed backlog can read 0 before the post-create wake has
// embedded anything, which a backlog wait would accept too early. The poll is
// deadline-bounded and, on timeout, reports the last search response, the
// last /health snapshot, and daemon stderr so a genuinely stuck embedder
// surfaces as a clear message instead of a bare timeout.
func waitForSemanticHit(t *testing.T, client *http.Client, baseURL, pidStr, query, shortID string, daemonStderr *safeBuffer) searchResult {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var lastSearch string
	for time.Now().Before(deadline) {
		status, body := searchStatus(t, client, baseURL, pidStr, query, "hybrid")
		lastSearch = string(body)
		if status == http.StatusOK {
			var res searchResult
			require.NoErrorf(t, json.Unmarshal(body, &res), "decode search response: %s", body)
			if hit, ok := findHit(res, shortID); ok && slices.Contains(hit.MatchedIn, "semantic") {
				return res
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("query %q never surfaced issue %s via the vector leg.\nlast search: %s\nlast /health: %s\ndaemon stderr: %s",
		query, shortID, lastSearch, embeddingHealth(t, client, baseURL), daemonStderr.String())
	return searchResult{}
}

// embeddingHealth fetches /health and returns the embeddings block as a string
// for diagnostics when a semantic poll times out. It is best-effort: any error
// is folded into the returned string rather than failing the test, since it
// only runs on an already-failing path.
func embeddingHealth(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/api/v1/health", nil)
	if err != nil {
		return fmt.Sprintf("(health request error: %v)", err)
	}
	resp, err := client.Do(req) //nolint:gosec // G704: test-only unix socket, fixed URL
	if err != nil {
		return fmt.Sprintf("(health fetch error: %v)", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("(health read error: %v)", err)
	}
	return string(raw)
}

// waitForBacklogZero polls /health until the embeddings block reports
// backlog 0 — the operator-visible signal that the reconciler has fully
// drained, not merely that one search query happened to find a match. On
// timeout it reports the last /health snapshot and daemon stderr.
func waitForBacklogZero(t *testing.T, client *http.Client, baseURL string, daemonStderr *safeBuffer) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = embeddingHealth(t, client, baseURL)
		// Embeddings is a pointer so a missing block cannot decode to a
		// zero struct and vacuously satisfy backlog == 0.
		var health struct {
			Embeddings *struct {
				Configured bool  `json:"configured"`
				Backlog    int64 `json:"backlog"`
			} `json:"embeddings"`
		}
		if err := json.Unmarshal([]byte(last), &health); err == nil &&
			health.Embeddings != nil && health.Embeddings.Configured && health.Embeddings.Backlog == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("embeddings backlog never reached 0.\nlast /health: %s\ndaemon stderr: %s", last, daemonStderr.String())
}

// startDaemonCmd is startDaemon (see hooks_test.go) but also returns the
// *exec.Cmd, so a caller can explicitly stop this daemon mid-test — e.g. to
// restart it with a changed config on the same KATA_HOME — instead of only
// stopping it via t.Cleanup at test end.
func startDaemonCmd(t *testing.T, bin string, env []string) (*exec.Cmd, *safeBuffer) {
	t.Helper()
	stderr := &safeBuffer{}
	//nolint:gosec // G204: bin is buildKataBinary's output
	cmd := exec.Command(bin, "daemon", "start", "--foreground")
	cmd.Env = env
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { stopDaemon(cmd) })
	return cmd, stderr
}
