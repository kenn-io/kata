//go:build rcpl_private

package db_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"
)

func projectorPins(d db.ScopedViewDescriptor) []db.RootKeyPin {
	p := []db.RootKeyPin{}
	for _, v := range d.Projects {
		p = append(p, v.Pin)
	}
	return p
}

// A present disposition requires both endpoint pins. A null register is the
// same subject-owned fact for a hidden peer or no peer; no hidden key is needed.
func TestScopedCloseNullSubjectSignature(t *testing.T) {
	c, d, keys := projectorFixture(t, 1, 2)
	id := projectorUID(11)
	i := c.Fold.Issues[id]
	i.Status = "closed"
	i.StatusIntentUID = projectorUID(81)
	c.Fold.Issues[id] = i
	c.Close = []db.CanonicalCloseState{{IssueUID: id, StatusIntentUID: i.StatusIntentUID, Value: &db.ScopedCloseValue{Kind: "duplicate", PeerProjectUID: projectorUID(2), PeerIssueUID: projectorUID(12)}}}
	r := reference(t, project(t, c, d), "close", id)
	signed, err := db.SignScopedReference(d, r, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(signed.Signatures) != 2 {
		t.Fatal("present cross-project close lacks both endorsements")
	}
	if err = db.VerifyScopedReference(d, signed, projectorPins(d)); err != nil {
		t.Fatal(err)
	}
	broken := signed
	broken.Signatures = broken.Signatures[:1]
	if db.VerifyScopedReference(d, broken, projectorPins(d)) == nil {
		t.Fatal("distinct peer signature optional")
	}
	// Tamper both directions; either endpoint binding must be committed.
	changed := signed
	value := *signed.Close
	value.Kind = "superseded"
	changed.Close = &value
	if db.VerifyScopedReference(d, changed, projectorPins(d)) == nil {
		t.Fatal("close kind tamper accepted")
	}
	_, narrow, _ := projectorFixture(t, 1)
	null := reference(t, project(t, c, narrow), "close", id)
	onlySubject := db.RootSigners{projectorUID(1): keys[projectorUID(1)]}
	null, err = db.SignScopedReference(narrow, null, onlySubject)
	if err != nil {
		t.Fatal(err)
	}
	if null.Present || null.Close != nil || null.Peer != nil || len(null.Signatures) != 1 || null.Signatures[0].ProjectUID != projectorUID(1) {
		t.Fatal("null register names hidden peer", null)
	}
	if err = db.VerifyScopedReference(narrow, null, projectorPins(narrow)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(projectedBytes(t, null), []byte(projectorUID(2))) {
		t.Fatal("null signature disclosed hidden project")
	}
	if _, err = db.SignScopedReference(d, r, onlySubject); err == nil {
		t.Fatal("missing distinct peer key fell back unsigned")
	}
}

func TestScopedSignatureSharedKeyAndDomain(t *testing.T) {
	c, d, keys := projectorFixture(t, 1, 2)
	r := reference(t, project(t, c, d), "parent", projectorUID(11))
	r.Present = true
	r.Peer = &db.ScopedEndpoint{ProjectUID: projectorUID(2), IssueUID: projectorUID(12)}
	d.Projects[1].Pin.KeyID = d.Projects[0].Pin.KeyID
	d.Projects[1].Pin.PublicKey = bytes.Clone(d.Projects[0].Pin.PublicKey)
	keys[projectorUID(2)] = keys[projectorUID(1)]
	signed, err := db.SignScopedReference(d, r, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(signed.Signatures) != 1 {
		t.Fatal("identical trusted project keys require redundant endorsements")
	}
	if err = db.VerifyScopedReference(d, signed, projectorPins(d)); err != nil {
		t.Fatal(err)
	}
	encoded := signed.Signatures[0].Signature
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.SignatureSize || !strings.HasSuffix(encoded, "==") {
		t.Fatal("signature must be padded base64 Ed25519")
	}
	transcript, err := db.ScopedReferenceSigningBytes(d, signed)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(ed25519.PublicKey(d.Projects[0].Pin.PublicKey), transcript, raw) {
		t.Fatal("signature transcript disagrees")
	}
	changed := d
	changed.Key.Generation++
	if db.VerifyScopedReference(changed, signed, projectorPins(d)) == nil {
		t.Fatal("signature crossed view generation")
	}
	duplicate := signed
	duplicate.Signatures = append(append([]db.ScopedSignature{}, signed.Signatures...), signed.Signatures...)
	if db.VerifyScopedReference(d, duplicate, projectorPins(d)) == nil {
		t.Fatal("duplicate endorsement accepted")
	}
}

func TestScopedSignatureProvenance(t *testing.T) {
	c, d, keys := projectorFixture(t, 1)
	p := project(t, c, d).Provenance[0]
	signed, err := db.SignScopedProvenance(d, p, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.VerifyScopedProvenance(d, signed, projectorPins(d)); err != nil {
		t.Fatal(err)
	}
	if signed.Creator != "author" || signed.AttributionStatus != "legacy" || len(signed.Signatures) != 1 {
		t.Fatal("creation attribution rewritten", signed)
	}
	signed.Creator = "editor"
	if db.VerifyScopedProvenance(d, signed, projectorPins(d)) == nil {
		t.Fatal("creation actor tamper accepted")
	}
	bad := keys[projectorUID(1)]
	bad.AuthorityUID = projectorUID(88)
	keys[projectorUID(1)] = bad
	if _, err = db.SignScopedProvenance(d, p, keys); err == nil {
		t.Fatal("wrong authority signed provenance")
	}
}

func TestScopedSignatureCheckpoint(t *testing.T) {
	c, d, keys := projectorFixture(t, 1, 2)
	state := project(t, c, d)
	checkpoint := db.ScopedCheckpoint{View: d, Cut: db.ScopedCut{Sequence: 0, CaptureUID: c.CaptureUID}, Collections: state}
	before := projectedBytes(t, checkpoint)
	signed, err := db.SignScopedCheckpoint(checkpoint, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, projectedBytes(t, checkpoint)) {
		t.Fatal("signing mutated retained input")
	}
	if err = db.VerifyScopedCheckpoint(signed, d, projectorPins(d)); err != nil {
		t.Fatal(err)
	}
	again, err := db.SignScopedCheckpoint(checkpoint, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(projectedBytes(t, signed), projectedBytes(t, again)) {
		t.Fatal("same frozen input produced different signed bytes")
	}
	if len(signed.Signatures) != 2 {
		t.Fatal("not all selected project pins endorsed common checkpoint")
	}
	tampered := signed
	tampered.Cut.CaptureUID = projectorUID(96)
	if db.VerifyScopedCheckpoint(tampered, d, projectorPins(d)) == nil {
		t.Fatal("capture identity tamper accepted")
	}
	tampered = signed
	tampered.Collections = project(t, c, d)
	if db.VerifyScopedCheckpoint(tampered, d, projectorPins(d)) == nil {
		t.Fatal("nested endorsement removal not committed")
	}
	untrusted := projectorPins(d)
	untrusted[0].PublicKey = bytes.Clone(untrusted[1].PublicKey)
	if db.VerifyScopedCheckpoint(signed, d, untrusted) == nil {
		t.Fatal("descriptor keys established their own trust")
	}
	delete(keys, projectorUID(2))
	if _, err = db.SignScopedCheckpoint(checkpoint, keys); err == nil {
		t.Fatal("missing selected signer did not stop checkpoint production")
	}
}

func TestScopedSignatureRejectsOffscopeState(t *testing.T) {
	c, d, keys := projectorFixture(t, 1)
	state := project(t, c, d)
	state.Issues = append(state.Issues, db.ScopedIssueState{UID: projectorUID(77), ProjectUID: projectorUID(3), Title: "HIDDEN-SIGNING-STATE-MARKER"})
	checkpoint := db.ScopedCheckpoint{View: d, Cut: db.ScopedCut{CaptureUID: c.CaptureUID}, Collections: state}
	if _, err := db.SignScopedCheckpoint(checkpoint, keys); err == nil {
		t.Fatal("checkpoint signer endorsed off-scope current content")
	}
}

func TestScopedSignatureBatch(t *testing.T) {
	c, d, keys := projectorFixture(t, 1, 2)
	c.Fold.Labels[db.FoldLabelKey{IssueUID: projectorUID(11), Label: "gone"}] = db.FoldElementState{Present: true}
	before := project(t, c, d)
	c.Fold.Labels[db.FoldLabelKey{IssueUID: projectorUID(11), Label: "gone"}] = db.FoldElementState{Present: false}
	c.Fold.Labels[db.FoldLabelKey{IssueUID: projectorUID(11), Label: "ready"}] = db.FoldElementState{Present: true}
	c.Fold.Links[db.FoldLinkKey{FromUID: projectorUID(11), ToUID: projectorUID(12), Type: "parent"}] = db.FoldElementState{Present: true}
	after := project(t, c, d)
	after.Issues[0].Title = "visible edit"
	effects, err := db.DiffScopedState(before, after, 8)
	if err != nil {
		t.Fatal(err)
	}
	batch := db.ScopedBatch{View: d, Cut: db.ScopedCut{Sequence: 8, CaptureUID: projectorUID(96)}, PreviousDigest: strings.Repeat("c", 64), Effects: effects}
	original := projectedBytes(t, batch)
	signed, err := db.SignScopedBatch(batch, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, projectedBytes(t, batch)) {
		t.Fatal("signing batch mutated retained input")
	}
	if err = db.VerifyScopedBatch(signed, d, projectorPins(d)); err != nil {
		t.Fatal(err)
	}
	if len(signed.Effects) != 4 || len(signed.ComponentDigests) != 3 || len(signed.Signatures) != 2 {
		t.Fatalf("incomplete atomic batch: %+v", signed)
	}
	for _, component := range signed.ComponentDigests {
		if component.ProjectUID != projectorUID(1) {
			t.Fatal("unchanged project leaked component activity")
		}
	}
	for _, effect := range signed.Effects {
		if effect.Revision != 8 {
			t.Fatal("nonatomic effect position")
		}
		if effect.Reference != nil && (effect.Reference.Revision != 8 || len(effect.Reference.Signatures) != 2) {
			t.Fatal("reference not endorsed at batch position")
		}
	}
	// Independent input order must yield exact identical signed bytes.
	reversed := batch
	reversed.Effects = append([]db.ScopedEffect{}, batch.Effects...)
	for a, b := 0, len(reversed.Effects)-1; a < b; a, b = a+1, b-1 {
		reversed.Effects[a], reversed.Effects[b] = reversed.Effects[b], reversed.Effects[a]
	}
	again, err := db.SignScopedBatch(reversed, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(projectedBytes(t, signed), projectedBytes(t, again)) {
		t.Fatal("effect input order changed signed batch")
	}
	broken := signed
	broken.ComponentDigests = nil
	if db.VerifyScopedBatch(broken, d, projectorPins(d)) == nil {
		t.Fatal("missing exact component inventory accepted")
	}
	empty := batch
	empty.Effects = []db.ScopedEffect{}
	if _, err = db.SignScopedBatch(empty, keys); err == nil {
		t.Fatal("empty visible batch was signed")
	}
	delete(keys, projectorUID(2))
	if _, err = db.SignScopedBatch(batch, keys); err == nil {
		t.Fatal("common batch manifest did not require all selected project keys")
	}
}

func TestScopedSignatureCheckpointHistory(t *testing.T) {
	c, d, keys := projectorFixture(t, 1)
	old := d.Projects[0].Pin
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	next := db.RootKeyPin{ProjectUID: old.ProjectUID, AuthorityUID: old.AuthorityUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	transition, err := db.SignRootKeyTransition(old, next, keys[old.ProjectUID].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	d.Projects[0].Pin = next
	keys[old.ProjectUID] = db.RootAttributionSigner{AuthorityUID: d.AuthorityUID, PrivateKey: private}
	checkpoint := db.ScopedCheckpoint{View: d, Cut: db.ScopedCut{CaptureUID: c.CaptureUID}, Collections: project(t, c, d), KeyHistory: []db.RootKeyTransition{transition}}
	signed, err := db.SignScopedCheckpoint(checkpoint, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(projectedBytes(t, checkpoint.KeyHistory), projectedBytes(t, signed.KeyHistory)) {
		t.Fatal("signer rewrote authenticated key history")
	}
	if err = db.VerifyScopedCheckpoint(signed, d, []db.RootKeyPin{old, next}); err != nil {
		t.Fatal(err)
	}
	if db.VerifyScopedCheckpoint(signed, d, []db.RootKeyPin{next}) == nil {
		t.Fatal("embedded history established its own previous-key trust")
	}
	bad := checkpoint
	bad.KeyHistory = append([]db.RootKeyTransition{}, checkpoint.KeyHistory...)
	bad.KeyHistory[0].Next.ProjectUID = projectorUID(3)
	if _, err = db.SignScopedCheckpoint(bad, keys); err == nil {
		t.Fatal("key history outside view scope accepted")
	}
}

func TestScopedSignatureContentEquivalence(t *testing.T) {
	c, d, keys := projectorFixture(t, 1)
	unsigned := project(t, c, d)
	signed, err := db.SignScopedCheckpoint(db.ScopedCheckpoint{View: d, Cut: db.ScopedCut{CaptureUID: c.CaptureUID}, Collections: unsigned}, keys)
	if err != nil {
		t.Fatal(err)
	}
	for n := range signed.Collections.References {
		signed.Collections.References[n].Revision = 91
	}
	a, err := db.ScopedStateDigest(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.ScopedStateDigest(signed.Collections)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("transport endorsements/positions changed current-content ETag")
	}
	effects, err := db.DiffScopedState(unsigned, signed.Collections, 0)
	if err != nil || len(effects) != 0 {
		t.Fatal("transport-only changes allocated public effects", effects, err)
	}
}
