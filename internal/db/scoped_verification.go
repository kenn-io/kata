package db

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

// scopedTranscript is for derived typed assertions only. Native originals and
// authenticated submissions keep their original bytes and use their raw codecs.
func scopedTranscript(domain string, value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	raw, err = ScopedSigningBytes(domain, raw)
	if err != nil {
		return nil, err
	}
	canonical := jsontext.Value(raw)
	if err = canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, err
	}
	return canonical, nil
}

// Only the outer endorsement array is cleared. json/v2 encodes nil slices as
// [], which is part of this transcript; nested endorsements remain committed.
func ScopedCheckpointSigningBytes(c ScopedCheckpoint) ([]byte, error) {
	c.Signatures = nil
	return scopedTranscript(ScopedCheckpointDomain, c)
}
func ScopedBatchSigningBytes(b ScopedBatch) ([]byte, error) {
	b.Signatures = nil
	return scopedTranscript(ScopedBatchDomain, b)
}
func ScopedReferenceSigningBytes(v ScopedViewDescriptor, r ScopedReference) ([]byte, error) {
	r.Signatures = nil
	return scopedTranscript(ScopedReferenceDomain, struct {
		View      ScopedViewDescriptor `json:"view"`
		Reference ScopedReference      `json:"reference"`
	}{v, r})
}
func ScopedProvenanceSigningBytes(v ScopedViewDescriptor, p ScopedProvenance) ([]byte, error) {
	p.Signatures = nil
	return scopedTranscript(ScopedProvenanceDomain, struct {
		View       ScopedViewDescriptor `json:"view"`
		Provenance ScopedProvenance     `json:"provenance"`
	}{v, p})
}
func ScopedSourceResultSigningBytes(r SourceResult) ([]byte, error) {
	r.Signatures = nil
	return scopedTranscript(ScopedResultDomain, r)
}
func ScopedSourceCoverageSigningBytes(c SourceCoverage) ([]byte, error) {
	c.Signatures = nil
	return scopedTranscript(ScopedCoverageDomain, c)
}
func ScopedCheckpointDigest(c ScopedCheckpoint) (string, error) {
	c.Cut.Digest = ""
	raw, err := ScopedCheckpointSigningBytes(c)
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}
func ScopedBatchDigest(b ScopedBatch) (string, error) {
	b.Cut.Digest = ""
	raw, err := ScopedBatchSigningBytes(b)
	if err != nil {
		return "", err
	}
	return ScopedOfferedDigest(raw), nil
}

// ScopedEffectProjectUID validates the canonical stable key against the closed
// typed replacement body, or parses a removal's project/entity identity. Label
// and reference-slot suffixes are free form and are never split into IDs.
// Native apply still checks that a removal belongs to its retained owner.
func ScopedEffectProjectUID(e ScopedEffect) (string, error) {
	if err := e.Validate(); err != nil {
		return "", err
	}
	parts := strings.SplitN(e.Key, ":", 2)
	project := parts[0]
	if !uid.Valid(project) {
		return "", errors.New("invalid scoped effect project key")
	}
	var expected string
	switch e.Collection {
	case "projects":
		if len(parts) != 1 {
			return "", errors.New("noncanonical scoped project key")
		}
		if e.Project != nil {
			expected = e.Project.UID
		}
	case "definitions":
		key := strings.Split(e.Key, ":")
		if len(key) != 3 {
			return "", errors.New("invalid typed scoped definition key")
		}
		canonical, err := ScopedDefinitionKey(key[0], key[1], key[2])
		if err != nil || canonical != e.Key {
			return "", errors.New("invalid typed scoped definition key")
		}
		if e.Definition != nil {
			expected, err = ScopedDefinitionKey(e.Definition.ProjectUID, e.Definition.Kind, e.Definition.UID)
			if err != nil {
				return "", err
			}
		}
	case "issues", "comments":
		if len(parts) != 2 || !uid.Valid(parts[1]) {
			return "", errors.New("invalid project-qualified scoped entity key")
		}
		switch {
		case e.Issue != nil:
			expected = e.Issue.ProjectUID + ":" + e.Issue.UID
		case e.Comment != nil:
			expected = e.Comment.ProjectUID + ":" + e.Comment.UID
		}
	case "labels":
		key := strings.SplitN(e.Key, ":", 3)
		if len(key) != 3 || !uid.Valid(key[1]) || key[2] == "" {
			return "", errors.New("invalid scoped label key")
		}
		if e.Label != nil {
			expected = e.Label.ProjectUID + ":" + e.Label.IssueUID + ":" + e.Label.Label
		}
	case "references":
		key := strings.SplitN(e.Key, ":", 4)
		if len(key) != 4 || key[3] == "" || (key[1] == "" && key[2] == "") || (key[1] != "" && !uid.Valid(key[1])) || (key[2] != "" && !uid.Valid(key[2])) {
			return "", errors.New("invalid scoped reference key")
		}
		if e.Reference != nil {
			r := e.Reference
			expected = r.Subject.ProjectUID + ":" + r.Subject.IssueUID + ":" + r.SubjectEntityUID + ":" + r.Slot
		}
	case "provenance":
		key := strings.SplitN(e.Key, ":", 3)
		if len(key) != 3 || key[1] == "" || !uid.Valid(key[2]) {
			return "", errors.New("invalid scoped provenance key")
		}
		if e.Provenance != nil {
			p := e.Provenance
			expected = p.ProjectUID + ":" + p.Kind + ":" + p.EntityUID
		}
	case "artifacts":
		if len(parts) != 2 || !scopedDigest(parts[1]) {
			return "", errors.New("invalid scoped artifact key")
		}
		if e.Artifact != nil {
			expected = e.Artifact.ProjectUID + ":" + e.Artifact.Digest
		}
	default:
		return "", errors.New("unknown scoped effect key collection")
	}
	if e.Action == "replace" && expected != e.Key {
		return "", errors.New("scoped replacement key differs from typed body")
	}
	return project, nil
}

// ScopedBatchComponentDigests commits only represented effect components, not
// full-state snapshots. Nested endorsements and transport revisions stay in the
// transcript. Grouping/sorting copies slices and never mutates caller input.
func ScopedBatchComponentDigests(effects []ScopedEffect) ([]ScopedComponentDigest, error) {
	type component struct{ project, collection string }
	groups := map[component][]ScopedEffect{}
	seen := map[string]bool{}
	for _, effect := range effects {
		project, err := ScopedEffectProjectUID(effect)
		if err != nil {
			return nil, err
		}
		identity := effect.Collection + ":" + effect.Key
		if seen[identity] {
			return nil, errors.New("duplicate scoped effect identity or disposition")
		}
		seen[identity] = true
		key := component{project, effect.Collection}
		groups[key] = append(groups[key], effect)
	}
	result := make([]ScopedComponentDigest, 0, len(groups))
	for key, group := range groups {
		slices.SortFunc(group, func(a, b ScopedEffect) int { return strings.Compare(a.Key, b.Key) })
		raw, err := scopedTranscript(ScopedEntityDomain, struct {
			Kind       string         `json:"kind"`
			ProjectUID string         `json:"project_uid"`
			Collection string         `json:"collection"`
			Effects    []ScopedEffect `json:"effects"`
		}{"batch_effect_component", key.project, key.collection, group})
		if err != nil {
			return nil, err
		}
		result = append(result, ScopedComponentDigest{ProjectUID: key.project, Collection: key.collection, Digest: ScopedOfferedDigest(raw)})
	}
	slices.SortFunc(result, func(a, b ScopedComponentDigest) int {
		if n := strings.Compare(a.ProjectUID, b.ProjectUID); n != 0 {
			return n
		}
		return strings.Compare(a.Collection, b.Collection)
	})
	return result, nil
}

type scopedPinIdentity struct{ project, key string }
type scopedVerificationPins map[scopedPinIdentity]RootKeyPin

// The caller obtains this set from enrolled native state/authenticated history.
// Embedded descriptor keys and KeyHistory never add trust to it.
func scopedTrustedPins(pins []RootKeyPin) (scopedVerificationPins, error) {
	out := scopedVerificationPins{}
	for _, p := range pins {
		if err := ValidateRootKeyPin(p); err != nil {
			return nil, err
		}
		id := scopedPinIdentity{p.ProjectUID, p.KeyID}
		if _, exists := out[id]; exists {
			return nil, errors.New("duplicate trusted scoped pin")
		}
		out[id] = p
	}
	return out, nil
}
func (pins scopedVerificationPins) get(project, key, authority string) (RootKeyPin, error) {
	p, ok := pins[scopedPinIdentity{project, key}]
	if !ok || p.AuthorityUID != authority {
		return RootKeyPin{}, errors.New("scoped endorsement has no enrolled historical pin")
	}
	return p, nil
}
func scopedExpectedView(actual, expected ScopedViewDescriptor, pins scopedVerificationPins) error {
	if err := actual.Validate(); err != nil {
		return err
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("scoped descriptor differs from staged expectation")
	}
	for _, entry := range expected.Projects {
		trusted, err := pins.get(entry.ProjectUID, entry.Pin.KeyID, expected.AuthorityUID)
		if err != nil {
			return err
		}
		if entry.Pin.AuthorityUID != trusted.AuthorityUID || !bytes.Equal(entry.Pin.PublicKey, trusted.PublicKey) {
			return errors.New("scoped descriptor pin differs from enrollment")
		}
	}
	return nil
}
func scopedViewProjects(v ScopedViewDescriptor) []string {
	out := make([]string, 0, len(v.Projects))
	for _, p := range v.Projects {
		out = append(out, p.ProjectUID)
	}
	return out
}
func scopedProjectInView(v ScopedViewDescriptor, id string) error {
	for _, p := range v.Projects {
		if p.ProjectUID == id {
			return nil
		}
	}
	return errors.New("scoped fact outside expected project scope")
}

// A closed sorted endorsement list prevents extra/duplicate claims from being
// ignored and gives signing/verification a deterministic portable encoding.
func verifyScopedEndorsements(raw []byte, signatures []ScopedSignature, required []string, authority string, pins scopedVerificationPins, view *ScopedViewDescriptor) error {
	required = slices.Clone(required)
	slices.Sort(required)
	required = slices.Compact(required)
	if len(required) == 0 || len(signatures) == 0 || len(signatures) > len(required) {
		return errors.New("missing or extra scoped endorsements")
	}
	var endorsed []RootKeyPin
	previous := ""
	for _, sig := range signatures {
		identity := sig.ProjectUID + ":" + sig.KeyID
		if identity <= previous || !slices.Contains(required, sig.ProjectUID) {
			return errors.New("unordered, off-scope or duplicate scoped endorsement")
		}
		previous = identity
		if view != nil {
			var key string
			for _, p := range view.Projects {
				if p.ProjectUID == sig.ProjectUID {
					key = p.Pin.KeyID
					break
				}
			}
			if key == "" || key != sig.KeyID {
				return errors.New("scoped endorsement key differs from expected descriptor")
			}
		}
		pin, err := pins.get(sig.ProjectUID, sig.KeyID, authority)
		if err != nil {
			return err
		}
		signature, err := base64.StdEncoding.DecodeString(sig.Signature)
		if err != nil || len(signature) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(signature) != sig.Signature {
			return errors.New("invalid scoped signature encoding")
		}
		if !ed25519.Verify(ed25519.PublicKey(pin.PublicKey), raw, signature) {
			return errors.New("invalid scoped signature")
		}
		endorsed = append(endorsed, pin)
	}
	// Endorsement authority is per project, not per signature entry. A single
	// signature may cover independent enrolled project pins sharing the SAME
	// actual public key and authority. Key-ID/actor labels alone confer nothing.
	for _, project := range required {
		covered := false
		for _, trusted := range pins {
			if trusted.ProjectUID != project || trusted.AuthorityUID != authority {
				continue
			}
			if view != nil {
				expectedKey := ""
				for _, p := range view.Projects {
					if p.ProjectUID == project {
						expectedKey = p.Pin.KeyID
						break
					}
				}
				if trusted.KeyID != expectedKey {
					continue
				}
			}
			for _, signer := range endorsed {
				if signer.AuthorityUID == trusted.AuthorityUID && signer.KeyID == trusted.KeyID && bytes.Equal(signer.PublicKey, trusted.PublicKey) {
					covered = true
					break
				}
			}
		}
		if !covered {
			return errors.New("missing scoped project endorsement")
		}
	}
	return nil
}

func VerifyScopedReference(view ScopedViewDescriptor, r ScopedReference, trusted []RootKeyPin) error {
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	if err = scopedExpectedView(view, view, pins); err != nil {
		return err
	}
	return verifyScopedReference(view, r, pins)
}
func verifyScopedReference(view ScopedViewDescriptor, r ScopedReference, pins scopedVerificationPins) error {
	if err := r.Validate(); err != nil {
		return err
	}
	required := []string{r.Subject.ProjectUID}
	if r.Present {
		if r.Close != nil {
			required = append(required, r.Close.PeerProjectUID)
		} else if r.Peer != nil {
			required = append(required, r.Peer.ProjectUID)
		}
	}
	for _, project := range required {
		if err := scopedProjectInView(view, project); err != nil {
			return err
		}
	}
	raw, err := ScopedReferenceSigningBytes(view, r)
	if err != nil {
		return err
	}
	return verifyScopedEndorsements(raw, r.Signatures, required, view.AuthorityUID, pins, &view)
}
func VerifyScopedProvenance(view ScopedViewDescriptor, p ScopedProvenance, trusted []RootKeyPin) error {
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	if err = scopedExpectedView(view, view, pins); err != nil {
		return err
	}
	return verifyScopedProvenance(view, p, pins)
}
func verifyScopedProvenance(view ScopedViewDescriptor, p ScopedProvenance, pins scopedVerificationPins) error {
	if !uid.Valid(p.EntityUID) || p.AttributionStatus == "" {
		return errors.New("invalid scoped provenance identity")
	}
	switch p.Kind {
	case "project":
		if p.EntityUID != p.ProjectUID {
			return errors.New("scoped project provenance identity mismatch")
		}
	case "issue", "comment", "cron_job", "cron_workflow":
	default:
		return errors.New("unknown scoped provenance family")
	}
	if err := scopedProjectInView(view, p.ProjectUID); err != nil {
		return err
	}
	raw, err := ScopedProvenanceSigningBytes(view, p)
	if err != nil {
		return err
	}
	return verifyScopedEndorsements(raw, p.Signatures, []string{p.ProjectUID}, view.AuthorityUID, pins, &view)
}

func verifyScopedCollections(view ScopedViewDescriptor, c ScopedCollections, pins scopedVerificationPins) error {
	if err := c.Validate(); err != nil {
		return err
	}
	var projects []string
	for _, x := range c.Projects {
		projects = append(projects, x.UID)
	}
	for _, x := range c.Issues {
		projects = append(projects, x.ProjectUID)
	}
	for _, x := range c.Comments {
		projects = append(projects, x.ProjectUID)
	}
	for _, x := range c.Labels {
		projects = append(projects, x.ProjectUID)
	}
	for _, x := range c.Definitions {
		projects = append(projects, x.ProjectUID)
	}
	for _, x := range c.Artifacts {
		projects = append(projects, x.ProjectUID)
	}
	for _, id := range projects {
		if err := scopedProjectInView(view, id); err != nil {
			return err
		}
	}
	for _, r := range c.References {
		if err := verifyScopedReference(view, r, pins); err != nil {
			return err
		}
	}
	for _, p := range c.Provenance {
		if err := verifyScopedProvenance(view, p, pins); err != nil {
			return err
		}
	}
	return nil
}

func VerifyScopedCheckpoint(c ScopedCheckpoint, expected ScopedViewDescriptor, trusted []RootKeyPin) error {
	if err := c.Validate(); err != nil {
		return err
	}
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	if err = scopedExpectedView(c.View, expected, pins); err != nil {
		return err
	}
	if err = verifyScopedCollections(expected, c.Collections, pins); err != nil {
		return err
	}
	for _, transition := range c.KeyHistory {
		previous, err := pins.get(transition.Next.ProjectUID, transition.PreviousKeyID, expected.AuthorityUID)
		if err != nil {
			return err
		}
		next, err := pins.get(transition.Next.ProjectUID, transition.Next.KeyID, expected.AuthorityUID)
		if err != nil {
			return err
		}
		if err = scopedProjectInView(expected, next.ProjectUID); err != nil {
			return err
		}
		if !bytes.Equal(next.PublicKey, transition.Next.PublicKey) {
			return errors.New("untrusted scoped key transition")
		}
		if err = VerifyRootKeyTransition(previous, transition); err != nil {
			return err
		}
	}
	digest, err := ScopedCheckpointDigest(c)
	if err != nil {
		return err
	}
	if digest != c.Cut.Digest {
		return errors.New("scoped checkpoint digest mismatch")
	}
	raw, err := ScopedCheckpointSigningBytes(c)
	if err != nil {
		return err
	}
	return verifyScopedEndorsements(raw, c.Signatures, scopedViewProjects(expected), expected.AuthorityUID, pins, &expected)
}

func VerifyScopedBatch(b ScopedBatch, expected ScopedViewDescriptor, trusted []RootKeyPin) error {
	if len(b.Effects) == 0 {
		return errors.New("empty scoped batch")
	}
	if err := b.Validate(); err != nil {
		return err
	}
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	if err = scopedExpectedView(b.View, expected, pins); err != nil {
		return err
	}
	components, err := ScopedBatchComponentDigests(b.Effects)
	if err != nil {
		return err
	}
	if !slices.Equal(components, b.ComponentDigests) {
		return errors.New("scoped batch effect-component inventory mismatch")
	}
	for _, component := range b.ComponentDigests {
		if err = scopedProjectInView(expected, component.ProjectUID); err != nil {
			return err
		}
	}
	for _, effect := range b.Effects {
		project, err := ScopedEffectProjectUID(effect)
		if err != nil {
			return err
		}
		if err = scopedProjectInView(expected, project); err != nil {
			return err
		}
		if effect.Action == "remove" {
			continue
		} // Stable-key ownership remains a native apply duty.
		c := scopedEffectCollections(effect)
		if err = verifyScopedCollections(expected, c, pins); err != nil {
			return err
		}
	}

	digest, err := ScopedBatchDigest(b)
	if err != nil {
		return err
	}
	if digest != b.Cut.Digest {
		return errors.New("scoped batch digest mismatch")
	}
	raw, err := ScopedBatchSigningBytes(b)
	if err != nil {
		return err
	}
	return verifyScopedEndorsements(raw, b.Signatures, scopedViewProjects(expected), expected.AuthorityUID, pins, &expected)
}

// scopedEffectCollections maps a previously validated replacement effect.
func scopedEffectCollections(effect ScopedEffect) ScopedCollections {
	c := ScopedCollections{Projects: []ScopedProjectState{}, Issues: []ScopedIssueState{}, Comments: []ScopedCommentState{}, Labels: []ScopedLabelState{}, Definitions: []ScopedDefinitionState{}, References: []ScopedReference{}, Provenance: []ScopedProvenance{}, Artifacts: []embedding.ArtifactManifest{}}
	switch {
	case effect.Project != nil:
		c.Projects = append(c.Projects, *effect.Project)
	case effect.Issue != nil:
		c.Issues = append(c.Issues, *effect.Issue)
	case effect.Comment != nil:
		c.Comments = append(c.Comments, *effect.Comment)
	case effect.Label != nil:
		c.Labels = append(c.Labels, *effect.Label)
	case effect.Definition != nil:
		c.Definitions = append(c.Definitions, *effect.Definition)
	case effect.Reference != nil:
		c.References = append(c.References, *effect.Reference)
	case effect.Provenance != nil:
		c.Provenance = append(c.Provenance, *effect.Provenance)
	case effect.Artifact != nil:
		c.Artifacts = append(c.Artifacts, *effect.Artifact)
	}
	return c
}

func scopedOperationProjects(op SourceOperation) ([]string, error) {
	if validateSourceOperationVersion(op) != nil || !uid.Valid(op.UID) || !uid.Valid(op.SourceInstanceUID) || op.Version != 3 && len(op.Members) == 0 || SourceCommandClass(op.CommandKind).Disposition != "source" {
		return nil, errors.New("invalid expected scoped operation")
	}
	if err := op.Principal.Validate(); err != nil {
		return nil, err
	}
	digest, err := SourceOperationDigest(op)
	if err != nil || digest != op.Digest {
		return nil, errors.New("expected scoped operation digest mismatch")
	}
	if op.Version == 3 {
		return []string{op.ImportIntent.ProjectUID}, nil
	}
	seen := map[string]bool{}
	var projects []string
	for i, m := range op.Members {
		if !uid.Valid(m.ProjectUID) || !uid.Valid(m.EventUID) || !scopedDigest(m.ContentHash) || m.Ordinal != i || seen[m.EventUID] {
			return nil, errors.New("invalid expected scoped operation members")
		}
		seen[m.EventUID] = true
		projects = append(projects, m.ProjectUID)
	}
	return projects, nil
}

func VerifyScopedSourceResult(r SourceResult, expected SourceOperation, trusted []RootKeyPin) error {
	if err := r.Validate(); err != nil {
		return err
	}
	projects, err := scopedOperationProjects(expected)
	if err != nil {
		return err
	}
	if (expected.Version == 3) != (r.ImportOutcome != nil) {
		return errors.New("scoped result arm differs from expected operation version")
	}
	if r.OperationUID != expected.UID || r.OperationDigest != expected.Digest || !slices.Equal(r.Members, expected.Members) {
		return errors.New("scoped result differs from expected manifest")
	}
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	if r.State == "accepted" {
		if len(r.Receipts) != len(expected.Members) {
			return errors.New("accepted scoped result has incomplete receipts")
		}
		seen := map[string]bool{}
		for _, receipt := range r.Receipts {
			found := false
			for _, member := range expected.Members {
				if receipt.EventUID == member.EventUID && receipt.ProjectUID == member.ProjectUID && receipt.ContentHash == member.ContentHash {
					found = true
					break
				}
			}
			if !found || seen[receipt.EventUID] {
				return errors.New("scoped result receipt differs from manifest")
			}
			seen[receipt.EventUID] = true
			pin, err := pins.get(receipt.ProjectUID, receipt.KeyID, expected.Principal.AuthorityUID)
			if err != nil {
				return err
			}
			if err = VerifyRootReceipt(pin, receipt); err != nil {
				return err
			}
		}
	}
	raw, err := ScopedSourceResultSigningBytes(r)
	if err != nil {
		return err
	}
	return verifyScopedEndorsements(raw, r.Signatures, projects, expected.Principal.AuthorityUID, pins, nil)
}
func VerifyScopedSourceCoverage(c SourceCoverage, operation SourceOperation, view ScopedViewDescriptor, cut ScopedCut, trusted []RootKeyPin) error {
	// Import representation must be joined to the genuine root execution and
	// eligible coverage. Empty producer members prove no projection coverage.
	if operation.Version == 3 {
		return errors.New("import coverage requires verified root execution coverage")
	}
	projects, err := scopedOperationProjects(operation)
	if err != nil {
		return err
	}
	if err = view.Validate(); err != nil {
		return err
	}
	if err = cut.Validate(); err != nil {
		return err
	}
	if c.OperationUID != operation.UID || c.OperationDigest != operation.Digest || c.View != view.Key || c.Cut != cut || view.AuthorityUID != operation.Principal.AuthorityUID {
		return errors.New("scoped coverage differs from expected operation/view/cut")
	}
	switch c.Outcome {
	case "represented", "unchanged", "outside_scope":
	default:
		return errors.New("unknown scoped coverage outcome")
	}
	pins, err := scopedTrustedPins(trusted)
	if err != nil {
		return err
	}
	if err = scopedExpectedView(view, view, pins); err != nil {
		return err
	}
	raw, err := ScopedSourceCoverageSigningBytes(c)
	if err != nil {
		return err
	}
	if err = verifyScopedEndorsements(raw, c.Signatures, projects, operation.Principal.AuthorityUID, pins, nil); err != nil {
		return fmt.Errorf("scoped coverage: %w", err)
	}
	return nil
}
