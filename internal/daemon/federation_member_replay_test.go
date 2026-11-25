//go:build linux

package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/testenv"
)

// Synthetic replay shape: 36 members on one project. Every member holds polls
// and a watch, member 0 holds more polls than one enrollment once could, and
// the hub holds more polls in total than the old process bound.
const (
	replayMembers           = 36
	replayPollsPerMember    = 8
	replayPollsFirstMember  = 40
	replayPollWait          = 30
	replayLargePushes       = 4
	replayLargePushBodySize = 60 << 20
)

// TestFederationMemberReplay drives a live federation listener the way member
// spokes do and logs a REPLAY line of counters for each phase. It compiles on
// main and on this branch so the same workload compares the two. It waits out
// the signing quarantine in real time, so it runs only when
// KATA_FEDERATION_REPLAY=1.
func TestFederationMemberReplay(t *testing.T) {
	if os.Getenv("KATA_FEDERATION_REPLAY") != "1" {
		t.Skip("set KATA_FEDERATION_REPLAY=1 to run the paired federation replay")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	env := testenv.New(t)
	project := createFederatedHubProject(t, env, "hub-project")
	type member struct {
		token, spokeUID string
		source          signingpolicy.Source
	}
	members := make([]member, replayMembers)
	keys := make([]signingpolicy.Key, replayMembers)
	for i := range members {
		keyEnv := fmt.Sprintf("TEST_REPLAY_MEMBER_KEY_%d", i)
		t.Setenv(keyEnv, fmt.Sprintf("%064d", i+1))
		members[i] = member{
			token:    fmt.Sprintf("replay-member-%d", i),
			spokeUID: fmt.Sprintf("01HZNQ7VFPK1XGD8R5MAB%05d", i),
			source:   signingpolicy.Source{KeyID: fmt.Sprintf("member-%d", i), KeyEnv: keyEnv},
		}
		enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: members[i].token, SpokeInstanceUID: members[i].spokeUID, ProjectID: &project.ID, Capabilities: "pull,push", Actor: "tester"})
		require.NoError(t, err)
		keys[i] = signingpolicy.Key{Source: members[i].source, EnrollmentID: enrollment.Enrollment.ID}
	}
	state := filepath.Join(t.TempDir(), "replay.state")
	require.NoError(t, federationsigning.InitializeReplayState(state))
	verifier, err := federationsigning.NewVerifier("https://hub.example", keys, state)
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifier.Close() })
	time.Sleep(federationsigning.Quarantine) //nolint:kennlint // sleeptest: the verifier accepts signatures only after its real-time quarantine
	srv := daemon.NewServer(daemon.ServerConfig{DB: env.DB, FederationSigning: verifier})
	t.Cleanup(func() { _ = srv.Close() })
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- srv.ServeListeners(ctx, daemon.ListenerBinding{Listener: listener, Policy: daemon.ListenerPolicy{Kind: daemon.ListenerFederation}})
	}()
	defer func() { cancel(); <-served }()
	// Each request opens its own connection, as separate member hosts do.
	client := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}
	type outcome struct {
		kind    string
		status  int
		events  int
		started time.Time
		elapsed time.Duration
		err     error
		detail  string
	}
	send := func(m member, method, path string, body []byte) outcome {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, "https://hub.example"+path, reader)
		if err != nil {
			return outcome{err: err}
		}
		req.Header.Set("Authorization", "Bearer "+m.token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := federationsigning.Sign(req, m.source); err != nil {
			return outcome{err: err}
		}
		req.URL.Scheme, req.URL.Host, req.Host = "http", listener.Addr().String(), "hub.example"
		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return outcome{started: started, elapsed: time.Since(started), err: err}
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		out := outcome{status: resp.StatusCode, started: started, elapsed: time.Since(started), err: err}
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
	pushBody := func(i int, issueBody string) []byte {
		issueUID := fmt.Sprintf("01HZNQ7VFPK1XGD8R5MAC%05d", i)
		shortID := strings.ToLower(issueUID[len(issueUID)-5:])
		ev := federationRemoteIssueCreatedEvent(t, project, members[i%replayMembers].spokeUID)
		ev.EventUID = fmt.Sprintf("01HZNQ7VFPK1XGD8R5MAD%05d", i)
		ev.IssueUID = &issueUID
		ev.Payload = fmt.Appendf(nil, `{"uid":%q,"short_id":%q,"title":"member work","body":%q,"author":"tester","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`, issueUID, shortID, issueBody)
		ev.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: ev.EventUID, OriginInstanceUID: ev.OriginInstanceUID, ProjectUID: ev.ProjectUID, ProjectName: ev.ProjectName, IssueUID: ev.IssueUID, Type: ev.Type, Actor: ev.Actor, HLCPhysicalMS: ev.HLCPhysicalMS, HLCCounter: ev.HLCCounter, CreatedAt: ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), Payload: ev.Payload})
		require.NoError(t, err)
		body, err := json.Marshal(federationIngestBody(federationIngestEnvelope(t, int64(i+1), ev)))
		require.NoError(t, err)
		return body
	}
	prefix := projectPath(project.ID) + "/federation"
	first := send(members[0], http.MethodGet, prefix+"/events?after_id=0", nil)
	require.NoError(t, first.err)
	require.Equal(t, http.StatusOK, first.status)
	cursor, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	type summary struct {
		Sent, OK, Refused429, OtherStatus, Errors int
		EarlyAnswers, Woken, Expired              int     `json:",omitempty"`
		P50Seconds, MaxSeconds                    float64 `json:",omitempty"`
		FirstFailure                              string  `json:",omitempty"`
	}
	// phase runs one workload and logs a REPLAY line of its counters. Polls
	// answered empty before settled are early answers: a member would fall back
	// to interval polling.
	phase := func(name string, workload func(launch func(kind string, m member, method, path string, body []byte)) (settled time.Time)) {
		runtime.GC()
		debug.FreeOSMemory()
		var peakHeap uint64
		var peakRSS int64
		var peakDisk int64
		sampling, sampled := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(sampled)
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			for {
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				peakHeap = max(peakHeap, stats.HeapInuse)
				if statm, err := os.ReadFile("/proc/self/statm"); err == nil {
					var size, resident int64
					if _, err := fmt.Sscan(string(statm), &size, &resident); err == nil {
						peakRSS = max(peakRSS, resident*int64(os.Getpagesize()))
					}
				}
				var disk int64
				_ = filepath.WalkDir(tmp, func(_ string, d fs.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						if info, err := d.Info(); err == nil {
							disk += info.Size()
						}
					}
					return nil
				})
				peakDisk = max(peakDisk, disk)
				select {
				case <-sampling:
					return
				case <-tick.C:
				}
			}
		}()
		var usage syscall.Rusage
		require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &usage))
		cpuBefore := time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
		dbBefore := env.DB.Stats()
		issuesBefore, err := env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID, Limit: 1000})
		require.NoError(t, err)
		began := time.Now()
		var mu sync.Mutex
		var outcomes []outcome
		var wg sync.WaitGroup
		settled := workload(func(kind string, m member, method, path string, body []byte) {
			wg.Go(func() {
				out := send(m, method, path, body)
				out.kind = kind
				mu.Lock()
				outcomes = append(outcomes, out)
				mu.Unlock()
			})
		})
		wg.Wait()
		elapsed := time.Since(began)
		close(sampling)
		<-sampled
		require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &usage))
		cpu := time.Duration(usage.Utime.Nano()+usage.Stime.Nano()) - cpuBefore
		dbAfter := env.DB.Stats()
		report := map[string]*summary{}
		latencies := map[string][]time.Duration{}
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
			case out.status == http.StatusTooManyRequests:
				s.Refused429++
			default:
				s.OtherStatus++
			}
			if out.kind == "poll" || out.kind == "watch" {
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
			latencies[out.kind] = append(latencies[out.kind], out.elapsed)
		}
		for kind, values := range latencies {
			slices.Sort(values)
			report[kind].P50Seconds = values[len(values)/2].Seconds()
			report[kind].MaxSeconds = values[len(values)-1].Seconds()
		}
		issues, err := env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: project.ID, Limit: 1000})
		require.NoError(t, err)
		line, err := json.Marshal(map[string]any{
			"phase":           name,
			"requests":        report,
			"issues_landed":   len(issues) - len(issuesBefore),
			"wall_seconds":    elapsed.Seconds(),
			"cpu_seconds":     cpu.Seconds(),
			"peak_rss_mib":    peakRSS >> 20,
			"peak_heap_mib":   peakHeap >> 20,
			"peak_tmp_disk_b": peakDisk,
			"db_wait_count":   dbAfter.WaitCount - dbBefore.WaitCount,
			"db_wait_seconds": (dbAfter.WaitDuration - dbBefore.WaitDuration).Seconds(),
		})
		require.NoError(t, err)
		t.Logf("REPLAY %s", line)
	}

	// Every member holds polls and a watch, then reads metadata and pushes one
	// change while they are held.
	phase("sync", func(launch func(kind string, m member, method, path string, body []byte)) time.Time {
		watch, err := json.Marshal(map[string]any{"after_id": cursor, "projects": []any{}})
		require.NoError(t, err)
		for i, m := range members {
			polls := replayPollsPerMember
			if i == 0 {
				polls = replayPollsFirstMember
			}
			for range polls {
				launch("poll", m, http.MethodGet, fmt.Sprintf("%s/events?after_id=%d&wait=%d", prefix, cursor, replayPollWait), nil)
			}
			launch("watch", m, http.MethodPost, fmt.Sprintf("%s/events:watch?wait=%d", prefix, replayPollWait), watch)
		}
		time.Sleep(2 * time.Second) //nolint:kennlint // sleeptest: polls still open after this real-time settle count as held
		settled := time.Now()
		for i, m := range members {
			launch("metadata", m, http.MethodGet, prefix+"/metadata", nil)
			launch("push", m, http.MethodPost, prefix+"/events:ingest", pushBody(i, ""))
		}
		return settled
	})
	// A few members push large batches at once.
	bodies := make([][]byte, replayLargePushes)
	for i := range bodies {
		bodies[i] = pushBody(replayMembers+i, strings.Repeat("x", replayLargePushBodySize))
	}
	phase("large-push", func(launch func(kind string, m member, method, path string, body []byte)) time.Time {
		for i, body := range bodies {
			launch("large-push", members[i], http.MethodPost, prefix+"/events:ingest", body)
		}
		return time.Now()
	})
}
