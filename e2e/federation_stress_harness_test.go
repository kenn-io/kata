//go:build federation_stress && !windows

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	gitcmd "go.kenn.io/kit/git/cmd"
	"pgregory.net/rapid"
)

const federationStressPullInterval = 25 * time.Millisecond

type federationStressTB interface {
	Helper()
	Name() string
	Context() context.Context
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	FailNow()
	Failed() bool
	Cleanup(func())
}

// stressTBRecorder records assertion failures instead of aborting, so a test
// can assert that a fixture-level check actually ran against a given node.
// FailNow is deliberately inert: the caller controls the scenario and wants
// every check in the loop to execute.
type stressTBRecorder struct {
	federationStressTB
	failures []string
}

func (r *stressTBRecorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *stressTBRecorder) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *stressTBRecorder) FailNow() {}

func (r *stressTBRecorder) Failed() bool { return len(r.failures) > 0 }

type federationStressFixture struct {
	bin           string
	hub           *federationStressNode
	spokes        []*federationStressNode
	hubIssue      createdIssue
	meta          api.ProjectFederationBody
	replayAfterID int64
	opSeq         int
}

// federationStressNode is one daemon in the fixture. Hub and spoke are the
// same shape: a node knows its own name, the project id it serves, and
// whether its process is up, so no caller has to know a node's role to
// address it. replica is zero for the hub.
type federationStressNode struct {
	name      string
	dirs      e2eDirs
	url       string
	http      *http.Client
	db        *sqlitestore.Store
	stderr    *safeBuffer
	cmd       *exec.Cmd
	projectID int64
	running   bool
	cred      config.FederationCredential
	replica   api.CreateFederationReplicaBody
}

type federationStressIssue struct {
	ID      int64
	UID     string
	ShortID string
	Deleted bool
}

func newFederationStressFixture(t federationStressTB, spokeCount int) *federationStressFixture {
	t.Helper()
	require.Positive(t, spokeCount)

	bin := buildFederationStressKataBinary(t)
	fx := &federationStressFixture{
		bin:    bin,
		hub:    startFederationStressHub(t, bin),
		spokes: make([]*federationStressNode, 0, spokeCount),
	}
	for i := 0; i < spokeCount; i++ {
		fx.spokes = append(fx.spokes, startFederationStressSpoke(t, bin, i))
	}
	return fx
}

// nodes returns every node in the fixture, hub first. Assertions that hold
// for hub and spokes alike iterate this instead of writing a hub arm and a
// spoke arm that can drift apart.
func (fx *federationStressFixture) nodes() []*federationStressNode {
	return append([]*federationStressNode{fx.hub}, fx.spokes...)
}

func (fx *federationStressFixture) enableProject(t federationStressTB, name string) {
	t.Helper()

	var initBody struct {
		Project api.ProjectOut `json:"project"`
	}
	stressDecodePOST(t, fx.hub.http, fx.hub.url+"/api/v1/projects",
		map[string]any{"name": name, "actor": "user-a"}, &initBody)
	fx.hub.projectID = initBody.Project.ID

	fx.hubIssue = stressCreateIssue(t, fx.hub.http,
		fx.hub.url+"/api/v1/projects/"+strconv.FormatInt(fx.hub.projectID, 10)+"/issues",
		map[string]any{
			"actor": "agent",
			"title": "stress baseline",
			"body":  "replicated through the real-daemon stress fixture",
		})

	stressDecodePOST(t, fx.hub.http,
		fx.hub.url+"/api/v1/projects/"+strconv.FormatInt(fx.hub.projectID, 10)+"/federation/enable",
		map[string]any{"actor": "agent"}, &fx.meta)
	fx.replayAfterID = fx.meta.ReplayHorizonEventID - 1

	for _, spoke := range fx.spokes {
		fx.enrollSpoke(t, spoke)
	}
}

func (fx *federationStressFixture) waitForAllSpokes(t *testing.T) {
	t.Helper()
	require.NotEmpty(t, fx.hubIssue.UID, "enableProject must create the baseline issue before waiting")
	fx.waitForConvergence(t)
	for _, spoke := range fx.spokes {
		waitForFederatedIssue(t, spoke.db, fx.hubIssue.UID, spoke.stderr)
	}
}

func (fx *federationStressFixture) assertAllFoldedProjectionsMatch(t federationStressTB) {
	t.Helper()
	for _, spoke := range fx.spokes {
		if !spoke.running {
			continue
		}
		fx.waitForFoldedProjectionMatch(t, spoke)
	}
}

// A node's SQLite file is readable while its daemon is down, so the
// duplicate-claim invariant is checked for every node unconditionally.
func (fx *federationStressFixture) assertNoDuplicateLiveClaims(t federationStressTB) {
	t.Helper()
	for _, node := range fx.nodes() {
		assertNoDuplicateLiveClaimsOnNode(t, node)
	}
}

func (fx *federationStressFixture) waitForConvergence(t federationStressTB) {
	t.Helper()
	fx.assertNoPendingPushBacklogEventually(t)
	fx.assertAllFoldedProjectionsMatch(t)
	fx.assertDaemonStderrClean(t)
}

func (fx *federationStressFixture) assertNoPendingPushBacklogEventually(t federationStressTB) {
	t.Helper()
	timeout := 10 * time.Second
	if pullWindow := 5 * federationStressPullInterval; pullWindow > timeout {
		timeout = pullWindow
	}
	deadline := time.Now().Add(timeout)
	var last []string
	for time.Now().Before(deadline) {
		last = fx.pendingPushBacklogs(t)
		if len(last) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Empty(t, last, "pending federation push backlog did not drain")
}

func (fx *federationStressFixture) enrollSpoke(t federationStressTB, spoke *federationStressNode) {
	t.Helper()
	token := "stress-" + spoke.name + "-token"
	var inst struct {
		InstanceUID string `json:"instance_uid"`
	}
	stressDecodeGET(t, spoke.http, spoke.url+"/api/v1/instance", &inst)
	var created api.FederationEnrollmentOut
	stressDecodePOST(t, fx.hub.http, fx.hub.url+"/api/v1/federation/enrollments", map[string]any{
		"token":              token,
		"spoke_instance_uid": inst.InstanceUID,
		"project_id":         fx.hub.projectID,
		"capabilities":       "pull,push,claim",
		"actor":              "stress",
	}, &created)

	var replica api.CreateFederationReplicaBody
	stressDecodePOST(t, spoke.http, spoke.url+"/api/v1/federation/replicas", map[string]any{
		"hub_url":                   fx.hub.url,
		"hub_project_id":            fx.hub.projectID,
		"hub_project_uid":           fx.meta.ProjectUID,
		"project_name":              fx.meta.ProjectName,
		"replay_horizon_event_id":   fx.meta.ReplayHorizonEventID,
		"baseline_through_event_id": fx.meta.BaselineThroughEventID,
		"token":                     created.Token,
		"capabilities":              "pull,push,claim",
		"actor":                     "stress",
		"push_enabled":              true,
	}, &replica)
	require.True(t, replica.Binding.PushEnabled)
	spoke.replica = replica
	spoke.projectID = replica.Project.ID
	spoke.cred = config.FederationCredential{
		HubURL:       fx.hub.url,
		HubProjectID: fx.hub.projectID,
		Token:        created.Token,
		Capabilities: "claim,pull,push",
		Actor:        "stress",
	}
}

func (fx *federationStressFixture) pendingPushBacklogs(t federationStressTB) []string {
	t.Helper()
	ctx := context.Background()
	var pending []string
	for _, spoke := range fx.spokes {
		if !spoke.running {
			continue
		}
		binding, err := spoke.db.FederationBindingByProject(ctx, spoke.projectID)
		require.NoError(t, err)
		events, err := spoke.db.PendingFederationPushEvents(ctx,
			spoke.projectID,
			spoke.db.InstanceUID(),
			binding.PushCursorEventID,
			1000,
		)
		require.NoError(t, err)
		if len(events) > 0 {
			pending = append(pending, fmt.Sprintf("%s:%d", spoke.name, len(events)))
		}
	}
	return pending
}

func (fx *federationStressFixture) applyRandomOperation(t *rapid.T) {
	t.Helper()
	fx.opSeq++
	switch rapid.IntRange(0, 5).Draw(t, "operation") {
	case 0:
		fx.createIssueOnNode(t, fx.hub, "hub-create")
	case 1:
		if spoke, ok := fx.drawRunningSpoke(t, "create-spoke"); ok {
			fx.createIssueOnNode(t, spoke, "spoke-create")
		}
	case 2:
		fx.acquireSpokeHardClaim(t)
	case 3:
		fx.releaseSpokeClaim(t)
	case 4:
		fx.editClaimedIssue(t)
	case 5:
		fx.commentClaimedIssue(t)
	}
}

func (fx *federationStressFixture) acquireSpokeHardClaim(t *rapid.T) {
	t.Helper()
	spoke, issue, ok := fx.drawSpokeLiveIssue(t, "claim")
	if !ok {
		return
	}
	actor := fx.drawActor(t, "claim-actor")
	_ = fx.acquireClaim(t, spoke, issue.ShortID, actor)
}

func (fx *federationStressFixture) releaseSpokeClaim(t *rapid.T) {
	t.Helper()
	spoke, ok := fx.drawRunningSpoke(t, "release-spoke")
	if !ok {
		return
	}
	claim, ok := fx.drawLiveClaim(t, spoke, "release-claim")
	if !ok {
		return
	}
	fx.postClaimAction(t, spoke, claim.IssueRef, "release", map[string]any{
		"holder": claim.Holder,
		"reason": "stress release",
	})
}

func (fx *federationStressFixture) editClaimedIssue(t *rapid.T) {
	t.Helper()
	spoke, issue, ok := fx.drawSpokeLiveIssue(t, "edit")
	if !ok {
		return
	}
	actor := fx.drawActor(t, "edit-actor")
	if !fx.acquireClaim(t, spoke, issue.ShortID, actor) {
		return
	}
	switch rapid.IntRange(0, 3).Draw(t, "edit-kind") {
	case 0:
		fx.patchIssue(t, spoke, issue.ShortID, map[string]any{
			"actor": actor,
			"title": fmt.Sprintf("stress title %03d", fx.opSeq),
			"body":  fmt.Sprintf("stress body %03d", fx.opSeq),
		})
	case 1:
		priority := int64(rapid.IntRange(0, 3).Draw(t, "priority"))
		fx.patchIssue(t, spoke, issue.ShortID, map[string]any{
			"actor":        actor,
			"set_priority": priority,
		})
	case 2:
		fx.patchIssue(t, spoke, issue.ShortID, map[string]any{
			"actor":          actor,
			"clear_priority": true,
		})
	case 3:
		fx.addLabel(t, spoke, issue.ShortID, actor,
			fmt.Sprintf("stress:%d", rapid.IntRange(0, 5).Draw(t, "label")))
	}
}

func (fx *federationStressFixture) commentClaimedIssue(t *rapid.T) {
	t.Helper()
	actor := fx.drawActor(t, "comment-actor")
	if rapid.Bool().Draw(t, "comment-on-hub") {
		issue, ok := fx.drawIssue(t, fx.hub, false, "hub-comment")
		if !ok {
			return
		}
		if !fx.acquireClaim(t, fx.hub, issue.ShortID, actor) {
			return
		}
		fx.commentOnNode(t, fx.hub, issue.ShortID, actor)
		return
	}
	spoke, issue, ok := fx.drawSpokeLiveIssue(t, "spoke-comment")
	if !ok {
		return
	}
	if !fx.acquireClaim(t, spoke, issue.ShortID, actor) {
		return
	}
	fx.commentOnNode(t, spoke, issue.ShortID, actor)
}

func (fx *federationStressFixture) createIssueOnNode(t federationStressTB, node *federationStressNode, prefix string) createdIssue {
	t.Helper()
	return stressCreateIssue(t, node.http,
		node.url+"/api/v1/projects/"+strconv.FormatInt(node.projectID, 10)+"/issues",
		map[string]any{
			"actor":     "stress",
			"title":     fmt.Sprintf("%s %03d", prefix, fx.opSeq),
			"body":      "generated by randomized federation stress workload",
			"labels":    []string{"stress"},
			"force_new": true,
		})
}

func (fx *federationStressFixture) acquireClaim(t federationStressTB, node *federationStressNode, ref, actor string) bool {
	t.Helper()
	var out api.ClaimActionResponseBody
	ok := fx.postClaimAction(t, node, ref, "acquire", map[string]any{
		"holder":      actor,
		"client_kind": "stress",
		"claim_kind":  "hard",
		"purpose":     "edit",
	}, &out)
	return ok && out.Granted && !out.Pending
}

func (fx *federationStressFixture) postClaimAction(
	t federationStressTB,
	node *federationStressNode,
	ref string,
	action string,
	body map[string]any,
	out ...*api.ClaimActionResponseBody,
) bool {
	t.Helper()
	var parsed api.ClaimActionResponseBody
	status, raw := stressDoJSON(t, node.http, http.MethodPost,
		node.url+"/api/v1/projects/"+strconv.FormatInt(node.projectID, 10)+
			"/issues/"+url.PathEscape(ref)+"/lease/actions/"+action,
		nil, body, &parsed)
	if status == http.StatusConflict {
		return false
	}
	require.Equalf(t, http.StatusOK, status, "%s lease action body: %s", action, raw)
	if len(out) > 0 {
		*out[0] = parsed
	}
	return true
}

func (fx *federationStressFixture) patchIssue(
	t federationStressTB,
	node *federationStressNode,
	ref string,
	body map[string]any,
) {
	t.Helper()
	status, raw := stressDoJSON(t, node.http, http.MethodPatch,
		node.url+"/api/v1/projects/"+strconv.FormatInt(node.projectID, 10)+"/issues/"+url.PathEscape(ref),
		nil, body, nil)
	require.Equalf(t, http.StatusOK, status, "patch issue body: %s", raw)
}

func (fx *federationStressFixture) addLabel(
	t federationStressTB,
	node *federationStressNode,
	ref string,
	actor string,
	label string,
) {
	t.Helper()
	status, raw := stressDoJSON(t, node.http, http.MethodPost,
		node.url+"/api/v1/projects/"+strconv.FormatInt(node.projectID, 10)+"/issues/"+url.PathEscape(ref)+"/labels",
		nil, map[string]any{"actor": actor, "label": label}, nil)
	require.Equalf(t, http.StatusOK, status, "add label body: %s", raw)
}

func (fx *federationStressFixture) commentOnNode(
	t federationStressTB,
	node *federationStressNode,
	ref string,
	actor string,
) {
	t.Helper()
	status, raw := stressDoJSON(t, node.http, http.MethodPost,
		node.url+"/api/v1/projects/"+strconv.FormatInt(node.projectID, 10)+"/issues/"+url.PathEscape(ref)+"/comments",
		nil, map[string]any{
			"actor": actor,
			"body":  fmt.Sprintf("claimed stress comment %03d", fx.opSeq),
		}, nil)
	require.Equalf(t, http.StatusOK, status, "comment body: %s", raw)
}

func (fx *federationStressFixture) drawActor(t *rapid.T, label string) string {
	t.Helper()
	actors := []string{"alice", "bob", "charlie", "dana"}
	return actors[rapid.IntRange(0, len(actors)-1).Draw(t, label)]
}

func (fx *federationStressFixture) drawRunningSpoke(t *rapid.T, label string) (*federationStressNode, bool) {
	t.Helper()
	var running []*federationStressNode
	for _, spoke := range fx.spokes {
		if spoke.running {
			running = append(running, spoke)
		}
	}
	if len(running) == 0 {
		return nil, false
	}
	return running[rapid.IntRange(0, len(running)-1).Draw(t, label)], true
}

func (fx *federationStressFixture) drawSpokeLiveIssue(t *rapid.T, label string) (*federationStressNode, federationStressIssue, bool) {
	t.Helper()
	spoke, ok := fx.drawRunningSpoke(t, label+"-spoke")
	if !ok {
		return nil, federationStressIssue{}, false
	}
	issues := fx.issuesOnNode(t, spoke, false)
	hubKnown := issues[:0]
	for _, issue := range issues {
		if fx.issueLiveOnHub(t, issue.UID) {
			hubKnown = append(hubKnown, issue)
		}
	}
	if len(hubKnown) == 0 {
		return nil, federationStressIssue{}, false
	}
	issue := hubKnown[rapid.IntRange(0, len(hubKnown)-1).Draw(t, label+"-issue")]
	return spoke, issue, true
}

func (fx *federationStressFixture) drawIssue(
	t *rapid.T,
	node *federationStressNode,
	includeDeleted bool,
	label string,
) (federationStressIssue, bool) {
	t.Helper()
	issues := fx.issuesOnNode(t, node, includeDeleted)
	if len(issues) == 0 {
		return federationStressIssue{}, false
	}
	return issues[rapid.IntRange(0, len(issues)-1).Draw(t, label)], true
}

type federationStressLiveClaim struct {
	IssueRef string
	Holder   string
}

func (fx *federationStressFixture) drawLiveClaim(
	t *rapid.T,
	node *federationStressNode,
	label string,
) (federationStressLiveClaim, bool) {
	t.Helper()
	rows, err := node.db.QueryContext(context.Background(), `
		SELECT i.short_id, c.holder
		  FROM issue_claims c
		  JOIN issues i ON i.uid = c.issue_uid
		 WHERE c.project_id = ?
		   AND c.released_at IS NULL
		   AND i.deleted_at IS NULL
		 ORDER BY c.id`, node.projectID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var claims []federationStressLiveClaim
	for rows.Next() {
		var claim federationStressLiveClaim
		require.NoError(t, rows.Scan(&claim.IssueRef, &claim.Holder))
		claims = append(claims, claim)
	}
	require.NoError(t, rows.Err())
	if len(claims) == 0 {
		return federationStressLiveClaim{}, false
	}
	return claims[rapid.IntRange(0, len(claims)-1).Draw(t, label)], true
}

func (fx *federationStressFixture) issuesOnNode(
	t federationStressTB,
	node *federationStressNode,
	includeDeleted bool,
) []federationStressIssue {
	t.Helper()
	q := `SELECT id, uid, short_id, deleted_at IS NOT NULL FROM issues WHERE project_id = ?`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	q += ` ORDER BY id`
	rows, err := node.db.QueryContext(context.Background(), q, node.projectID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var issues []federationStressIssue
	for rows.Next() {
		var issue federationStressIssue
		require.NoError(t, rows.Scan(&issue.ID, &issue.UID, &issue.ShortID, &issue.Deleted))
		issues = append(issues, issue)
	}
	require.NoError(t, rows.Err())
	return issues
}

func (fx *federationStressFixture) issueLiveOnHub(t federationStressTB, issueUID string) bool {
	t.Helper()
	_, err := fx.hub.db.IssueByUID(context.Background(), issueUID, db.IncludeDeletedNo)
	return err == nil
}

func (fx *federationStressFixture) waitForFoldedProjectionMatch(t federationStressTB, spoke *federationStressNode) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastHub, lastSpoke db.FoldProjection
	for time.Now().Before(deadline) {
		var err error
		lastHub, lastSpoke, err = stressFoldedProjections(t, fx.hub.db, spoke.db,
			fx.hub.projectID, spoke.projectID, fx.replayAfterID)
		require.NoError(t, err)
		if assert.ObjectsAreEqual(lastHub.Issues, lastSpoke.Issues) &&
			assert.ObjectsAreEqual(lastHub.Comments, lastSpoke.Comments) &&
			assert.ObjectsAreEqual(lastHub.Labels, lastSpoke.Labels) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Equal(t, lastHub.Issues, lastSpoke.Issues)
	assert.Equal(t, lastHub.Comments, lastSpoke.Comments)
	assert.Equal(t, lastHub.Labels, lastSpoke.Labels)
	t.Fatalf("folded projections did not converge for %s\ndaemon stderr: %s", spoke.name, spoke.stderr.String())
}

func stressFoldedProjections(
	t federationStressTB,
	hub *sqlitestore.Store,
	spoke *sqlitestore.Store,
	hubProjectID int64,
	spokeProjectID int64,
	hubAfterID int64,
) (db.FoldProjection, db.FoldProjection, error) {
	t.Helper()
	ctx := context.Background()
	hubEvents, err := hub.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: hubProjectID,
		AfterID:   hubAfterID,
		Limit:     1000,
	})
	if err != nil {
		return db.FoldProjection{}, db.FoldProjection{}, err
	}
	spokeEvents, err := spoke.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: spokeProjectID,
		Limit:     1000,
	})
	if err != nil {
		return db.FoldProjection{}, db.FoldProjection{}, err
	}
	return db.FoldEvents(foldEvents(hubEvents)), db.FoldEvents(foldEvents(spokeEvents)), nil
}

// A daemon's stderr buffer outlives its process, so every node is inspected
// whether or not it is running. `running` gates only the loops that need a
// live daemon to make progress (assertAllFoldedProjectionsMatch,
// pendingPushBacklogs).
func (fx *federationStressFixture) assertDaemonStderrClean(t federationStressTB) {
	t.Helper()
	for _, node := range fx.nodes() {
		fx.assertNodeStderrClean(t, node)
	}
}

func (fx *federationStressFixture) assertNodeStderrClean(t federationStressTB, node *federationStressNode) {
	t.Helper()
	log := strings.ToLower(node.stderr.String())
	for _, bad := range []string{
		"panic",
		"fatal",
		"database is locked",
		"unauthorized",
		"forbidden",
		"auth failed",
		"invalid token",
	} {
		if strings.Contains(log, bad) {
			t.Fatalf("%s daemon stderr contains %q:\n%s", node.name, bad, node.stderr.String())
		}
	}
}

func startFederationStressHub(t federationStressTB, bin string) *federationStressNode {
	t.Helper()
	dirs := newFederationStressDirs(t)
	port := federationStressFreeTCPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	stderr := &safeBuffer{}
	cmd := startFederationStressTCPDaemon(t, bin, dirs, stderr, addr)
	url := "http://" + addr
	stressWaitForPing(t, url, 5*time.Second)
	store, err := sqlitestore.Open(context.Background(), dirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return &federationStressNode{
		name:    "hub",
		dirs:    dirs,
		url:     url,
		http:    &http.Client{Timeout: 5 * time.Second},
		db:      store,
		stderr:  stderr,
		cmd:     cmd,
		running: true,
	}
}

func startFederationStressTCPDaemon(
	t federationStressTB,
	bin string,
	dirs e2eDirs,
	stderr *safeBuffer,
	addr string,
) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "daemon", "start", "--foreground", "--listen", addr) //nolint:gosec // test-built binary and loopback address
	cmd.Env = append(dirs.env(), federationStressPullIntervalEnv())
	cmd.Dir = dirs.repoDir
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("hub daemon stderr:\n%s", stderr.String())
		}
	})
	t.Cleanup(func() { stopDaemon(cmd) })
	return cmd
}

func startFederationStressSpoke(t federationStressTB, bin string, index int) *federationStressNode {
	t.Helper()
	dirs := newFederationStressDirs(t)
	stderr := &safeBuffer{}
	cmd := startFederationStressUnixDaemon(t, bin, dirs, stderr)
	url, client := stressConnectDaemon(t, dirs, stderr)
	store, err := sqlitestore.Open(context.Background(), dirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return &federationStressNode{
		name:    fmt.Sprintf("spoke-%d", index),
		dirs:    dirs,
		url:     url,
		http:    client,
		db:      store,
		stderr:  stderr,
		cmd:     cmd,
		running: true,
	}
}

func startFederationStressUnixDaemon(t federationStressTB, bin string, dirs e2eDirs, stderr *safeBuffer) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "daemon", "start", "--foreground") //nolint:gosec // test-built binary
	cmd.Env = append(dirs.env(), federationStressPullIntervalEnv())
	cmd.Dir = dirs.repoDir
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("spoke daemon stderr:\n%s", stderr.String())
		}
	})
	t.Cleanup(func() { stopDaemon(cmd) })
	return cmd
}

func assertNoDuplicateLiveClaimsOnNode(t federationStressTB, node *federationStressNode) {
	t.Helper()
	rows, err := node.db.QueryContext(context.Background(), `
		SELECT issue_uid, COUNT(*)
		  FROM issue_claims
		 WHERE released_at IS NULL
		 GROUP BY issue_uid
		HAVING COUNT(*) > 1`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var issueUID string
		var count int
		require.NoError(t, rows.Scan(&issueUID, &count))
		assert.LessOrEqualf(t, count, 1, "%s has duplicate live claims for issue %s", node.name, issueUID)
	}
	require.NoError(t, rows.Err())
}

func buildFederationStressKataBinary(t federationStressTB) string {
	t.Helper()
	federationStressBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kata-federation-stress-bin-")
		if err != nil {
			federationStressBuildErr = err
			return
		}
		bin := filepath.Join(dir, "kata")
		build := exec.Command("go", "build", "-tags", "kit_posthog_disabled", "-o", bin, "go.kenn.io/kata/cmd/kata") //nolint:gosec // fixed args, test-only
		var stderr bytes.Buffer
		build.Stderr = &stderr
		if err := build.Run(); err != nil {
			federationStressBuildErr = fmt.Errorf("go build kata: %w: %s", err, stderr.String())
			return
		}
		federationStressBuildBin = bin
	})
	require.NoError(t, federationStressBuildErr)
	require.NotEmpty(t, federationStressBuildBin)
	return federationStressBuildBin
}

var (
	federationStressBuildOnce sync.Once
	federationStressBuildBin  string
	federationStressBuildErr  error
)

func federationStressPullIntervalEnv() string {
	return "KATA_FEDERATION_PULL_INTERVAL_MS=" + strconv.FormatInt(federationStressPullInterval.Milliseconds(), 10)
}

func newFederationStressDirs(t federationStressTB) e2eDirs {
	t.Helper()
	home, err := os.MkdirTemp("", "kata-federation-stress-home-")
	require.NoError(t, err)
	repoDir, err := os.MkdirTemp("", "kata-federation-stress-repo-")
	require.NoError(t, err)
	xdg, err := os.MkdirTemp("/tmp", "kata-e2e-xdg-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
	t.Cleanup(func() { _ = os.RemoveAll(xdg) })
	runFederationStressGit(t, repoDir, "init", "--quiet")
	runFederationStressGit(t, repoDir, "remote", "add", "origin", "https://github.com/wesm/kata-e2e.git")
	return e2eDirs{
		home:    home,
		repoDir: repoDir,
		dbPath:  filepath.Join(home, "kata.db"),
		marker:  filepath.Join(home, "marker.txt"),
		script:  filepath.Join(home, "hook.sh"),
		xdgDir:  xdg,
	}
}

func runFederationStressGit(t federationStressTB, dir string, args ...string) {
	t.Helper()
	stdout, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoErrorf(t, err, "git %v: %s%s", args, stdout, stderr)
}

func stressConnectDaemon(t federationStressTB, d e2eDirs, daemonStderr *safeBuffer) (string, *http.Client) {
	t.Helper()
	runtimeDir := filepath.Join(d.home, "runtime", config.DBHash(d.dbPath))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sockPath, ok := readDaemonSocketPath(runtimeDir)
		if ok {
			client := &http.Client{
				Transport: newUnixTransport(sockPath),
				Timeout:   5 * time.Second,
			}
			if pingDaemon(client) {
				return "http://kata.invalid", client
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon never advertised a unix socket in %s\ndaemon stderr: %s",
		runtimeDir, daemonStderr.String())
	return "", nil
}

func stressDecodeGET(t federationStressTB, client *http.Client, url string, out any) {
	t.Helper()
	resp, err := client.Get(url) //nolint:gosec,noctx // test loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

func stressDecodePOST(t federationStressTB, client *http.Client, url string, body, out any) {
	t.Helper()
	status, raw := stressDoJSON(t, client, http.MethodPost, url, nil, body, out)
	require.Equalf(t, http.StatusOK, status, "POST %s body: %s", url, raw)
}

func stressCreateIssue(t federationStressTB, client *http.Client, url string, body any) createdIssue {
	t.Helper()
	status, raw := stressDoJSON(t, client, http.MethodPost, url, nil, body, nil)
	require.Equalf(t, http.StatusOK, status, "create issue body: %s", raw)
	return stressDecodeMutationIssue(t, raw)
}

func stressDoJSON(
	t federationStressTB,
	client *http.Client,
	method string,
	url string,
	headers map[string]string,
	body any,
	out any,
) (int, []byte) {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for {
		req, err := http.NewRequestWithContext(context.Background(), method, url, bytes.NewReader(bs))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req) //nolint:gosec // test loopback
		require.NoError(t, err)
		raw, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		if resp.StatusCode == http.StatusInternalServerError &&
			isSQLiteBusyMessage(string(raw)) &&
			time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		if out != nil && resp.StatusCode == http.StatusOK {
			require.NoErrorf(t, json.Unmarshal(raw, out), "decode response body: %s", raw)
		}
		return resp.StatusCode, raw
	}
}

func stressDecodeMutationIssue(t federationStressTB, body []byte) createdIssue {
	t.Helper()
	var parsed struct {
		Issue struct {
			ShortID string `json:"short_id"`
			UID     string `json:"uid"`
		} `json:"issue"`
	}
	require.NoErrorf(t, json.Unmarshal(body, &parsed), "decode mutation body: %s", body)
	require.NotEmptyf(t, parsed.Issue.ShortID, "short_id missing from response: %s", body)
	require.NotEmptyf(t, parsed.Issue.UID, "uid missing from response: %s", body)
	return createdIssue{ShortID: parsed.Issue.ShortID, UID: parsed.Issue.UID}
}

func federationStressFreeTCPPort(t federationStressTB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func stressWaitForPing(t federationStressTB, base string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 250 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/api/v1/ping") //nolint:noctx
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var info struct {
					OK bool `json:"ok"`
				}
				if json.Unmarshal(body, &info) == nil && info.OK {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon at %s did not answer /api/v1/ping within %s", base, timeout)
}
