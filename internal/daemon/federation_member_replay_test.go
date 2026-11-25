//go:build linux

package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

// The comparison uses 36 devices and 365 projects. Each device polls and
// pushes each of its bindings, with one push at a time per device.
const (
	replayMembers           = 36
	replayProjects          = 365
	replayPollWait          = 30
	replayLargePushes       = 4
	replayLargePushBodySize = 60 << 20
	replayHubEnv            = "KATA_FEDERATION_REPLAY_HUB"
)

type replayKey struct {
	KeyID        string `json:"key_id"`
	KeyEnv       string `json:"key_env"`
	EnrollmentID int64  `json:"enrollment_id"`
}

type replayHubAddrs struct {
	Federation string `json:"federation"`
	Stats      string `json:"stats"`
}

type replayHubStats struct {
	WaitCount        int64         `json:"wait_count"`
	WaitDuration     time.Duration `json:"wait_duration"`
	WriteWaitCount   int64         `json:"write_wait_count"`
	WriteWaitSeconds float64       `json:"write_wait_seconds"`
}

// TestFederationMemberReplayHub is the hub process for
// TestFederationMemberReplay. It serves the prepared database on a federation
// listener, and the store's pool statistics on a second listener, until its
// standard input closes.
func TestFederationMemberReplayHub(t *testing.T) {
	dir := os.Getenv(replayHubEnv)
	if dir == "" {
		t.Skip("started by TestFederationMemberReplay")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "keys.json")) //nolint:gosec // test-owned replay directory
	require.NoError(t, err)
	var stored []replayKey
	require.NoError(t, json.Unmarshal(raw, &stored))
	keys := make([]signingpolicy.Key, len(stored))
	for i, k := range stored {
		keys[i] = signingpolicy.Key{KeyID: k.KeyID, KeyEnv: k.KeyEnv, EnrollmentID: k.EnrollmentID}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, err := sqlitestore.Open(ctx, filepath.Join(dir, "hub.db"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	state := filepath.Join(dir, "replay.state")
	require.NoError(t, federationsigning.InitializeReplayState(state))
	verifier, err := federationsigning.NewVerifier("https://hub.example", keys, state)
	require.NoError(t, err)
	defer func() { _ = verifier.Close() }()
	time.Sleep(federationsigning.Quarantine) //nolint:kennlint // sleeptest: the verifier accepts signatures only after its real-time quarantine
	srv := daemon.NewServer(daemon.ServerConfig{DB: store, FederationSigning: verifier})
	defer func() { _ = srv.Close() }()
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	statsListener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	runtime.SetBlockProfileRate(1)
	stats := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := store.Stats()
		var profile bytes.Buffer
		_ = pprof.Lookup("block").WriteTo(&profile, 1)
		var cycles, count, hz int64
		for _, block := range strings.Split(profile.String(), "\n\n") {
			for _, line := range strings.Split(block, "\n") {
				if _, rest, ok := strings.Cut(line, "cycles/second="); ok {
					_, _ = fmt.Sscan(rest, &hz)
				}
			}
			if strings.Contains(block, "sqlitestore.(*writeQueue).acquire") {
				for _, line := range strings.Split(block, "\n") {
					var c, n int64
					if scanned, _ := fmt.Sscan(line, &c, &n); scanned == 2 {
						cycles += c
						count += n
					}
				}
			}
		}
		var seconds float64
		if hz > 0 {
			seconds = float64(cycles) / float64(hz)
		}
		_ = json.NewEncoder(w).Encode(replayHubStats{WaitCount: s.WaitCount, WaitDuration: s.WaitDuration, WriteWaitCount: count, WriteWaitSeconds: seconds})
	})}
	go func() { _ = stats.Serve(statsListener) }()
	defer func() { _ = stats.Close() }()
	served := make(chan error, 1)
	go func() {
		served <- srv.ServeListeners(ctx, daemon.ListenerBinding{Listener: listener, Policy: daemon.ListenerPolicy{Kind: daemon.ListenerFederation}})
	}()
	addrs, err := json.Marshal(replayHubAddrs{Federation: listener.Addr().String(), Stats: statsListener.Addr().String()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "addrs.json.tmp"), addrs, 0o600))                  //nolint:gosec // test-owned replay directory
	require.NoError(t, os.Rename(filepath.Join(dir, "addrs.json.tmp"), filepath.Join(dir, "addrs.json"))) //nolint:gosec // test-owned replay directory
	_, _ = io.Copy(io.Discard, os.Stdin)
	cancel()
	<-served
}

// TestFederationMemberReplay drives a hub in a child process the way member
// spokes do, logs a REPLAY line per phase, and then requires every request to
// succeed and every pushed change to land. CPU, peak RSS, temporary disk, and
// database waits are the hub process's alone. Block profiles measure SQLite's
// write queue separately from connection-pool waits. It compiles on main and
// this branch for comparison. The hub waits out the signing quarantine, so it
// runs only when KATA_FEDERATION_REPLAY=1.
func TestFederationMemberReplay(t *testing.T) {
	if os.Getenv("KATA_FEDERATION_REPLAY") != "1" {
		t.Skip("set KATA_FEDERATION_REPLAY=1 to run the paired federation replay")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	hubTmp := filepath.Join(dir, "tmp")
	require.NoError(t, os.Mkdir(hubTmp, 0o700))
	store, err := sqlitestore.Open(ctx, filepath.Join(dir, "hub.db"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	projects := make([]db.Project, replayProjects)
	for i := range projects {
		projects[i], err = store.CreateProject(ctx, fmt.Sprintf("replay-project-%03d", i))
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, projects[i].ID, "tester")
		require.NoError(t, err)
	}
	// An enrollment is one member's credential for one project; each has its
	// own signing key and secret.
	type enrollment struct {
		project db.Project
		token   string
		source  signingpolicy.Source
	}
	type member struct {
		spokeUID string
		shared   enrollment
		own      []enrollment
	}
	members := make([]member, replayMembers)
	var keys []replayKey
	enroll := func(i int, project db.Project) enrollment {
		keyEnv := fmt.Sprintf("TEST_REPLAY_KEY_%d", len(keys))
		t.Setenv(keyEnv, fmt.Sprintf("%064d", len(keys)+1))
		e := enrollment{
			project: project,
			token:   fmt.Sprintf("replay-member-%d-project-%d", i, project.ID),
			source:  signingpolicy.Source{KeyID: fmt.Sprintf("member-%d-project-%d", i, project.ID), KeyEnv: keyEnv},
		}
		created, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{Token: e.token, SpokeInstanceUID: members[i].spokeUID, ProjectID: &project.ID, Capabilities: "pull,push", Actor: "tester"})
		require.NoError(t, err)
		keys = append(keys, replayKey{KeyID: e.source.KeyID, KeyEnv: keyEnv, EnrollmentID: created.Enrollment.ID})
		return e
	}
	for i := range members {
		members[i].spokeUID = fmt.Sprintf("01HZNQ7VFPK1XGD8R5MAB%05d", i)
		members[i].shared = enroll(i, projects[0])
		for p := 1 + i; p < replayProjects; p += replayMembers {
			members[i].own = append(members[i].own, enroll(i, projects[p]))
		}
	}
	raw, err := json.Marshal(keys)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keys.json"), raw, 0o600))
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)

	// KATA_FEDERATION_REPLAY_HUB_FLAGS passes test flags such as
	// -test.cpuprofile to the hub process.
	args := append([]string{"-test.run=^TestFederationMemberReplayHub$", "-test.v"}, strings.Fields(os.Getenv("KATA_FEDERATION_REPLAY_HUB_FLAGS"))...)
	hub := exec.CommandContext(ctx, os.Args[0], args...) //nolint:gosec // re-executes this test binary
	hub.Env = append(os.Environ(), replayHubEnv+"="+dir, "TMPDIR="+hubTmp)
	hubLog, err := os.Create(filepath.Join(dir, "hub.log")) //nolint:gosec // test-owned replay directory
	require.NoError(t, err)
	defer func() { _ = hubLog.Close() }()
	hub.Stdout, hub.Stderr = hubLog, hubLog
	stdin, err := hub.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, hub.Start())
	defer func() {
		_ = stdin.Close()
		if err := hub.Wait(); err != nil {
			hubOutput, _ := os.ReadFile(filepath.Join(dir, "hub.log")) //nolint:gosec // test-owned replay directory
			t.Errorf("hub process: %v\n%s", err, hubOutput)
		}
	}()
	var addrs replayHubAddrs
	//nolint:kennlint // sleeptest: the hub is another process; its address file is the only signal
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(dir, "addrs.json")) //nolint:gosec // test-owned replay directory
		return err == nil && json.Unmarshal(raw, &addrs) == nil
	}, federationsigning.Quarantine+time.Minute, 100*time.Millisecond, "hub did not start")
	pid := hub.Process.Pid

	// Each request opens its own connection, as separate member hosts do.
	client := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}
	type outcome struct {
		kind       string
		status     int
		events     int
		started    time.Time
		elapsed    time.Duration
		err        error
		detail     string
		retryAfter time.Duration
		completion time.Duration
	}
	send := func(requestCtx context.Context, e enrollment, method, path string, body []byte) outcome {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(requestCtx, method, "https://hub.example"+path, reader)
		if err != nil {
			return outcome{err: err}
		}
		req.Header.Set("Authorization", "Bearer "+e.token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := federationsigning.Sign(req, e.source); err != nil {
			return outcome{err: err}
		}
		req.URL.Scheme, req.URL.Host, req.Host = "http", addrs.Federation, "hub.example"
		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return outcome{started: started, elapsed: time.Since(started), err: err}
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		out := outcome{status: resp.StatusCode, started: started, elapsed: time.Since(started), err: err}
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			out.retryAfter = time.Duration(seconds) * time.Second
		}
		if resp.StatusCode != http.StatusOK {
			out.detail = fmt.Sprintf("%d %.200s", resp.StatusCode, raw)
		}
		var page struct {
			Events []json.RawMessage `json:"events"`
			Ready  []int64           `json:"ready"`
		}
		if resp.StatusCode == http.StatusOK && json.Unmarshal(raw, &page) == nil {
			out.events = len(page.Events) + len(page.Ready)
		}
		return out
	}
	pushes := 0
	pushBody := func(e enrollment, spokeUID, issueBody string) []byte {
		pushes++
		issueUID := fmt.Sprintf("01HZNQ7VFPK1XGD8R5MAC%05d", pushes)
		shortID := strings.ToLower(issueUID[len(issueUID)-5:])
		ev := federationRemoteIssueCreatedEvent(t, e.project, spokeUID)
		ev.EventUID = fmt.Sprintf("01HZNQ7VFPK1XGD8R5MAD%05d", pushes)
		ev.IssueUID = &issueUID
		ev.Payload = fmt.Appendf(nil, `{"uid":%q,"short_id":%q,"title":"member work","body":%q,"author":"tester","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`, issueUID, shortID, issueBody)
		ev.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: ev.EventUID, OriginInstanceUID: ev.OriginInstanceUID, ProjectUID: ev.ProjectUID, ProjectName: ev.ProjectName, IssueUID: ev.IssueUID, Type: ev.Type, Actor: ev.Actor, HLCPhysicalMS: ev.HLCPhysicalMS, HLCCounter: ev.HLCCounter, CreatedAt: ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), Payload: ev.Payload})
		require.NoError(t, err)
		body, err := json.Marshal(federationIngestBody(federationIngestEnvelope(t, int64(pushes), ev)))
		require.NoError(t, err)
		return body
	}
	issueCounts := func() map[int64]int {
		rows, err := store.QueryContext(ctx, `SELECT project_id, count(*) FROM issues GROUP BY project_id`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		counts := map[int64]int{}
		for rows.Next() {
			var project int64
			var n int
			require.NoError(t, rows.Scan(&project, &n))
			counts[project] = n
		}
		require.NoError(t, rows.Err())
		return counts
	}
	hubStats := func() replayHubStats {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addrs.Stats, nil)
		require.NoError(t, err)
		resp, err := client.Do(req) //nolint:gosec // test-owned loopback hub
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var stats replayHubStats
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&stats))
		return stats
	}
	// hubCPU reads the hub's user and system CPU from /proc in clock ticks,
	// which Linux reports at 100 per second.
	hubCPU := func() time.Duration {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		require.NoError(t, err)
		fields := strings.Fields(string(raw[bytes.LastIndexByte(raw, ')')+2:]))
		utime, err := strconv.ParseInt(fields[11], 10, 64)
		require.NoError(t, err)
		stime, err := strconv.ParseInt(fields[12], 10, 64)
		require.NoError(t, err)
		return time.Duration(utime+stime) * 10 * time.Millisecond
	}
	hubPeakRSS := func() int64 {
		f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if rest, ok := strings.CutPrefix(scanner.Text(), "VmHWM:"); ok {
				kib, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
				require.NoError(t, err)
				return kib << 10
			}
		}
		t.Fatal("hub status has no VmHWM")
		return 0
	}

	type summary struct {
		Sent, OK, Refused429, OtherStatus, Errors  int
		EarlyAnswers, Woken, Expired               int     `json:",omitempty"`
		P50Seconds, MaxSeconds                     float64 `json:",omitempty"`
		CompletionP50Seconds, CompletionMaxSeconds float64 `json:",omitempty"`
		FirstFailure                               string  `json:",omitempty"`
		FirstError                                 string  `json:",omitempty"`
	}
	// phase runs one workload, logs a REPLAY line of its counters, and then
	// requires every request to succeed and every expected issue to land.
	// Polls answered empty before settled are early answers: a member would
	// fall back to interval polling.
	phase := func(name string, want map[int64]int, workload func(launch func(kind string, e enrollment, method, path string, body []byte), sequence func(func()), record func(string, enrollment, string, string, []byte)) (settled time.Time)) {
		// Writing 5 resets the hub's peak RSS to its current RSS.
		require.NoError(t, os.WriteFile(fmt.Sprintf("/proc/%d/clear_refs", pid), []byte("5"), 0o600))
		var peakDisk int64
		var minAvailable int64
		sampling, sampled := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(sampled)
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				var disk int64
				_ = filepath.WalkDir(hubTmp, func(_ string, d fs.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						if info, err := d.Info(); err == nil {
							disk += info.Size()
						}
					}
					return nil
				})
				peakDisk = max(peakDisk, disk)
				raw, err := os.ReadFile("/proc/meminfo")
				if err == nil {
					for _, line := range strings.Split(string(raw), "\n") {
						if rest, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
							var kib int64
							_, _ = fmt.Sscan(rest, &kib)
							if minAvailable == 0 || kib < minAvailable {
								minAvailable = kib
							}
						}
					}
				}
				select {
				case <-sampling:
					return
				case <-tick.C:
				}
			}
		}()
		cpuBefore := hubCPU()
		dbBefore := hubStats()
		issuesBefore := issueCounts()
		began := time.Now()
		var mu sync.Mutex
		var outcomes []outcome
		var wg sync.WaitGroup
		record := func(kind string, e enrollment, method, path string, body []byte) {
			started := time.Now()
			for {
				out := send(ctx, e, method, path, body)
				out.kind = kind
				if out.status != http.StatusTooManyRequests || out.retryAfter <= 0 {
					out.completion = time.Since(started)
				}
				mu.Lock()
				outcomes = append(outcomes, out)
				mu.Unlock()
				if out.status != http.StatusTooManyRequests || out.retryAfter <= 0 {
					return
				}
				timer := time.NewTimer(out.retryAfter)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
		}
		settled := workload(func(kind string, e enrollment, method, path string, body []byte) {
			wg.Go(func() { record(kind, e, method, path, body) })
		}, func(f func()) { wg.Go(f) }, record)
		wg.Wait()
		elapsed := time.Since(began)
		close(sampling)
		<-sampled
		cpu := hubCPU() - cpuBefore
		dbAfter := hubStats()
		peakRSS := hubPeakRSS()
		report := map[string]*summary{}
		latencies := map[string][]time.Duration{}
		completion := map[string][]time.Duration{}
		for _, out := range outcomes {
			s := report[out.kind]
			if s == nil {
				s = &summary{}
				report[out.kind] = s
			}
			s.Sent++
			switch {
			case out.err != nil:
				s.Errors++
			case out.status == http.StatusOK:
				s.OK++
			case out.status == http.StatusTooManyRequests && out.retryAfter > 0:
				s.Refused429++
			default:
				s.OtherStatus++
			}
			if (out.kind == "poll" || out.kind == "watch") && out.err == nil && out.status == http.StatusOK {
				switch {
				case out.err == nil && out.started.Add(out.elapsed).Before(settled) && out.events == 0:
					s.EarlyAnswers++
				case out.events > 0:
					s.Woken++
				default:
					s.Expired++
				}
			}
			if s.FirstFailure == "" && out.detail != "" {
				s.FirstFailure = out.detail
			} else if s.FirstFailure == "" && out.err != nil {
				s.FirstFailure = out.err.Error()
			}
			if s.FirstError == "" && out.err != nil {
				s.FirstError = out.err.Error()
			}
			if out.status == http.StatusOK && out.err == nil {
				latencies[out.kind] = append(latencies[out.kind], out.elapsed)
				completion[out.kind] = append(completion[out.kind], out.completion)
			}
		}
		for kind, values := range latencies {
			slices.Sort(values)
			report[kind].P50Seconds = values[len(values)/2].Seconds()
			report[kind].MaxSeconds = values[len(values)-1].Seconds()
		}
		for kind, values := range completion {
			slices.Sort(values)
			report[kind].CompletionP50Seconds = values[len(values)/2].Seconds()
			report[kind].CompletionMaxSeconds = values[len(values)-1].Seconds()
		}
		issuesAfter := issueCounts()
		landed, missing := 0, 0
		for project, n := range want {
			got := issuesAfter[project] - issuesBefore[project]
			landed += min(got, n)
			missing += max(n-got, 0)
		}
		line, err := json.Marshal(map[string]any{
			"phase":                         name,
			"requests":                      report,
			"issues_expected":               landed + missing,
			"issues_missing":                missing,
			"wall_seconds":                  elapsed.Seconds(),
			"hub_cpu_seconds":               cpu.Seconds(),
			"hub_peak_rss_mib":              peakRSS >> 20,
			"host_min_available_mib":        minAvailable >> 10,
			"hub_sqlite_write_wait_count":   dbAfter.WriteWaitCount - dbBefore.WriteWaitCount,
			"hub_sqlite_write_wait_seconds": dbAfter.WriteWaitSeconds - dbBefore.WriteWaitSeconds,
			"hub_peak_tmp_disk_b":           peakDisk,
			"hub_db_pool_wait_count":        dbAfter.WaitCount - dbBefore.WaitCount,
			"hub_db_pool_wait_seconds":      (dbAfter.WaitDuration - dbBefore.WaitDuration).Seconds(),
		})
		require.NoError(t, err)
		t.Logf("REPLAY %s", line)
		for kind, s := range report {
			assert.Equal(t, s.Sent-s.Refused429, s.OK, "%s %s: every request must succeed; first failure: %s", name, kind, s.FirstFailure)
			if name == "sync" && os.Getenv("KATA_FEDERATION_REPLAY_EXPECT_NO_REFUSALS") == "1" {
				assert.Zero(t, s.Refused429, "%s: candidate must admit each request", kind)
			}
			if name == "sync" && kind == "poll" && os.Getenv("KATA_FEDERATION_REPLAY_EXPECT_NO_POLL_REFUSALS") == "1" {
				assert.Zero(t, s.Refused429, "polls must reach their waiter bounds")
			}
		}
		assert.Zero(t, missing, "%s: every pushed change must land", name)
	}

	// Hold eight signed polls and eight idle keep-alive connections. The base
	// admits at most eight signed polls, so all 16 sockets need both kinds.
	{
		beforeCPU, beforeDB := hubCPU(), hubStats()
		pollCtx, stopPolls := context.WithCancel(ctx)
		pollResults := make(chan outcome, 8)
		for i := range 8 {
			e := members[i].shared
			go func() {
				pollResults <- send(pollCtx, e, http.MethodGet, fmt.Sprintf("%s/federation/events?after_id=%d&wait=%d", projectPath(e.project.ID), cursor, replayPollWait), nil)
			}()
		}
		time.Sleep(2 * time.Second) //nolint:kennlint // sleeptest: allow held polls to reach the listener
		early := len(pollResults)
		require.Zero(t, early, "eight polls must stay held")
		var idle []net.Conn
		for i := range 8 {
			e := members[8+i].shared
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addrs.Federation)
			require.NoError(t, err)
			idle = append(idle, conn)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://hub.example"+projectPath(e.project.ID)+"/federation/metadata", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+e.token)
			require.NoError(t, federationsigning.Sign(req, e.source))
			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
			require.NoError(t, req.Write(conn))
			resp, err := http.ReadResponse(bufio.NewReader(conn), req)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, conn.SetDeadline(time.Time{}))
		}
		metadata := make(chan outcome, 1)
		go func() {
			metadata <- send(ctx, members[16].shared, http.MethodGet, projectPath(members[16].shared.project.ID)+"/federation/metadata", nil)
		}()
		var result outcome
		var completed bool
		select {
		case result = <-metadata:
			completed = true
		case <-time.After(2 * time.Second):
		}
		// Closing one idle connection makes a slot available on the base.
		require.NoError(t, idle[0].Close())
		if !completed {
			result = <-metadata
		}
		for _, conn := range idle[1:] {
			require.NoError(t, conn.Close())
		}
		stopPolls()
		for range 8 {
			<-pollResults
		}
		afterDB := hubStats()
		line, err := json.Marshal(map[string]any{"phase": "listener", "polls_early": early, "metadata_before_release": completed, "metadata_seconds": result.elapsed.Seconds(), "metadata_status": result.status, "hub_cpu_seconds": (hubCPU() - beforeCPU).Seconds(), "hub_peak_rss_mib": hubPeakRSS() >> 20, "hub_db_pool_wait_count": afterDB.WaitCount - beforeDB.WaitCount, "hub_db_pool_wait_seconds": (afterDB.WaitDuration - beforeDB.WaitDuration).Seconds(), "hub_sqlite_write_wait_count": afterDB.WriteWaitCount - beforeDB.WriteWaitCount, "hub_sqlite_write_wait_seconds": afterDB.WriteWaitSeconds - beforeDB.WriteWaitSeconds})
		require.NoError(t, err)
		t.Logf("REPLAY %s", line)
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.status)
		if os.Getenv("KATA_FEDERATION_REPLAY_EXPECT_16") == "1" {
			require.True(t, completed, "metadata must complete before a connection is released")
		}
	}

	// Prepare bodies before starting the concurrent member loops.
	syncWant := map[int64]int{}
	syncBodies := map[*member][][]byte{}
	for i := range members {
		m := &members[i]
		for _, e := range append([]enrollment{m.shared}, m.own...) {
			syncBodies[m] = append(syncBodies[m], pushBody(e, m.spokeUID, ""))
			syncWant[e.project.ID]++
		}
	}
	phase("sync", syncWant, func(launch func(string, enrollment, string, string, []byte), sequence func(func()), record func(string, enrollment, string, string, []byte)) time.Time {
		for i := range members {
			m := &members[i]
			for _, e := range append([]enrollment{m.shared}, m.own...) {
				launch("poll", e, http.MethodGet, fmt.Sprintf("%s/federation/events?after_id=%d&wait=%d", projectPath(e.project.ID), cursor, replayPollWait), nil)
			}
		}
		time.Sleep(2 * time.Second) //nolint:kennlint // sleeptest: allow held polls to reach the listener
		settled := time.Now()
		for i := range members {
			m := &members[i]
			sequence(func() {
				for j, e := range append([]enrollment{m.shared}, m.own...) {
					record("push", e, http.MethodPost, projectPath(e.project.ID)+"/federation/events:ingest", syncBodies[m][j])
				}
			})
		}
		return settled
	})
	if os.Getenv("KATA_FEDERATION_REPLAY_LARGE") != "1" {
		return
	}
	// Optional stress phase: four concurrent large signed pushes exceed the
	// retained two-push gate and can fail while a client is still uploading.
	large := map[int64]int{}
	largeBodies := make([][]byte, replayLargePushes)
	for i := range largeBodies {
		e := members[i].own[0]
		largeBodies[i] = pushBody(e, members[i].spokeUID, strings.Repeat("x", replayLargePushBodySize))
		large[e.project.ID]++
	}
	phase("large-push", large, func(launch func(kind string, e enrollment, method, path string, body []byte), _ func(func()), _ func(string, enrollment, string, string, []byte)) time.Time {
		for i, body := range largeBodies {
			launch("large-push", members[i].own[0], http.MethodPost, projectPath(members[i].own[0].project.ID)+"/federation/events:ingest", body)
		}
		return time.Now()
	})
}
