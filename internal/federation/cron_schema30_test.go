package federation_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
)

// This compatibility gate runs a real pre-cron executable. The optional
// external binary is test-owned; ordinary package runs need no old toolchain.
func TestCronSchema30ExecutableCompatibility(t *testing.T) {
	binary := os.Getenv("KATA_TEST_SCHEMA30_BINARY")
	if binary == "" {
		t.Skip("set KATA_TEST_SCHEMA30_BINARY to the retained schema30 executable")
	}
	home := t.TempDir()
	workspace := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	oldURL := "http://" + address
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "KATA_HOME="+home, "KATA_DB="+filepath.Join(home, "kata.db"), "KATA_AUTHOR=worker", "KATA_FEDERATION_PULL_INTERVAL_MS=100")
	//nolint:gosec // Owned test daemon binary, arguments and temporary directory.
	log, err := os.Create(filepath.Join(home, "daemon.log"))
	require.NoError(t, err)
	//nolint:gosec // Owned test daemon binary, arguments and temporary directory.
	process := exec.Command(binary, "daemon", "start", "--foreground", "--listen", address)
	process.Env = env
	process.Dir = workspace
	process.Stdout = log
	process.Stderr = log
	require.NoError(t, process.Start())
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	t.Cleanup(func() {
		_ = process.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = process.Process.Kill()
			<-done
		}
		_ = log.Close()
	})
	//nolint:kennlint // An external daemon process cannot participate in synctest; poll its bounded public readiness/progress signal.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, oldURL+"/api/v1/ping", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == 200
	}, 15*time.Second, 25*time.Millisecond)
	_, raw := nativeRequest(t, oldURL+"/api/v1/instance", "", "", http.MethodGet, nil)
	var instance struct {
		InstanceUID   string `json:"instance_uid"`
		SchemaVersion int    `json:"schema_version"`
	}
	require.NoError(t, json.Unmarshal(raw, &instance))
	require.Equal(t, 30, instance.SchemaVersion)

	// A real old CLI joins a never-used project; its metadata/bootstrap/poll
	// requests omit feature support. Once the first cron commits, its
	// already-enrolled runner must stop without silently advancing its cursor.
	hub := nativeStore(t, "sqlite")
	handler := daemon.NewServer(daemon.ServerConfig{DB: hub, Broadcaster: daemon.NewEventBroadcaster()}).Handler()
	var oldPolls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if strings.HasSuffix(r.URL.Path, "/federation/events") && r.Header.Get(db.EventFeaturesHeader) == "" {
			oldPolls.Add(1)
		}
	}))
	t.Cleanup(server.Close)
	project, err := hub.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	issue, _, err := hub.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Purge reset fixture", Author: "worker"})
	require.NoError(t, err)
	boundary, err := hub.EnableProjectFederation(t.Context(), project.ID, "worker")
	require.NoError(t, err)
	grant, err := hub.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: instance.InstanceUID, ProjectID: &project.ID, Actor: "worker", Capabilities: "pull"})
	require.NoError(t, err)
	args := []string{"--workspace", workspace, "--project", "spoke-project", "--json", "federation", "join", "--hub-url", server.URL, "--hub-project-id", fmt.Sprint(project.ID), "--actor", "worker", "--capabilities", "pull", "--token", grant.Token}
	//nolint:gosec // Owned test daemon binary, arguments and temporary directory.
	join := exec.CommandContext(t.Context(), binary, args...)
	join.Env = append(append([]string{}, env...), "KATA_SERVER="+oldURL)
	join.Dir = workspace
	output, err := join.CombinedOutput()
	require.NoError(t, err, string(output))
	status := func() api.FederationStatusBody {
		_, raw := nativeRequest(t, oldURL+"/api/v1/federation/status", "", "", http.MethodGet, nil)
		var body api.FederationStatusBody
		require.NoError(t, json.Unmarshal(raw, &body))
		return body
	}
	//nolint:kennlint // An external daemon process cannot participate in synctest; poll its bounded public readiness/progress signal.
	require.Eventually(t, func() bool {
		body := status()
		return len(body.Statuses) == 1 && body.Statuses[0].PullCursorEventID >= boundary.ReplayHorizonEventID
	}, 20*time.Second, 50*time.Millisecond)
	before := status().Statuses[0].PullCursorEventID
	_, err = hub.PurgeIssue(t.Context(), issue.ID, "worker", nil)
	require.NoError(t, err)
	//nolint:kennlint // An external daemon process cannot participate in synctest; poll its bounded public readiness/progress signal.
	require.Eventually(t, func() bool {
		body := status()
		return body.Statuses[0].PullCursorEventID > before && body.Statuses[0].LastError == nil
	}, 20*time.Second, 25*time.Millisecond, "actual old reader accepts a legacy-only purge reset")
	issue, issueEvent, err := hub.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Cron reset fixture", Author: "worker"})
	require.NoError(t, err)
	//nolint:kennlint // An external daemon process cannot participate in synctest; poll its bounded public readiness/progress signal.
	require.Eventually(t, func() bool { return status().Statuses[0].PullCursorEventID >= issueEvent.ID }, 20*time.Second, 25*time.Millisecond)
	before = status().Statuses[0].PullCursorEventID
	nativeJob(t, hub, project)
	//nolint:kennlint // An external daemon process cannot participate in synctest; poll its bounded public readiness/progress signal.
	require.Eventually(t, func() bool {
		body := status()
		return len(body.Statuses) == 1 && body.Statuses[0].LastError != nil && strings.Contains(*body.Statuses[0].LastError, "unsupported_event_features")
	}, 20*time.Second, 50*time.Millisecond)
	require.Equal(t, before, status().Statuses[0].PullCursorEventID)
	_, err = hub.PurgeIssue(t.Context(), issue.ID, "worker", nil)
	require.NoError(t, err)
	pollsBeforeReset := oldPolls.Load()
	//nolint:kennlint // An external daemon process cannot participate in synctest; poll its bounded public readiness/progress signal.
	require.Eventually(t, func() bool { return oldPolls.Load() > pollsBeforeReset }, 20*time.Second, 25*time.Millisecond)
	require.Equal(t, before, status().Statuses[0].PullCursorEventID, "old executable must not accept a cron project's reset cursor")
	require.Contains(t, *status().Statuses[0].LastError, "unsupported_event_features")
	//nolint:gosec // Owned test daemon binary, arguments and temporary directory.
	retry := exec.CommandContext(t.Context(), binary, args...)
	retry.Env = join.Env
	retry.Dir = workspace
	output, err = retry.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "unsupported_event_features")

	// The same schema30 executable is now a hub. Ordinary new-reader publication
	// must retain legacy envelope compatibility; cron publication must fail
	// before POST even though the old project requires no features yet.
	resp, raw := nativeRequest(t, oldURL+"/api/v1/projects", "", "", http.MethodPost, []byte(`{"name":"legacy-hub-project","actor":"worker"}`))
	require.Less(t, resp.StatusCode, 300, string(raw))
	var init struct {
		Project api.ProjectOut `json:"project"`
	}
	require.NoError(t, json.Unmarshal(raw, &init))
	require.Positive(t, init.Project.ID)
	resp, raw = nativeRequest(t, fmt.Sprintf("%s/api/v1/projects/%d/federation/enable", oldURL, init.Project.ID), "", "", http.MethodPost, []byte(`{"actor":"worker"}`))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	origin, err := uid.New()
	require.NoError(t, err)
	input, _ := json.Marshal(map[string]any{"spoke_instance_uid": origin, "project_id": init.Project.ID, "capabilities": "pull,push", "actor": "worker"})
	resp, raw = nativeRequest(t, oldURL+"/api/v1/federation/enrollments", "", "", http.MethodPost, input)
	require.Less(t, resp.StatusCode, 300, string(raw))
	var enrollment api.FederationEnrollmentOut
	require.NoError(t, json.Unmarshal(raw, &enrollment))
	client, err := federation.NewClient(t.Context(), oldURL, enrollment.Token, clientpkg.Opts{})
	require.NoError(t, err)
	eventUID, err := uid.New()
	require.NoError(t, err)
	event := api.FederationIngestEventEnvelope{EventID: 1, EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: init.Project.UID, ProjectName: init.Project.Name, Type: "project.metadata_updated", Actor: "worker", HLCPhysicalMS: time.Now().UnixMilli(), CreatedAt: time.Now().UTC().Truncate(time.Millisecond), Payload: jsontext.Value(`{"diff":{"review":{"from":null,"to":"received"}}}`)}
	event.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, CreatedAt: event.CreatedAt.Format(db.EventTimestampFormat), Payload: event.Payload})
	require.NoError(t, err)
	accepted, err := client.IngestProjectEvents(t.Context(), init.Project.ID, []api.FederationIngestEventEnvelope{event})
	require.NoError(t, err)
	require.Equal(t, 1, accepted.Accepted)
	event.Type = "cron.job.created"
	event.EventID = 2
	_, err = client.IngestProjectEvents(t.Context(), init.Project.ID, []api.FederationIngestEventEnvelope{event})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
}
