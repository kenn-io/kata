package jsonl

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// Native config shape is a positive control in every mismatch case. The full
// staged graph, not native path syntax, must bind the signed descriptor route.
func scopedUpstreamRouteFixture(t *testing.T, indexFirstHop bool) ([]db.ImportRecord, *db.FederationBindingExport, string) {
	t.Helper()
	project, authority, local, intermediate := "00000000000000000000000001", "00000000000000000000000009", "00000000000000000000000008", "00000000000000000000000006"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project, AuthorityUID: authority, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	encode := func(value any) []byte { raw, err := json.Marshal(value); require.NoError(t, err); return raw }
	sign := func(raw []byte) []db.ScopedSignature {
		return []db.ScopedSignature{{ProjectUID: project, KeyID: pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}}
	}
	readiness, suspended := []byte("[]\n"), []byte(" [ ] ")
	cutover := db.ScopedProtocolCutoverCertificate{Version: 1, Cutover: db.ScopedProtocolCutoverExpectation{UID: "00000000000000000000000060", AuthorityUID: authority, RequestDigest: strings.Repeat("c", 64), ProjectUIDs: []string{project}, Floor: 2, ReadinessDigest: db.ScopedOfferedDigest(readiness), SuspendedBindingsDigest: db.ScopedOfferedDigest(suspended)}}
	transcript, err := db.ScopedCutoverCertificateSigningBytes(cutover)
	require.NoError(t, err)
	cutover.Signatures = sign(transcript)
	descriptor := db.ScopedViewDescriptor{Version: 2, Schema: 1, Key: db.ScopedViewKey{ViewUID: "00000000000000000000000020", Generation: 1}, AuthorityUID: authority, DestinationUID: local, RouteDigest: strings.Repeat("a", 64), RequestDigest: strings.Repeat("b", 64), RootCutoverUID: cutover.Cutover.UID, Projects: []db.ScopedProjectPin{{ProjectUID: project, Pin: pin}}, Route: []db.ScopedHopBinding{{BindingUID: "00000000000000000000000005", InstanceUID: intermediate, Epoch: 3}, {BindingUID: "00000000000000000000000007", InstanceUID: local, Epoch: 1}}}
	checkpoint := db.ScopedCheckpoint{View: descriptor, Cut: db.ScopedCut{Sequence: 1, CaptureUID: "00000000000000000000000070"}, Collections: db.ScopedCollections{Projects: []db.ScopedProjectState{}, Issues: []db.ScopedIssueState{}, Comments: []db.ScopedCommentState{}, Labels: []db.ScopedLabelState{}, Definitions: []db.ScopedDefinitionState{}, References: []db.ScopedReference{}, Provenance: []db.ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}}
	checkpoint.Cut.Digest, err = db.ScopedCheckpointDigest(checkpoint)
	require.NoError(t, err)
	transcript, err = db.ScopedCheckpointSigningBytes(checkpoint)
	require.NoError(t, err)
	checkpoint.Signatures = sign(transcript)
	descriptorBytes, checkpointBytes := encode(descriptor), encode(checkpoint)
	indexed := descriptor.Route[len(descriptor.Route)-1]
	if indexFirstHop {
		indexed = descriptor.Route[0]
	}
	generation := int64(1)
	view := &db.ScopedViewExport{ViewUID: descriptor.Key.ViewUID, CatalogEntryUID: "00000000000000000000000004", AuthorityUID: authority, BindingUID: indexed.BindingUID, Epoch: indexed.Epoch, AccountableActor: "member", RouteDigest: descriptor.RouteDigest, DestinationUID: local, ActiveGeneration: &generation, State: "active", RequestDigest: descriptor.RequestDigest}
	records := []db.ImportRecord{
		&db.MetaKV{Key: "instance_uid", Value: local},
		&db.ProjectExport{ID: 1, UID: project, Name: "example-project"}, &pin, view,
		&db.ScopedGenerationExport{ViewUID: descriptor.Key.ViewUID, Generation: 1, DescriptorBytes: descriptorBytes, DescriptorDigest: db.ScopedOfferedDigest(descriptorBytes), CheckpointBytes: checkpointBytes, CheckpointDigest: &checkpoint.Cut.Digest, CutSequence: 1, CutDigest: checkpoint.Cut.Digest, State: "active", AssembledBytes: int64(len(checkpointBytes)), RootCutoverUID: cutover.Cutover.UID},
		&db.ScopedProjectExport{ProjectUID: project, OwnerViewUID: &view.ViewUID, OwnerGeneration: &generation, ProtocolFloor: 2, RootCutoverUID: &cutover.Cutover.UID},
		&db.ScopedProtocolCutoverExport{CutoverUID: cutover.Cutover.UID, RequestDigest: cutover.Cutover.RequestDigest, ProjectUIDsBytes: encode([]string{project}), Floor: 2, ReadinessBytes: readiness, SuspendedBindingsBytes: suspended, CertificateBytes: encode(cutover), State: "committed"},
	}
	binding := &db.FederationBindingExport{ProjectID: 1, Role: "spoke", HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project, Actor: "member", Enabled: true, RelayConfig: &db.RelayBindingConfig{ProtocolVersion: 2, BindingUID: indexed.BindingUID, ResetEpoch: indexed.Epoch, AuthorityUID: authority, UpstreamInstanceUID: intermediate, LocalActor: "member", HubPath: []string{authority, intermediate, local}, ScopedView: &descriptor.Key, RootCutoverUID: cutover.Cutover.UID}}
	return records, binding, local
}

func TestScopedImportNativeUpstreamRoute(t *testing.T) {
	t.Run("exact_multihop", func(t *testing.T) {
		records, binding, local := scopedUpstreamRouteFixture(t, false)
		require.NoError(t, binding.RelayConfig.Validate(local))
		require.NoError(t, db.ValidateImportRecords(append(records, binding)))
	})
	for _, mismatch := range []string{"other_upstream", "extra_retained_path_node"} {
		t.Run(mismatch, func(t *testing.T) {
			records, binding, local := scopedUpstreamRouteFixture(t, false)
			config := binding.RelayConfig
			other := "00000000000000000000000010"
			if mismatch == "other_upstream" {
				config.UpstreamInstanceUID = other
				config.HubPath = []string{config.AuthorityUID, other, local}
			} else {
				config.HubPath = []string{config.AuthorityUID, other, config.UpstreamInstanceUID, local}
			}
			require.NoError(t, config.Validate(local), "native path remains valid; this must exercise descriptor equality")
			require.Error(t, db.ValidateImportRecords(append(records, binding)), "native upstream/path must equal exact retained descriptor at local destination")
		})
	}
}

func TestScopedImportUpstreamUsesTerminalHopWithoutRestrictingGenericView(t *testing.T) {
	records, binding, local := scopedUpstreamRouteFixture(t, true)
	require.NoError(t, db.ValidateImportRecords(records), "generic retained view may index any included authenticated binding/epoch")
	require.NoError(t, binding.RelayConfig.Validate(local), "first included tuple still has native-valid path syntax")
	require.Error(t, db.ValidateImportRecords(append(records, binding)), "native upstream negotiation at destination requires terminal signed binding/epoch")
}
