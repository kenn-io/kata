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

type federationStressFixture struct {
	bin           string
	hub           federationStressNode
	spokes        []federationStressNode
	hubProject    api.ProjectOut
	hubIssue      createdIssue
	meta          api.ProjectFederationBody
	replayAfterID int64
	opSeq         int
}

type federationStressNode struct {
	dirs    e2eDirs
	url     string
	http    *http.Client
	db      *sqlitestore.Store
	stderr  *safeBuffer
	cmd     *exec.Cmd
	online  bool
	cred    config.FederationCredential
	replica api.CreateFederationReplicaBody
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
		spokes: make([]federationStressNode, 0, spokeCount),
	}
	for i := 0; i < spokeCount; i++ {
		fx.spokes = append(fx.spokes, startFederationStressSpoke(t, bin))
	}
	return fx
}

func (fx *federationStressFixture) enableProject(t federationStressTB, name string) {
	t.Helper()

	var initBody struct {
		Project api.ProjectOut `json:"project"`
	}
	stressDecodePOST(t, fx.hub.http, fx.hub.url+"/api/v1/projects",
		map[string]any{"name": name}, &initBody)
	fx.hubProject = initBody.Project

	fx.hubIssue = stressCreateIssue(t, fx.hub.http,
		fx.hub.url+"/api/v1/projects/"+strconv.FormatInt(fx.hubProject.ID, 10)+"/issues",
		map[string]any{
			"actor": "agent",
			"title": "stress baseline",
			"body":  "replicated through the real-daemon stress fixture",
		})

	stressDecodePOST(t, fx.hub.http,
		fx.hub.url+"/api/v1/projects/"+strconv.FormatInt(fx.hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "agent"}, &fx.meta)
	fx.replayAfterID = fx.meta.ReplayHorizonEventID - 1

	for i := range fx.spokes {
		fx.spokes[i].replica = fx.enrollSpoke(t, i)
	}
}

func (fx *federationStressFixture) waitForAllSpokes(t *testing.T) {
	t.Helper()
	require.NotEmpty(t, fx.hubIssue.UID, "enableProject must create the baseline issue before waiting")
	fx.waitForConvergence(t)
	for i := range fx.spokes {
		waitForFederatedIssue(t, fx.spokes[i].db, fx.hubIssue.UID, fx.spokes[i].stderr)
	}
}

func (fx *federationStressFixture) assertAllFoldedProjectionsMatch(t federationStressTB) {
	t.Helper()
	for i := range fx.spokes {
		if !fx.spokes[i].online {
			continue
		}
		fx.waitForFoldedProjectionMatch(t, i)
	}
}

func (fx *federationStressFixture) assertNoDuplicateLiveClaims(t federationStressTB) {
	t.Helper()
	assertNoDuplicateLiveClaimsOnNode(t, "hub", fx.hub.db)
	for i := range fx.spokes {
		if !fx.spokes[i].online {
			continue
		}
		assertNoDuplicateLiveClaimsOnNode(t, fmt.Sprintf("spoke-%d", i), fx.spokes[i].db)
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

func (fx *federationStressFixture) enrollSpoke(t federationStressTB, i int) api.CreateFederationReplicaBody {
	t.Helper()
	spoke := fx.spokes[i]
	token := fmt.Sprintf("stress-spoke-%d-token", i)
	var inst struct {
		InstanceUID string `json:"instance_uid"`
	}
	stressDecodeGET(t, spoke.http, spoke.url+"/api/v1/instance", &inst)
	var created api.FederationEnrollmentOut
	stressDecodePOST(t, fx.hub.http, fx.hub.url+"/api/v1/federation/enrollments", map[string]any{
		"token":              token,
		"spoke_instance_uid": inst.InstanceUID,
		"project_id":         fx.hubProject.ID,
		"capabilities":       "pull,push,claim",
	}, &created)

	var replica api.CreateFederationReplicaBody
	stressDecodePOST(t, spoke.http, spoke.url+"/api/v1/federation/replicas", map[string]any{
		"hub_url":                   fx.hub.url,
		"hub_project_id":            fx.hubProject.ID,
		"hub_project_uid":           fx.meta.ProjectUID,
		"project_name":              fx.meta.ProjectName,
		"replay_horizon_event_id":   fx.meta.ReplayHorizonEventID,
		"baseline_through_event_id": fx.meta.BaselineThroughEventID,
		"token":                     created.Token,
		"capabilities":              "pull,push,claim",
		"push_enabled":              true,
	}, &replica)
	require.True(t, replica.Binding.PushEnabled)
	fx.spokes[i].cred = config.FederationCredential{
		HubURL:       fx.hub.url,
		HubProjectID: fx.hubProject.ID,
		Token:        created.Token,
		Capabilities: "claim,pull,push",
	}
	return replica
}

func (fx *federationStressFixture) pendingPushBacklogs(t federationStressTB) []string {
	t.Helper()
	ctx := context.Background()
	var pending []string
	for i := range fx.spokes {
		if !fx.spokes[i].online {
			continue
		}
		spoke := fx.spokes[i]
		binding, err := spoke.db.FederationBindingByProject(ctx, spoke.replica.Project.ID)
		require.NoError(t, err)
		events, err := spoke.db.PendingFederationPushEvents(ctx,
			spoke.replica.Project.ID,
			spoke.db.InstanceUID(),
			binding.PushCursorEventID,
			1000,
		)
		require.NoError(t, err)
		if len(events) > 0 {
			pending = append(pending, fmt.Sprintf("spoke-%d:%d", i, len(events)))
		}
	}
	return pending
}

func (fx *federationStressFixture) applyRandomOperation(t *rapid.T) {
	t.Helper()
	fx.opSeq++
	switch rapid.IntRange(0, 5).Draw(t, "operation") {
	case 0:
		fx.createHubIssue(t)
	case 1:
		fx.createSpokeIssue(t)
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

func (fx *federationStressFixture) createHubIssue(t *rapid.T) {
	t.Helper()
	fx.createIssueOnNode(t, &fx.hub, fx.hubProject.ID, "hub-create")
}

func (fx *federationStressFixture) createSpokeIssue(t *rapid.T) {
	t.Helper()
	i, ok := fx.drawOnlineSpoke(t, "create-spoke")
	if !ok {
		return
	}
	fx.createIssueOnNode(t, &fx.spokes[i], fx.spokes[i].replica.Project.ID, "spoke-create")
}

func (fx *federationStressFixture) acquireSpokeHardClaim(t *rapid.T) {
	t.Helper()
	spokeIdx, issue, ok := fx.drawSpokeLiveIssue(t, "claim")
	if !ok {
		return
	}
	actor := fx.drawActor(t, "claim-actor")
	_ = fx.acquireClaim(t, spokeIdx, issue.ShortID, actor)
}

func (fx *federationStressFixture) releaseSpokeClaim(t *rapid.T) {
	t.Helper()
	spokeIdx, ok := fx.drawOnlineSpoke(t, "release-spoke")
	if !ok {
		return
	}
	claim, ok := fx.drawLiveClaim(t, fx.spokes[spokeIdx], fx.spokes[spokeIdx].replica.Project.ID, "release-claim")
	if !ok {
		return
	}
	fx.postClaimAction(t, spokeIdx, claim.IssueRef, "release", map[string]any{
		"holder": claim.Holder,
		"reason": "stress release",
	})
}

func (fx *federationStressFixture) editClaimedIssue(t *rapid.T) {
	t.Helper()
	spokeIdx, issue, ok := fx.drawSpokeLiveIssue(t, "edit")
	if !ok {
		return
	}
	actor := fx.drawActor(t, "edit-actor")
	if !fx.acquireClaim(t, spokeIdx, issue.ShortID, actor) {
		return
	}
	spoke := fx.spokes[spokeIdx]
	switch rapid.IntRange(0, 3).Draw(t, "edit-kind") {
	case 0:
		fx.patchIssue(t, spoke, spoke.replica.Project.ID, issue.ShortID, map[string]any{
			"actor": actor,
			"title": fmt.Sprintf("stress title %03d", fx.opSeq),
			"body":  fmt.Sprintf("stress body %03d", fx.opSeq),
		})
	case 1:
		priority := int64(rapid.IntRange(0, 3).Draw(t, "priority"))
		fx.patchIssue(t, spoke, spoke.replica.Project.ID, issue.ShortID, map[string]any{
			"actor":        actor,
			"set_priority": priority,
		})
	case 2:
		fx.patchIssue(t, spoke, spoke.replica.Project.ID, issue.ShortID, map[string]any{
			"actor":          actor,
			"clear_priority": true,
		})
	case 3:
		fx.addLabel(t, spoke, spoke.replica.Project.ID, issue.ShortID, actor,
			fmt.Sprintf("stress:%d", rapid.IntRange(0, 5).Draw(t, "label")))
	}
}

func (fx *federationStressFixture) commentClaimedIssue(t *rapid.T) {
	t.Helper()
	actor := fx.drawActor(t, "comment-actor")
	if rapid.Bool().Draw(t, "comment-on-hub") {
		issue, ok := fx.drawIssue(t, fx.hub, fx.hubProject.ID, false, "hub-comment")
		if !ok {
			return
		}
		if !fx.acquireHubClaim(t, issue.ShortID, actor) {
			return
		}
		fx.commentOnNode(t, fx.hub, fx.hubProject.ID, issue.ShortID, actor)
		return
	}
	spokeIdx, issue, ok := fx.drawSpokeLiveIssue(t, "spoke-comment")
	if !ok {
		return
	}
	if !fx.acquireClaim(t, spokeIdx, issue.ShortID, actor) {
		return
	}
	fx.commentOnNode(t, fx.spokes[spokeIdx], fx.spokes[spokeIdx].replica.Project.ID, issue.ShortID, actor)
}

func (fx *federationStressFixture) createIssueOnNode(t federationStressTB, node *federationStressNode, projectID int64, prefix string) createdIssue {
	t.Helper()
	return stressCreateIssue(t, node.http,
		node.url+"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues",
		map[string]any{
			"actor":     "stress",
			"title":     fmt.Sprintf("%s %03d", prefix, fx.opSeq),
			"body":      "generated by randomized federation stress workload",
			"labels":    []string{"stress"},
			"force_new": true,
		})
}

func (fx *federationStressFixture) acquireClaim(t federationStressTB, spokeIdx int, ref, actor string) bool {
	t.Helper()
	var out api.ClaimActionResponseBody
	ok := fx.postClaimAction(t, spokeIdx, ref, "acquire", map[string]any{
		"holder":      actor,
		"client_kind": "stress",
		"claim_kind":  "hard",
		"purpose":     "edit",
	}, &out)
	return ok && out.Granted && !out.Pending
}

func (fx *federationStressFixture) acquireHubClaim(t federationStressTB, ref, actor string) bool {
	t.Helper()
	var out api.ClaimActionResponseBody
	ok := fx.postClaimActionOnNode(t, fx.hub, fx.hubProject.ID, ref, "acquire", map[string]any{
		"holder":      actor,
		"client_kind": "stress",
		"claim_kind":  "hard",
		"purpose":     "edit",
	}, &out)
	return ok && out.Granted && !out.Pending
}

func (fx *federationStressFixture) postClaimAction(
	t federationStressTB,
	spokeIdx int,
	ref string,
	action string,
	body map[string]any,
	out ...*api.ClaimActionResponseBody,
) bool {
	t.Helper()
	spoke := fx.spokes[spokeIdx]
	return fx.postClaimActionOnNode(t, spoke, spoke.replica.Project.ID, ref, action, body, out...)
}

func (fx *federationStressFixture) postClaimActionOnNode(
	t federationStressTB,
	node federationStressNode,
	projectID int64,
	ref string,
	action string,
	body map[string]any,
	out ...*api.ClaimActionResponseBody,
) bool {
	t.Helper()
	var parsed api.ClaimActionResponseBody
	status, raw := stressDoJSON(t, node.http, http.MethodPost,
		node.url+"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+
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
	node federationStressNode,
	projectID int64,
	ref string,
	body map[string]any,
) {
	t.Helper()
	status, raw := stressDoJSON(t, node.http, http.MethodPatch,
		node.url+"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+url.PathEscape(ref),
		nil, body, nil)
	require.Equalf(t, http.StatusOK, status, "patch issue body: %s", raw)
}

func (fx *federationStressFixture) addLabel(
	t federationStressTB,
	node federationStressNode,
	projectID int64,
	ref string,
	actor string,
	label string,
) {
	t.Helper()
	status, raw := stressDoJSON(t, node.http, http.MethodPost,
		node.url+"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+url.PathEscape(ref)+"/labels",
		nil, map[string]any{"actor": actor, "label": label}, nil)
	require.Equalf(t, http.StatusOK, status, "add label body: %s", raw)
}

func (fx *federationStressFixture) commentOnNode(
	t federationStressTB,
	node federationStressNode,
	projectID int64,
	ref string,
	actor string,
) {
	t.Helper()
	status, raw := stressDoJSON(t, node.http, http.MethodPost,
		node.url+"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+url.PathEscape(ref)+"/comments",
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

func (fx *federationStressFixture) drawOnlineSpoke(t *rapid.T, label string) (int, bool) {
	t.Helper()
	var online []int
	for i := range fx.spokes {
		if fx.spokes[i].online {
			online = append(online, i)
		}
	}
	if len(online) == 0 {
		return 0, false
	}
	return online[rapid.IntRange(0, len(online)-1).Draw(t, label)], true
}

func (fx *federationStressFixture) drawSpokeLiveIssue(t *rapid.T, label string) (int, federationStressIssue, bool) {
	t.Helper()
	spokeIdx, ok := fx.drawOnlineSpoke(t, label+"-spoke")
	if !ok {
		return 0, federationStressIssue{}, false
	}
	issues := fx.issuesOnNode(t, fx.spokes[spokeIdx], fx.spokes[spokeIdx].replica.Project.ID, false)
	hubKnown := issues[:0]
	for _, issue := range issues {
		if fx.issueLiveOnHub(t, issue.UID) {
			hubKnown = append(hubKnown, issue)
		}
	}
	if len(hubKnown) == 0 {
		return 0, federationStressIssue{}, false
	}
	issue := hubKnown[rapid.IntRange(0, len(hubKnown)-1).Draw(t, label+"-issue")]
	return spokeIdx, issue, ok
}

func (fx *federationStressFixture) drawIssue(
	t *rapid.T,
	node federationStressNode,
	projectID int64,
	includeDeleted bool,
	label string,
) (federationStressIssue, bool) {
	t.Helper()
	issues := fx.issuesOnNode(t, node, projectID, includeDeleted)
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
	node federationStressNode,
	projectID int64,
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
		 ORDER BY c.id`, projectID)
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
	node federationStressNode,
	projectID int64,
	includeDeleted bool,
) []federationStressIssue {
	t.Helper()
	q := `SELECT id, uid, short_id, deleted_at IS NOT NULL FROM issues WHERE project_id = ?`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	q += ` ORDER BY id`
	rows, err := node.db.QueryContext(context.Background(), q, projectID)
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

func (fx *federationStressFixture) waitForFoldedProjectionMatch(t federationStressTB, spokeIdx int) {
	t.Helper()
	spoke := fx.spokes[spokeIdx]
	deadline := time.Now().Add(10 * time.Second)
	var lastHub, lastSpoke db.FoldProjection
	for time.Now().Before(deadline) {
		var err error
		lastHub, lastSpoke, err = stressFoldedProjections(t, fx.hub.db, spoke.db,
			fx.hubProject.ID, spoke.replica.Project.ID, fx.replayAfterID)
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
	t.Fatalf("folded projections did not converge for spoke-%d\ndaemon stderr: %s", spokeIdx, spoke.stderr.String())
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

func (fx *federationStressFixture) assertDaemonStderrClean(t federationStressTB) {
	t.Helper()
	fx.assertNodeStderrClean(t, "hub", fx.hub)
	for i := range fx.spokes {
		if fx.spokes[i].online {
			fx.assertNodeStderrClean(t, fmt.Sprintf("spoke-%d", i), fx.spokes[i])
		}
	}
}

func (fx *federationStressFixture) assertNodeStderrClean(t federationStressTB, name string, node federationStressNode) {
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
			t.Fatalf("%s daemon stderr contains %q:\n%s", name, bad, node.stderr.String())
		}
	}
}

func startFederationStressHub(t federationStressTB, bin string) federationStressNode {
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
	return federationStressNode{
		dirs:   dirs,
		url:    url,
		http:   &http.Client{Timeout: 5 * time.Second},
		db:     store,
		stderr: stderr,
		cmd:    cmd,
		online: true,
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
	cmd := exec.Command(bin, "daemon", "start", "--listen", addr) //nolint:gosec // test-built binary and loopback address
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

func startFederationStressSpoke(t federationStressTB, bin string) federationStressNode {
	t.Helper()
	dirs := newFederationStressDirs(t)
	stderr := &safeBuffer{}
	cmd := startFederationStressUnixDaemon(t, bin, dirs, stderr)
	url, client := stressConnectDaemon(t, dirs, stderr)
	store, err := sqlitestore.Open(context.Background(), dirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return federationStressNode{dirs: dirs, url: url, http: client, db: store, stderr: stderr, cmd: cmd, online: true}
}

func startFederationStressUnixDaemon(t federationStressTB, bin string, dirs e2eDirs, stderr *safeBuffer) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "daemon", "start") //nolint:gosec // test-built binary
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

func assertNoDuplicateLiveClaimsOnNode(t federationStressTB, node string, store *sqlitestore.Store) {
	t.Helper()
	rows, err := store.QueryContext(context.Background(), `
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
		assert.LessOrEqualf(t, count, 1, "%s has duplicate live claims for issue %s", node, issueUID)
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
		build := exec.Command("go", "build", "-o", bin, "go.kenn.io/kata/cmd/kata") //nolint:gosec // fixed args, test-only
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
