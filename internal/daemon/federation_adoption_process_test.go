package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// TestRelayAdoptionProcesses exercises the public CLI against independent
// daemons. Removing the adopt-existing flag, requiring a hub owner credential,
// or losing original identity/history during baseline must break this contract.
func TestRelayAdoptionProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs isolated hub, source and leaf processes")
	}
	binary := buildRelayProcessCLI(t)
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			hubHome, sourceHome, leafHome := relayProcessHome(t), relayProcessHome(t), relayProcessHome(t)
			bootstrap, account, sourceAccount := relayProcessToken(t), relayProcessToken(t), relayProcessToken(t)
			sourceOwner := relayProcessToken(t)
			var hubStore db.Storage
			var storageConfig string
			hubDSN := filepath.Join(hubHome, "kata.db")
			if backend == "postgres" {
				dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
				t.Cleanup(cleanup)
				store, err := pgstore.Open(t.Context(), dsn)
				require.NoError(t, err)
				hubStore = store
				hubDSN = dsn
				storageConfig = fmt.Sprintf("[storage]\ndsn = %q\n", dsn)
			} else {
				store, err := sqlitestore.Open(t.Context(), filepath.Join(hubHome, "kata.db"))
				require.NoError(t, err)
				hubStore = store
			}
			// Owner-only scratch provisioning ends before the adopting daemon is
			// started. Its catalog receives only this ordinary unscoped account.
			_, _, err := hubStore.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
				Actor: "publishing-account", AdminActor: "scratch-owner", PlaintextToken: account,
			})
			require.NoError(t, err)
			hubUID := hubStore.InstanceUID()
			require.NoError(t, hubStore.Close())
			writeRelayProcessConfig(t, hubHome, storageConfig+fmt.Sprintf("[auth]\ntoken = %q\nrequire_token_identity = true\n", bootstrap))
			hub := startRelayProcess(t, binary, hubHome)
			hub.openHistory(hubDSN, backend)
			hub.monitorServing(t)
			status, raw := hub.get("/api/v1/instance", account)
			require.Equal(t, http.StatusOK, status, "%s", raw)
			var identity struct {
				Auth struct {
					Kind  string `json:"kind"`
					Actor string `json:"actor"`
				} `json:"auth"`
			}
			require.NoError(t, json.Unmarshal(raw, &identity))
			require.Equal(t, "publishing-account", identity.Auth.Actor)
			status, raw = hub.get("/api/v1/tokens", account)
			// Native host policy conceals owner-only operations from ordinary
			// accounts, as in TestCreateTokenEndpoint_DBTokenForbidden.
			require.Equal(t, http.StatusNotFound, status, "the publishing credential cannot see token administration: %s", raw)
			require.Equal(t, "not_found", bridgeErrorCode(t, raw))

			original, refused := seedRelayProcessSource(t, sourceHome, hubUID, sourceAccount)
			writeRelayProcessConfig(t, sourceHome, fmt.Sprintf("[auth]\ntoken = %q\nrequire_token_identity = true\n[[daemon]]\nname = \"selected-hub\"\nurl = %q\ntoken = %q\n", sourceOwner, hub.url, account))
			source := startRelayProcess(t, binary, sourceHome)
			source.ownerToken = sourceOwner
			source.openHistory(filepath.Join(sourceHome, "kata.db"), "sqlite")
			// Host tokens do not enter project publication. The leaf receives
			// this distinct ordinary source account, never either owner token.
			writeRelayProcessConfig(t, leafHome, fmt.Sprintf("[[daemon]]\nname = \"selected-source\"\nurl = %q\ntoken = %q\n", source.url, sourceAccount))
			leaf := startRelayProcess(t, binary, leafHome)
			leaf.openHistory(filepath.Join(leafHome, "kata.db"), "sqlite")
			require.True(t, hub.healthy())
			require.True(t, leaf.healthy())

			// The source CLI uses only its own local owner token for bridge
			// administration. Its catalog has an ordinary hub account; the hub
			// bootstrap token is never passed to the source, CLI or leaf.
			args := []string{"federation", "bridge", "connect", "--project", original.ProjectName,
				"--hub-daemon", "selected-hub", "--hub-project", original.ProjectName,
				"--adopt-existing", "--serve-downstream", "--as", "local-member", "--json"}
			output, err := source.cli(append(args, "--preflight")...)
			require.NoError(t, err, "ordinary-account adoption preflight: %s", output)
			var preview struct {
				Status          string `json:"status"`
				UpstreamAccount string `json:"upstream_account"`
			}
			require.NoError(t, json.Unmarshal(output, &preview))
			require.Equal(t, "ready", preview.Status)
			require.Equal(t, "publishing-account", preview.UpstreamAccount)
			_, published := hub.projectByUID(original.ProjectUID, account)
			require.False(t, published, "preflight must not publish a hub project")
			sourceProject, found := source.projectByUID(original.ProjectUID, sourceAccount)
			require.True(t, found)
			assertRelayProcessOriginal(t, source, sourceProject, sourceAccount, original)
			require.True(t, hub.healthy(), "publication must not restart or stop the hub")
			output, err = source.cli(args...)
			require.NoError(t, err, "ordinary-account adoption commit: %s", output)
			require.True(t, hub.healthy())
			awaitRelayProcess(t, "source cutover", func() error {
				state := source.bridgeStatus(original.ProjectName)
				if state.State != "connected" || state.SourcePaused {
					return fmt.Errorf("bridge state %s", state.State)
				}
				return source.drained(state.ProjectID, sourceAccount)
			})
			hubProject, found := hub.projectByUID(original.ProjectUID, account)
			require.True(t, found)
			require.Equal(t, original.ProjectName, hubProject.Name)
			sourceState := source.bridgeStatus(original.ProjectName)
			require.Equal(t, original.ProjectUID, sourceState.ProjectUID)
			require.Equal(t, hub.url, sourceState.HubURL)
			require.NotNil(t, sourceState.Relay)
			require.Equal(t, hubUID, sourceState.Relay.UpstreamInstanceUID)
			require.True(t, sourceState.Relay.ServeDownstream)
			assertRelayProcessOriginal(t, hub, hubProject, account, original)
			assertRelayProcessOriginal(t, source, sourceProject, sourceAccount, original)
			leafOriginal := relayProcessLeafExpectation(original)

			// Assert the downstream credential actually authenticates as an
			// ordinary source account before asking the leaf to connect.
			status, raw = source.get("/api/v1/instance", sourceAccount)
			require.Equal(t, http.StatusOK, status, "%s", raw)
			require.NoError(t, json.Unmarshal(raw, &identity))
			require.Equal(t, "db_token", identity.Auth.Kind)
			require.Equal(t, "local-member", identity.Auth.Actor)
			leafArgs := []string{"federation", "bridge", "connect", "--project", "leaf-project",
				"--hub-daemon", "selected-source", "--hub-project", original.ProjectName,
				"--as", "device-member", "--json"}
			output, err = leaf.cli(append(leafArgs, "--preflight")...)
			require.NoError(t, err, "leaf preflight: %s", output)
			output, err = leaf.cli(leafArgs...)
			require.NoError(t, err, "leaf connect through source: %s", output)
			leafState := leaf.bridgeStatus("leaf-project")
			require.Equal(t, source.url, leafState.HubURL)
			require.NotNil(t, leafState.Federation)
			require.Equal(t, sourceProject.ID, leafState.Federation.HubProjectID)
			require.Equal(t, "local-member", leafState.UpstreamAccount)
			require.NotNil(t, leafState.Relay)
			require.Equal(t, original.SourceInstanceUID, leafState.Relay.UpstreamInstanceUID)
			require.Equal(t, []string{hubUID, original.SourceInstanceUID, leaf.instanceUID()}, leafState.Relay.HubPath,
				"leaf must be under source, not directly under root")
			require.False(t, leafState.Relay.ServeDownstream)
			leafProject, found := leaf.projectByUID(original.ProjectUID, "")
			require.True(t, found)
			require.Equal(t, "leaf-project", leafProject.Name)
			awaitRelayProcess(t, "leaf baseline", func() error {
				events := leaf.events(leafProject.ID)
				for _, record := range leafOriginal.Records {
					if event, ok := record.(*db.EventExport); ok {
						if _, present := events[event.UID]; !present {
							return fmt.Errorf("leaf has not received original %s event %s (stored events=%d, drain=%v)",
								event.Type, event.UID, len(events), leaf.drained(leafProject.ID, ""))
						}
					}
				}
				return leaf.drained(leafProject.ID, "")
			})
			assertRelayProcessLeaf(t, leaf, leafProject, leafOriginal)
			assertRelayProcessOriginal(t, source, sourceProject, sourceAccount, original)
			assertRelayProcessOriginal(t, hub, hubProject, account, original)

			// Capture the actual applied baseline clock, not just original input.
			// A later source event must be strictly after every imported event.
			sourceHistory, leafHistory := source.events(sourceProject.ID), leaf.events(leafProject.ID)
			var baselineSnapshots int
			for _, event := range hub.events(hubProject.ID) {
				if event.Type == "issue.snapshot" && event.OriginInstanceUID == hubUID {
					baselineSnapshots++
					assertRelayProcessEvent(t, event, sourceHistory)
					assertRelayProcessEvent(t, event, leafHistory)
				}
			}
			require.Positive(t, baselineSnapshots, "ordinary enable must supplement original history with a root baseline")
			baseline := latestRelayProcessEvent(sourceHistory)
			require.NotEmpty(t, baseline.EventUID)
			firstUID := "01K00000000000000000000002"
			upstream := source.editTitle(sourceProject.ID, firstUID, sourceAccount, "local-member", "Edited on source")
			require.True(t, relayProcessHLCAfter(upstream, baseline), "source HLC must follow imported baseline")
			awaitRelayProcess(t, "source edit through root and leaf", func() error {
				for _, target := range []struct {
					process   *relayProcess
					projectID int64
					token     string
				}{
					{hub, hubProject.ID, account}, {leaf, leafProject.ID, ""},
				} {
					if target.process.showIssue(target.projectID, firstUID, target.token).Issue.Title != "Edited on source" {
						return fmt.Errorf("source edit has not reached %s", target.process.url)
					}
				}
				return source.drained(sourceProject.ID, sourceAccount)
			})
			assertRelayProcessEvent(t, upstream, hub.events(hubProject.ID))
			assertRelayProcessEvent(t, upstream, leaf.events(leafProject.ID))

			downstream := hub.editTitle(hubProject.ID, firstUID, account, "publishing-account", "Edited on root")
			require.True(t, relayProcessHLCAfter(downstream, upstream), "root HLC must observe the source edit")
			awaitRelayProcess(t, "root edit through source and leaf", func() error {
				if source.showIssue(sourceProject.ID, firstUID, sourceAccount).Issue.Title != "Edited on root" ||
					leaf.showIssue(leafProject.ID, firstUID, "").Issue.Title != "Edited on root" {
					return fmt.Errorf("root edit has not reached both descendants")
				}
				return nil
			})
			assertRelayProcessEvent(t, downstream, source.events(sourceProject.ID))
			assertRelayProcessEvent(t, downstream, leaf.events(leafProject.ID))

			fromLeaf := leaf.editTitle(leafProject.ID, firstUID, "", "device-member", "Edited on leaf")
			require.True(t, relayProcessHLCAfter(fromLeaf, downstream), "leaf HLC must observe the root baseline and edit")
			awaitRelayProcess(t, "leaf edit accepted by root", func() error {
				if hub.showIssue(hubProject.ID, firstUID, account).Issue.Title != "Edited on leaf" ||
					source.showIssue(sourceProject.ID, firstUID, sourceAccount).Issue.Title != "Edited on leaf" {
					return fmt.Errorf("leaf edit has not reached source and root")
				}
				return leaf.drained(leafProject.ID, "")
			})
			assertRelayProcessEvent(t, fromLeaf, hub.events(hubProject.ID))
			assertRelayProcessEvent(t, fromLeaf, source.events(sourceProject.ID))

			rootCredential := relayProcessCredential(t, sourceHome, original.ProjectUID)
			leafCredential := relayProcessCredential(t, leafHome, original.ProjectUID)
			require.Equal(t, hub.url, rootCredential.HubURL)
			require.Equal(t, source.url, leafCredential.HubURL)
			awaitRelayProcess(t, "all streams and root receipts drained", func() error {
				for _, target := range []struct {
					process   *relayProcess
					projectID int64
					token     string
				}{
					{hub, hubProject.ID, account}, {source, sourceProject.ID, sourceAccount}, {leaf, leafProject.ID, ""},
				} {
					if err := target.process.drained(target.projectID, target.token); err != nil {
						return err
					}
				}
				if err := hub.noDeliveries(hubProject.ID, rootCredential.Token); err != nil {
					return err
				}
				return source.noDeliveries(sourceProject.ID, leafCredential.Token)
			})
			// Edits may add events. Every original tuple must still remain.
			assertRelayProcessHistory(t, original, hub.events(hubProject.ID))
			assertRelayProcessHistory(t, original, source.events(sourceProject.ID))
			assertRelayProcessHistory(t, leafOriginal, leaf.events(leafProject.ID))

			output, err = source.cli("federation", "bridge", "connect", "--project", refused.Name,
				"--hub-daemon", "selected-hub", "--hub-project", refused.Name,
				"--adopt-existing", "--as", "local-member", "--json")
			require.Error(t, err, "a source requiring reset must fail before hub publication")
			require.Contains(t, string(output), "federation_adoption_ineligible")
			_, found = hub.projectByUID(refused.UID, account)
			require.False(t, found, "refusal must leave no published hub identity")
			status, raw = hub.get("/api/v1/projects", account)
			require.Equal(t, http.StatusOK, status)
			var catalog api.ListProjectsResponse
			require.NoError(t, json.Unmarshal(raw, &catalog.Body))
			for _, project := range catalog.Body.Projects {
				require.NotEqual(t, refused.Name, project.Name, "refusal must not leave a new-UID project either")
			}
		})
	}
}

func seedRelayProcessSource(t *testing.T, home, targetUID, sourceAccount string) (db.FederationPublication, db.Project) {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	// Reuse the native portable fixture's valid six-character UID suffixes,
	// two original authors, reply/edit lineage, labels, links and future HLC.
	fixture := dbtest.PublicationFixture(t, targetUID)
	require.NoError(t, store.ImportReplay(t.Context(), fixture.Records, db.ImportOptions{NewInstance: true, RequireFreshTarget: true}))
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		Actor: "local-member", AdminActor: "scratch-owner", PlaintextToken: sourceAccount,
	})
	require.NoError(t, err)
	project, err := store.ProjectByUID(t.Context(), fixture.ProjectUID)
	require.NoError(t, err)
	_, _, _, err = store.RenameProjectAndEvent(t.Context(), project.ID, "source-project", "original-author")
	require.NoError(t, err)
	first, err := store.IssueByUID(t.Context(), "01K00000000000000000000002", db.IncludeDeletedYes)
	require.NoError(t, err)
	_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: first.ID, Author: "third-author", Teammate: "build-worker", Body: "Retained source comment",
		ReplyToUID: "01K00000000000000000000006", ReplyKind: "reply", ValidateReply: true,
	})
	require.NoError(t, err)
	_, _, _, err = store.EditComment(t.Context(), db.EditCommentParams{
		IssueID: first.ID, CommentUID: "01K00000000000000000000007", Actor: "reply-author", Body: "Reply edited before publication",
	})
	require.NoError(t, err)
	deleted, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Retained tombstone", Author: "other-author"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(t.Context(), deleted.ID, "other-author")
	require.NoError(t, err)
	publication, err := jsonl.CaptureFederationPublication(t.Context(), store, project.ID, db.FederationPublication{
		OperationUID: fixture.OperationUID, SourceInstanceUID: store.InstanceUID(),
		TargetInstanceUID: targetUID, ProjectUID: project.UID, ProjectName: "source-project",
	})
	require.NoError(t, err, "process fixture must satisfy the closed publication contract before starting adoption")
	var renamed bool
	for _, record := range publication.Records {
		if event, ok := record.(*db.EventExport); ok && event.Type == "project.renamed" {
			renamed = true
		}
	}
	require.True(t, renamed, "fixture must prove host-local rename history survives on source and published hub")
	// This second scratch project has a real retained update but no retained
	// introduction. It exercises the explicit reset-required source refusal.
	refused, err := store.CreateProject(t.Context(), "reset-source-project")
	require.NoError(t, err)
	issue, introduction, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: refused.ID, Title: "Missing retained introduction", Author: "source-author"})
	require.NoError(t, err)
	title := "Retained dependent update"
	_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "source-author"})
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "DELETE FROM events WHERE uid = ?", introduction.UID)
	require.NoError(t, err)
	var history []db.EventExport
	for event, err := range store.ExportEvents(t.Context(), db.ExportFilter{ProjectID: &refused.ID, IncludeDeleted: true}) {
		require.NoError(t, err)
		history = append(history, event)
	}
	needsReset, err := db.RelayHistoryNeedsReset(history)
	require.NoError(t, err)
	require.True(t, needsReset, "refusal fixture must exercise an actual non-replayable retained prefix")
	return publication, refused
}

func (p *relayProcess) instanceUID() string {
	status, raw := p.get("/api/v1/instance", "")
	require.Equal(p.t, http.StatusOK, status, "%s", raw)
	var instance struct {
		UID string `json:"instance_uid"`
	}
	require.NoError(p.t, json.Unmarshal(raw, &instance))
	require.NotEmpty(p.t, instance.UID)
	return instance.UID
}

func (p *relayProcess) projectByUID(uid, token string) (api.ProjectOut, bool) {
	status, raw := p.get("/api/v1/projects", token)
	require.Equal(p.t, http.StatusOK, status, "%s", raw)
	var projects api.ListProjectsResponse
	require.NoError(p.t, json.Unmarshal(raw, &projects.Body))
	for _, project := range projects.Body.Projects {
		if project.UID == uid {
			return project, true
		}
	}
	return api.ProjectOut{}, false
}

func (p *relayProcess) bridgeStatus(name string) api.FederationBridgeStatusBody {
	status, raw := p.get("/api/v1/federation/bridges/"+name, p.ownerToken)
	require.Equal(p.t, http.StatusOK, status, "%s", raw)
	var bridge api.FederationBridgeStatusBody
	require.NoError(p.t, json.Unmarshal(raw, &bridge))
	return bridge
}

func (p *relayProcess) showIssue(projectID int64, uid, token string) api.ShowIssueResponseBody {
	status, raw := p.get(fmt.Sprintf("/api/v1/projects/%d/issues/%s?include_deleted=true", projectID, uid), token)
	require.Equal(p.t, http.StatusOK, status, "%s", raw)
	var issue api.ShowIssueResponseBody
	require.NoError(p.t, json.Unmarshal(raw, &issue))
	require.False(p.t, issue.CommentsTruncated, "small process fixture must fit one complete comment page")
	return issue
}

// Inspect original rows while the independent daemon serves. The live event
// polling API deliberately resets old cursors when attribution receipts change;
// that invalidation is not an archival-history read or a destructive reset.
// Opening with ReadOnly skips native initialization/migration/write paths.
func (p *relayProcess) openHistory(dsn, backend string) {
	p.t.Helper()
	var store db.Storage
	if backend == "postgres" {
		reader, err := pgstore.Open(p.t.Context(), dsn, db.ReadOnly())
		require.NoError(p.t, err)
		store = reader
	} else {
		reader, err := sqlitestore.Open(p.t.Context(), dsn, db.ReadOnly())
		require.NoError(p.t, err)
		store = reader
	}
	p.history = store
	p.t.Cleanup(func() { require.NoError(p.t, store.Close()) })
}

func (p *relayProcess) events(projectID int64) map[string]api.EventEnvelope {
	p.t.Helper()
	require.NotNil(p.t, p.history)
	snapshotter, ok := p.history.(interface {
		BeginExportSnapshot(context.Context) (db.Storage, func() error, error)
	})
	require.True(p.t, ok)
	snapshot, release, err := snapshotter.BeginExportSnapshot(p.t.Context())
	require.NoError(p.t, err)
	defer func() { require.NoError(p.t, release()) }()
	reader, ok := snapshot.(db.FederationPublicationSnapshotReader)
	require.True(p.t, ok)
	all := map[string]api.EventEnvelope{}
	for row, err := range reader.ExportFederationPublicationEvents(p.t.Context(), projectID) {
		require.NoError(p.t, err)
		_, duplicate := all[row.UID]
		require.False(p.t, duplicate, "duplicate original event UID")
		all[row.UID] = api.EventEnvelope{EventID: row.ID, EventUID: row.UID,
			OriginInstanceUID: row.OriginInstanceUID, ProjectID: row.ProjectID,
			ProjectUID: row.ProjectUID, ProjectName: row.ProjectName,
			IssueID: row.IssueID, IssueUID: row.IssueUID, RelatedIssueID: row.RelatedIssueID,
			RelatedIssueUID: row.RelatedIssueUID, Type: row.Type, Actor: row.Actor,
			HLCPhysicalMS: row.HLCPhysicalMS, HLCCounter: row.HLCCounter,
			ContentHash: row.ContentHash, Payload: row.Payload, CreatedAt: relayProcessTime(p.t, row.CreatedAt)}
		require.LessOrEqual(p.t, len(all), 10000, "process fixture history must remain bounded")
	}
	return all
}

func (p *relayProcess) drained(projectID int64, token string) error {
	status, raw := p.get(fmt.Sprintf("/api/v1/projects/%d/federation/status", projectID), token)
	require.Equal(p.t, http.StatusOK, status, "%s", raw)
	var body api.FederationStatusBody
	require.NoError(p.t, json.Unmarshal(raw, &body))
	require.Len(p.t, body.Statuses, 1)
	state := body.Statuses[0]
	if !state.Enabled || state.PendingPushCount != 0 || state.PendingPushHighWaterEventID != 0 ||
		state.LiveClaimCount != 0 || state.PendingClaimCount != 0 || state.ActiveQuarantineCount != 0 ||
		len(state.ActiveQuarantines) != 0 || state.UnresolvedViolationCount != 0 || state.ResetBlocker != "" ||
		(state.LastError != nil && *state.LastError != "") {
		return fmt.Errorf("project %s has unfinished relay work: pending=%d claims=%d/%d quarantine=%d reset_blocker=%s",
			state.ProjectUID, state.PendingPushCount, state.LiveClaimCount, state.PendingClaimCount, state.ActiveQuarantineCount, state.ResetBlocker)
	}
	return nil
}

func relayProcessCredential(t *testing.T, home, uid string) config.FederationCredential {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(home, "credentials.toml")) //nolint:gosec // home is a test-owned temporary directory.
	require.NoError(t, err)
	var credentials config.FederationCredentials
	_, err = toml.Decode(string(contents), &credentials)
	require.NoError(t, err)
	credential, found := credentials.Projects[uid]
	require.True(t, found)
	require.False(t, credential.RelayEnrollmentPending)
	require.NotEmpty(t, credential.Token)
	return credential
}

// Read the sender's actual unacknowledged prefix with the retained narrow
// credential. This observes event and receipt ACKs, not just projection parity.
func (p *relayProcess) noDeliveries(projectID int64, transportToken string) error {
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		status, raw := p.get(fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=%s&limit=1", projectID, stream), transportToken)
		require.Equal(p.t, http.StatusOK, status, "retained transport stream read failed")
		var batch db.RelayBatch
		require.NoError(p.t, json.Unmarshal(raw, &batch))
		require.Equal(p.t, stream, batch.Stream)
		if len(batch.Envelopes) != 0 || len(batch.Artifacts) != 0 {
			return fmt.Errorf("project %d still owes %s deliveries", projectID, stream)
		}
	}
	return nil
}

func (p *relayProcess) editTitle(projectID int64, uid, token, actor, title string) api.EventEnvelope {
	status, raw := p.request(http.MethodPatch, fmt.Sprintf("/api/v1/projects/%d/issues/%s", projectID, uid), token,
		map[string]string{"actor": actor, "title": title})
	require.Equal(p.t, http.StatusOK, status, "%s", raw)
	var response api.EditIssueResponse
	require.NoError(p.t, json.Unmarshal(raw, &response.Body))
	require.True(p.t, response.Body.Changed)
	require.Equal(p.t, title, response.Body.Issue.Title)
	require.NotNil(p.t, response.Body.Event)
	event, found := p.events(projectID)[response.Body.Event.UID]
	require.True(p.t, found, "a successful edit must have a durably readable event")
	return event
}

func relayProcessTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}

func assertRelayProcessOriginal(t *testing.T, process *relayProcess, project api.ProjectOut, token string, original db.FederationPublication) {
	t.Helper()
	issues := assertRelayProcessContent(t, process, project, token, original)
	for _, record := range original.Records {
		switch row := record.(type) {
		case *db.ProjectExport:
			require.True(t, relayProcessTime(t, row.CreatedAt).Equal(project.CreatedAt),
				"source and published hub must preserve the original project creation time")
		case *db.IssueLabelExport:
			for _, label := range issues[row.IssueID].Labels {
				if label.Label == row.Label {
					require.Equal(t, row.Author, label.Author)
					require.True(t, relayProcessTime(t, row.CreatedAt).Equal(label.CreatedAt))
				}
			}
		}
	}
}

func assertRelayProcessLeaf(t *testing.T, process *relayProcess, project api.ProjectOut, original db.FederationPublication) {
	t.Helper()
	issues := assertRelayProcessContent(t, process, project, "", original)
	for _, issue := range issues {
		for _, label := range issue.Labels {
			require.Equal(t, "federation", label.Author, "native label snapshots carry names, not historical attribution")
			require.False(t, label.CreatedAt.IsZero(), "native replay creates a local label bookkeeping timestamp")
		}
	}
}

// A newly connected leaf creates its own local catalog row. Its issue/comment
// projections and all portable event tuples still retain original timestamps.
func assertRelayProcessContent(t *testing.T, process *relayProcess, project api.ProjectOut, token string, original db.FederationPublication) map[int64]api.ShowIssueResponseBody {
	t.Helper()
	require.Equal(t, original.ProjectUID, project.UID)
	issues := map[int64]api.ShowIssueResponseBody{}
	for _, record := range original.Records {
		if row, ok := record.(*db.IssueExport); ok {
			read := process.showIssue(project.ID, row.UID, token)
			issues[row.ID] = read
			require.Equal(t, row.UID, read.Issue.UID)
			require.Equal(t, row.ShortID, read.Issue.ShortID)
			require.Equal(t, project.ID, read.Issue.ProjectID)
			require.Equal(t, row.Author, read.Issue.Author)
			require.Equal(t, row.Title, read.Issue.Title)
			require.Equal(t, row.Body, read.Issue.Body)
			require.Equal(t, row.Status, read.Issue.Status)
			require.True(t, relayProcessTime(t, row.CreatedAt).Equal(read.Issue.CreatedAt))
			require.True(t, relayProcessTime(t, row.UpdatedAt).Equal(read.Issue.UpdatedAt))
			require.JSONEq(t, string(row.Metadata), string(read.Issue.Metadata))
			if row.DeletedAt == nil {
				require.Nil(t, read.Issue.DeletedAt)
			} else {
				require.NotNil(t, read.Issue.DeletedAt)
				require.True(t, relayProcessTime(t, *row.DeletedAt).Equal(*read.Issue.DeletedAt))
			}
		}
	}
	expectedLabels := map[int64][]string{}
	expectedComments := map[int64]int{}
	for _, record := range original.Records {
		switch row := record.(type) {
		case *db.ProjectExport:
			require.JSONEq(t, string(row.Metadata), string(project.Metadata))
		case *db.CommentExport:
			expectedComments[row.IssueID]++
			var found bool
			for _, comment := range issues[row.IssueID].Comments {
				if comment.UID != row.UID {
					continue
				}
				found = true
				require.Equal(t, row.Author, comment.Author)
				require.Equal(t, row.Teammate, comment.Teammate)
				require.Equal(t, row.Body, comment.Body)
				require.Equal(t, row.ReplyToUID, comment.ReplyToUID)
				require.Equal(t, row.ReplyKind, comment.ReplyKind)
				require.True(t, relayProcessTime(t, row.CreatedAt).Equal(comment.CreatedAt))
				if row.EditedAt == "" {
					require.Nil(t, comment.EditedAt)
				} else {
					require.NotNil(t, comment.EditedAt)
					require.True(t, relayProcessTime(t, row.EditedAt).Equal(*comment.EditedAt))
				}
			}
			require.True(t, found, "original comment %s must survive baseline", row.UID)
		case *db.IssueLabelExport:
			expectedLabels[row.IssueID] = append(expectedLabels[row.IssueID], row.Label)
		case *db.LinkExport:
			var found bool
			for _, link := range issues[row.FromIssueID].Links {
				if link.From.UID == row.FromIssueUID && link.To.UID == row.ToIssueUID && link.Type == row.Type {
					found = true
					require.Equal(t, row.Author, link.Author)
					require.True(t, relayProcessTime(t, row.CreatedAt).Equal(link.CreatedAt))
				}
			}
			require.True(t, found, "original intra-project link must survive baseline")
		}
	}
	for id, issue := range issues {
		var labels []string
		for _, label := range issue.Labels {
			labels = append(labels, label.Label)
		}
		require.ElementsMatch(t, expectedLabels[id], labels, "exact label names and issue associations must survive baseline")
		require.Len(t, issue.Comments, expectedComments[id], "original comments must survive without duplication")
	}
	assertRelayProcessHistory(t, original, process.events(project.ID))
	return issues
}

func assertRelayProcessHistory(t *testing.T, original db.FederationPublication, events map[string]api.EventEnvelope) {
	t.Helper()
	var previous int64
	for _, record := range original.Records {
		row, ok := record.(*db.EventExport)
		if !ok {
			continue
		}
		want := api.EventEnvelope{EventUID: row.UID, OriginInstanceUID: row.OriginInstanceUID,
			ProjectUID: original.ProjectUID, ProjectName: row.ProjectName, IssueUID: row.IssueUID,
			RelatedIssueUID: row.RelatedIssueUID, Type: row.Type, Actor: row.Actor,
			HLCPhysicalMS: row.HLCPhysicalMS, HLCCounter: row.HLCCounter, ContentHash: row.ContentHash,
			Payload: row.Payload, CreatedAt: relayProcessTime(t, row.CreatedAt)}
		assertRelayProcessEvent(t, want, events)
		require.Greater(t, events[row.UID].EventID, previous, "original event order must survive native ID remapping")
		previous = events[row.UID].EventID
	}
}

// Relay replicas have independent local catalog names. Publication retains
// these three host-local catalog event families on source and hub, but native
// relay transport carries portable history rather than replaying their catalog.
// All other original rows remain mandatory, including every issue event.
func relayProcessLeafExpectation(original db.FederationPublication) db.FederationPublication {
	portable := original
	portable.Records = make([]db.ImportRecord, 0, len(original.Records))
	for _, record := range original.Records {
		if event, ok := record.(*db.EventExport); ok {
			switch event.Type {
			case "project.created", "project.renamed", "project.author_rewritten":
				continue
			}
		}
		portable.Records = append(portable.Records, record)
	}
	return portable
}

func assertRelayProcessEvent(t *testing.T, expected api.EventEnvelope, events map[string]api.EventEnvelope) {
	t.Helper()
	actual, found := events[expected.EventUID]
	require.True(t, found, "original event %s must remain", expected.EventUID)
	// Numeric row IDs may change. All portable identity, history, payload bytes
	// and hash fields are compared as observed, without recomputing either hash.
	portable := func(event api.EventEnvelope) db.RemoteEvent {
		return db.RemoteEvent{EventUID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
			ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID,
			RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor,
			HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, ContentHash: event.ContentHash,
			Payload: event.Payload, CreatedAt: event.CreatedAt.UTC()}
	}
	require.Equal(t, portable(expected), portable(actual))
}

func relayProcessHLCAfter(a, b api.EventEnvelope) bool {
	return a.HLCPhysicalMS > b.HLCPhysicalMS || (a.HLCPhysicalMS == b.HLCPhysicalMS && a.HLCCounter > b.HLCCounter)
}

func latestRelayProcessEvent(events map[string]api.EventEnvelope) api.EventEnvelope {
	var latest api.EventEnvelope
	for _, event := range events {
		if relayProcessHLCAfter(event, latest) {
			latest = event
		}
	}
	return latest
}
