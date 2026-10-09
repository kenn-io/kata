package dbtest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// FrozenRelay32Fixture is owner-created signed state whose source event was
// compacted before the next upgrade. Historical verification keys must survive.
func FrozenRelay32Fixture(t *testing.T) (string, db.RootKeyPin, db.AttributionReceipt) {
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000009", KeyID: db.RootPublicKeyID(public), PublicKey: public}
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{Version: 1, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, EventUID: "00000000000000000000000005", ContentHash: strings.Repeat("a", 64), AccountableActor: "member", SourceActor: "assistant", IngressInstanceUID: pin.AuthorityUID, AcceptedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), ResetEpoch: 1, Sequence: 1}, private)
	require.NoError(t, err)
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	sql := fmt.Sprintf(`
 INSERT INTO meta(key,value) VALUES('schema_version','32'),('instance_uid','00000000000000000000000009');
 UPDATE meta SET value='47' WHERE key='project_access_revision';
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','shared-project');
 INSERT INTO teams(uid,name,revision) VALUES('00000000000000000000000003','members',3);
 INSERT INTO team_memberships(team_uid,actor) VALUES('00000000000000000000000003','member');
 INSERT INTO project_access_policies(project_uid,visibility,revision) VALUES('00000000000000000000000001','teams',7);
 INSERT INTO project_access_teams(project_uid,team_uid) VALUES('00000000000000000000000001','00000000000000000000000003');
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000004',id,'0004','Existing signed task','assistant' FROM projects WHERE uid='00000000000000000000000001';
 INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key,active) VALUES('%s','%s','%s','%s',1);
 INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES('%s','%s','%s',1,1,'%s','%s');
 INSERT INTO federation_entity_provenance(project_uid,kind,entity_uid,event_uid) VALUES('%s','issue','00000000000000000000000004','%s');
 `, pin.ProjectUID, pin.AuthorityUID, pin.KeyID, base64.StdEncoding.EncodeToString(pin.PublicKey), pin.ProjectUID, receipt.EventUID, receipt.ContentHash, pin.KeyID, strings.ReplaceAll(string(raw), "'", "''"), pin.ProjectUID, receipt.EventUID)
	return sql, pin, receipt
}

// AssertFrozenRelay32 checks upgrade preservation against the frozen schema-32 fixture.
func AssertFrozenRelay32(t *testing.T, store db.Storage, pin db.RootKeyPin, receipt db.AttributionReceipt) {
	ctx := t.Context()
	actual, err := store.RootAuthority(ctx, pin.ProjectUID)
	require.NoError(t, err)
	require.Equal(t, pin, actual)
	proof, err := store.EntityAttribution(ctx, pin.ProjectUID, "issue", "00000000000000000000000004")
	require.NoError(t, err)
	require.Equal(t, receipt, proof)
	require.NoError(t, db.VerifyRootReceipt(pin, proof))
	issue, err := store.IssueByUID(ctx, "00000000000000000000000004", db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "verified", issue.Verification)
	require.Equal(t, "member", issue.AccountableActor)
	require.Equal(t, "assistant", issue.Author)
	policy, err := store.ProjectAccessPolicy(ctx, pin.ProjectUID)
	require.NoError(t, err)
	require.Equal(t, "teams", policy.Visibility)
	require.Equal(t, int64(7), policy.Revision)
	require.Equal(t, []string{"00000000000000000000000003"}, policy.TeamUIDs)
	visible, err := store.AccessibleProjectUIDs(ctx, "outsider")
	require.NoError(t, err)
	require.Empty(t, visible)
	epoch, err := store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(47), epoch)
}
