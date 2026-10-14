package db

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The relay schema carries no CHECK constraints, so the row shapes it needs
// must be refused by the Go validators every write and import path shares.
// Each case starts from a record that validates and changes one field, so a
// removed check cannot hide behind an unrelated defect in the fixture.
func TestImportRecordsRefuseMalformedRelayShapes(t *testing.T) {
	project := "01HZNQ7VFPK1XGD8R5MABCD4EA"
	binding := "01HZNQ7VFPK1XGD8R5MABCD4EB"
	entity := "01HZNQ7VFPK1XGD8R5MABCD4EC"
	authority := "01HZNQ7VFPK1XGD8R5MABCD4ED"
	parent := int64(7)
	projectID := int64(3)
	hash := strings.Repeat("a", 64)
	public, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	validTeam := func() *Team { return &Team{UID: project, Name: "reviewers", Revision: 1} }
	validMembership := func() *TeamMembership { return &TeamMembership{TeamUID: project, Actor: "member"} }
	validPolicy := func() *ProjectAccessPolicy {
		return &ProjectAccessPolicy{ProjectUID: project, Visibility: "teams", Revision: 2, TeamUIDs: []string{binding}}
	}
	validProvenance := func() *EntityProvenance {
		return &EntityProvenance{ProjectUID: project, Kind: "issue", EntityUID: entity, EventUID: binding}
	}
	validPin := func() *RootKeyPin {
		return &RootKeyPin{ProjectUID: project, AuthorityUID: authority, KeyID: RootPublicKeyID(public), PublicKey: public}
	}
	validCursor := func() *RelayCursorExport {
		return &RelayCursorExport{ProjectUID: project, BindingUID: binding, Stream: RelayStreamEvent, ResetEpoch: 1, OfferedThrough: 2, AcceptedThrough: 1, EmittedThrough: 2, AcknowledgedThrough: 1}
	}
	validOutbox := func() *RelayOutboxExport {
		envelope, err := SealRelayEnvelope(RelayEnvelope{
			Version: RelayProtocolVersion, BindingUID: binding, ProjectUID: project, AuthorityUID: authority,
			SenderInstanceUID: authority, ReceiverInstanceUID: entity, Epoch: 1, Sequence: 1, Stream: RelayStreamEvent,
			Path: []string{authority}, SourceUID: entity, SourceHash: hash, Body: []byte("{}"),
		})
		require.NoError(t, err)
		raw, err := json.Marshal(envelope)
		require.NoError(t, err)
		return &RelayOutboxExport{ID: 1, ProjectUID: project, BindingUID: binding, Stream: RelayStreamEvent, ResetEpoch: 1, SourceUID: entity, SourceHash: hash, Envelope: string(raw), Emitted: true}
	}
	validRelayEnrollment := func() *FederationEnrollmentExport {
		return &FederationEnrollmentExport{TokenHash: hash, SpokeInstanceUID: authority, Capabilities: "claim,pull,push", Actor: "member", ProjectID: &projectID, RelayProtocolVersion: RelayProtocolVersion, RelayBindingUID: &binding, ParentTokenID: &parent, RelayResetEpoch: 1}
	}
	validLegacyEnrollment := func() *FederationEnrollmentExport {
		return &FederationEnrollmentExport{TokenHash: hash, SpokeInstanceUID: authority, Capabilities: "pull", Actor: "member", ProjectID: &projectID}
	}

	for name, build := range map[string]func() ImportRecord{
		"team without a name":                func() ImportRecord { r := validTeam(); r.Name = "   "; return r },
		"team with a short uid":              func() ImportRecord { r := validTeam(); r.UID = "short"; return r },
		"team at revision zero":              func() ImportRecord { r := validTeam(); r.Revision = 0; return r },
		"membership without an actor":        func() ImportRecord { r := validMembership(); r.Actor = " "; return r },
		"policy with unknown visibility":     func() ImportRecord { r := validPolicy(); r.Visibility = "private"; return r },
		"policy with teams under all":        func() ImportRecord { r := validPolicy(); r.Visibility = "all"; return r },
		"policy at revision zero":            func() ImportRecord { r := validPolicy(); r.Revision = 0; return r },
		"creation provenance of a label":     func() ImportRecord { r := validProvenance(); r.Kind = "label"; return r },
		"root pin with a short authority":    func() ImportRecord { r := validPin(); r.AuthorityUID = "short"; return r },
		"root pin with a mismatched key id":  func() ImportRecord { r := validPin(); r.KeyID = hash; return r },
		"cursor accepted beyond offered":     func() ImportRecord { r := validCursor(); r.AcceptedThrough = 3; return r },
		"cursor acknowledged beyond emitted": func() ImportRecord { r := validCursor(); r.AcknowledgedThrough = 3; return r },
		"cursor with an unknown stream":      func() ImportRecord { r := validCursor(); r.Stream = "labels"; return r },
		"cursor at epoch zero":               func() ImportRecord { r := validCursor(); r.ResetEpoch = 0; return r },
		"outbox acknowledged before emitted": func() ImportRecord { r := validOutbox(); r.Emitted, r.Acknowledged = false, true; return r },
		"outbox with a changed source hash":  func() ImportRecord { r := validOutbox(); r.SourceHash = strings.Repeat("b", 64); return r },
		"relay enrollment without a binding": func() ImportRecord { r := validRelayEnrollment(); r.RelayBindingUID = nil; return r },
		"relay enrollment without a parent":  func() ImportRecord { r := validRelayEnrollment(); r.ParentTokenID = nil; return r },
		"relay enrollment at epoch zero":     func() ImportRecord { r := validRelayEnrollment(); r.RelayResetEpoch = -1; return r },
		"legacy enrollment with a binding":   func() ImportRecord { r := validLegacyEnrollment(); r.RelayBindingUID = &binding; return r },
		"legacy enrollment serving downstream": func() ImportRecord {
			r := validLegacyEnrollment()
			r.RelayServeDownstream = true
			return r
		},
		"enrollment with a future protocol": func() ImportRecord { r := validLegacyEnrollment(); r.RelayProtocolVersion = 2; return r },
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateImportRecord(build()))
		})
	}
	for _, record := range []ImportRecord{validTeam(), validMembership(), validPolicy(), validProvenance(), validPin(), validCursor(), validOutbox(), validRelayEnrollment(), validLegacyEnrollment()} {
		require.NoError(t, validateImportRecord(record), "%T baseline must validate so each case changes exactly one field", record)
	}
}

func TestRetainedArtifactBytesAreBounded(t *testing.T) {
	require.NoError(t, ValidateRetainedArtifactBytes([]byte("{}"), []byte("{}")))
	require.Error(t, ValidateRetainedArtifactBytes(nil, []byte("{}")), "an empty manifest is refused")
	require.Error(t, ValidateRetainedArtifactBytes(make([]byte, MaxRetainedArtifactManifestBytes+1), []byte("{}")))
	require.Error(t, ValidateRetainedArtifactBytes([]byte("{}"), make([]byte, MaxRetainedArtifactBytes+1)))
}
