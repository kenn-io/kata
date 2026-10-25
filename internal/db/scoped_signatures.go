package db

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"slices"
)

// signScopedProjects uses only the selected native project keys. Every required
// project must have matching loaded material; independently pinned projects
// sharing the exact same authority/key need only one wire endorsement.
func signScopedProjects(view ScopedViewDescriptor, projects []string, raw []byte, signers RootSigners) ([]ScopedSignature, error) {
	if err := view.Validate(); err != nil {
		return nil, err
	}
	projects = slices.Clone(projects)
	slices.Sort(projects)
	projects = slices.Compact(projects)
	result := []ScopedSignature{}
	signed := map[string]bool{}
	for _, project := range projects {
		var pin RootKeyPin
		found := false
		for _, p := range view.Projects {
			if p.ProjectUID == project {
				pin = p.Pin
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("cannot sign project outside scoped view")
		}
		if err := ValidateRootKeyPin(pin); err != nil {
			return nil, err
		}
		signer, ok := signers[project]
		if !ok || !RootSignerMatches(pin, signer) || !bytes.Equal(pin.PublicKey, signer.PrivateKey.Public().(ed25519.PublicKey)) {
			return nil, &RootSigningKeyError{Project: project}
		}
		key := pin.AuthorityUID + ":" + pin.KeyID
		if signed[key] {
			continue
		}
		signed[key] = true
		result = append(result, ScopedSignature{ProjectUID: project, KeyID: pin.KeyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(signer.PrivateKey, raw))})
	}
	if len(result) == 0 {
		return nil, errors.New("scoped signing requires project authority")
	}
	return result, nil
}
func scopedSigningPins(view ScopedViewDescriptor) []RootKeyPin {
	pins := make([]RootKeyPin, 0, len(view.Projects))
	for _, p := range view.Projects {
		pins = append(pins, p.Pin)
	}
	return pins
}

// SignScopedReference endorses one subject-owned slot under the exact view.
// Present values require both endpoint authorities; null names only subject.
func SignScopedReference(view ScopedViewDescriptor, r ScopedReference, signers RootSigners) (ScopedReference, error) {
	if err := r.Validate(); err != nil {
		return ScopedReference{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return ScopedReference{}, err
	}
	var result ScopedReference
	if err = json.Unmarshal(raw, &result); err != nil {
		return ScopedReference{}, err
	}
	projects := []string{r.Subject.ProjectUID}
	if r.Present {
		if r.Close != nil {
			projects = append(projects, r.Close.PeerProjectUID)
		} else if r.Peer != nil {
			projects = append(projects, r.Peer.ProjectUID)
		}
	}
	transcript, err := ScopedReferenceSigningBytes(view, result)
	if err != nil {
		return ScopedReference{}, err
	}
	result.Signatures, err = signScopedProjects(view, projects, transcript, signers)
	if err != nil {
		return ScopedReference{}, err
	}
	if err = VerifyScopedReference(view, result, scopedSigningPins(view)); err != nil {
		return ScopedReference{}, err
	}
	return result, nil
}

// SignScopedProvenance preserves the creator and legacy/unverified labels.
// Signing a current attestation never manufactures an original source receipt.
func SignScopedProvenance(view ScopedViewDescriptor, p ScopedProvenance, signers RootSigners) (ScopedProvenance, error) {
	transcript, err := ScopedProvenanceSigningBytes(view, p)
	if err != nil {
		return ScopedProvenance{}, err
	}
	p.Signatures, err = signScopedProjects(view, []string{p.ProjectUID}, transcript, signers)
	if err != nil {
		return ScopedProvenance{}, err
	}
	if err = VerifyScopedProvenance(view, p, scopedSigningPins(view)); err != nil {
		return ScopedProvenance{}, err
	}
	return p, nil
}

// SignScopedCheckpoint signs a frozen complete current state. KeyHistory must
// come from authenticated native history: this API has no historical trust
// parameter. It preserves that history exactly and checks its structural scope;
// recipients use VerifyScopedCheckpoint with their enrolled/historical pins.
func SignScopedCheckpoint(c ScopedCheckpoint, signers RootSigners) (ScopedCheckpoint, error) {
	if err := c.View.Validate(); err != nil {
		return ScopedCheckpoint{}, err
	}
	if err := c.Collections.Validate(); err != nil {
		return ScopedCheckpoint{}, err
	}
	projectsInView := make([]string, 0, len(c.View.Projects))
	for _, project := range c.View.Projects {
		projectsInView = append(projectsInView, project.ProjectUID)
	}
	permitted, err := RestrictScopedState(c.Collections, projectsInView, nil)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	suppliedDigest, err := ScopedStateDigest(c.Collections)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	permittedDigest, err := ScopedStateDigest(permitted)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	if suppliedDigest != permittedDigest {
		return ScopedCheckpoint{}, errors.New("checkpoint contains off-scope or ineligible current content")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	var result ScopedCheckpoint
	if err = json.Unmarshal(raw, &result); err != nil {
		return ScopedCheckpoint{}, err
	}
	result.Collections, err = normalizeScopedCollections(result.Collections)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	seen := map[string]bool{}
	for _, transition := range result.KeyHistory {
		id := transition.Next.ProjectUID + ":" + transition.Next.KeyID
		if transition.Version != 1 || !scopedDigest(transition.PreviousKeyID) || transition.Next.AuthorityUID != result.View.AuthorityUID || len(transition.Signature) != ed25519.SignatureSize || seen[id] {
			return ScopedCheckpoint{}, errors.New("invalid scoped checkpoint key history")
		}
		seen[id] = true
		inside := false
		for _, p := range result.View.Projects {
			inside = inside || p.ProjectUID == transition.Next.ProjectUID
		}
		if !inside {
			return ScopedCheckpoint{}, errors.New("key history outside scoped view")
		}
		if err = ValidateRootKeyPin(transition.Next); err != nil {
			return ScopedCheckpoint{}, err
		}
	}
	for n, r := range result.Collections.References {
		signed, err := SignScopedReference(result.View, r, signers)
		if err != nil {
			return ScopedCheckpoint{}, err
		}
		result.Collections.References[n] = signed
	}
	for n, p := range result.Collections.Provenance {
		signed, err := SignScopedProvenance(result.View, p, signers)
		if err != nil {
			return ScopedCheckpoint{}, err
		}
		result.Collections.Provenance[n] = signed
	}
	result.Cut.Digest, err = ScopedCheckpointDigest(result)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	if err = result.Validate(); err != nil {
		return ScopedCheckpoint{}, err
	}
	projects := []string{}
	for _, p := range result.View.Projects {
		projects = append(projects, p.ProjectUID)
	}
	transcript, err := ScopedCheckpointSigningBytes(result)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	result.Signatures, err = signScopedProjects(result.View, projects, transcript, signers)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return ScopedCheckpoint{}, err
	}
	if err = ValidateScopedCheckpointSize(int64(len(raw))); err != nil {
		return ScopedCheckpoint{}, err
	}
	return result, nil
}

// SignScopedBatch signs a fresh frozen set of visible effects, not retained
// delivery bytes. Production retries reuse the exact retained signed envelope.
// ProjectScopedState/DiffScopedState establish content eligibility before this
// step; the native installer checks removal ownership against its prior rows.
func SignScopedBatch(b ScopedBatch, signers RootSigners) (ScopedBatch, error) {
	if err := b.View.Validate(); err != nil {
		return ScopedBatch{}, err
	}
	if len(b.Effects) == 0 {
		return ScopedBatch{}, errors.New("cannot sign an empty visible batch")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return ScopedBatch{}, err
	}
	var result ScopedBatch
	if err = json.Unmarshal(raw, &result); err != nil {
		return ScopedBatch{}, err
	}
	slices.SortFunc(result.Effects, func(a, b ScopedEffect) int {
		if a.Collection < b.Collection {
			return -1
		}
		if a.Collection > b.Collection {
			return 1
		}
		if a.Key < b.Key {
			return -1
		}
		if a.Key > b.Key {
			return 1
		}
		return 0
	})
	for n, effect := range result.Effects {
		if err = effect.Validate(); err != nil {
			return ScopedBatch{}, err
		}
		if effect.Revision != result.Cut.Sequence {
			return ScopedBatch{}, errors.New("effect position differs from scoped batch")
		}
		if effect.Reference != nil {
			if effect.Reference.Revision != effect.Revision {
				return ScopedBatch{}, errors.New("reference position differs from atomic effect")
			}
			signed, err := SignScopedReference(result.View, *effect.Reference, signers)
			if err != nil {
				return ScopedBatch{}, err
			}
			result.Effects[n].Reference = &signed
		}
		if effect.Provenance != nil {
			signed, err := SignScopedProvenance(result.View, *effect.Provenance, signers)
			if err != nil {
				return ScopedBatch{}, err
			}
			result.Effects[n].Provenance = &signed
		}
	}
	result.ComponentDigests, err = ScopedBatchComponentDigests(result.Effects)
	if err != nil {
		return ScopedBatch{}, err
	}
	result.Cut.Digest, err = ScopedBatchDigest(result)
	if err != nil {
		return ScopedBatch{}, err
	}
	if err = result.Validate(); err != nil {
		return ScopedBatch{}, err
	}
	projects := []string{}
	for _, project := range result.View.Projects {
		projects = append(projects, project.ProjectUID)
	}
	transcript, err := ScopedBatchSigningBytes(result)
	if err != nil {
		return ScopedBatch{}, err
	}
	result.Signatures, err = signScopedProjects(result.View, projects, transcript, signers)
	if err != nil {
		return ScopedBatch{}, err
	}
	if err = VerifyScopedBatch(result, result.View, scopedSigningPins(result.View)); err != nil {
		return ScopedBatch{}, err
	}
	raw, err = json.Marshal(result)
	if err != nil {
		return ScopedBatch{}, err
	}
	if err = ValidateScopedCheckpointSize(int64(len(raw))); err != nil {
		return ScopedBatch{}, err
	}
	return result, nil
}
