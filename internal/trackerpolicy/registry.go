package trackerpolicy

import (
	"errors"
	"slices"

	"go.kenn.io/kata/internal/uid"
)

// Registry errors prevent changed, retired, or unrecordable authority.
var (
	ErrGenerationChanged = errors.New("tracker authority changed without a fresh connection generation")
	ErrGenerationRetired = errors.New("tracker connection generation is retired; choose a fresh generation")
	ErrHistoryFull       = errors.New("tracker retained history is full; new authority is blocked")
)

// RegistryState identifies whether a retained generation is active.
type RegistryState string

// Registry states preserve revocation and restore boundaries.
const (
	RegistryActive   RegistryState = "active"
	RegistryRevoked  RegistryState = "revoked"
	RegistryRestored RegistryState = "restored"
)

// RegistryPolicy is a digest of normalized local configuration, never a token
// fingerprint. Credential VALUES are not read while constructing it.
type RegistryPolicy struct {
	Selection Selection `json:"selection"`
	Provider  string    `json:"provider"`
	Digest    string    `json:"digest"`
}

// RetiredGeneration records a previously used generation and its digest.
type RetiredGeneration struct {
	Generation string `json:"generation"`
	Digest     string `json:"digest"`
}

// RegistryRecord retains used generation names; eviction would reauthorize old
// configurations. Capacity exhaustion blocks new generations instead.
type RegistryRecord struct {
	Version      int                 `json:"version"`
	Policy       RegistryPolicy      `json:"policy"`
	ProcessEpoch string              `json:"process_epoch"`
	State        RegistryState       `json:"state"`
	Retired      []RetiredGeneration `json:"retired,omitempty"`
}

// DigestValid checks the lowercase hexadecimal digest format.
func DigestValid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// Validate checks the selection, provider, and digest.
func (p RegistryPolicy) Validate() error {
	if p.Selection.Validate() != nil || !SupportedProvider(p.Provider) || !DigestValid(p.Digest) {
		return ErrInvalid
	}
	return nil
}

// Validate checks the retained generation history.
func (r RegistryRecord) Validate() error {
	if r.Version != 1 || r.Policy.Validate() != nil || !uid.Valid(r.ProcessEpoch) || (r.State != RegistryActive && r.State != RegistryRevoked && r.State != RegistryRestored) || len(r.Retired) > 4096 {
		return ErrInvalid
	}
	seen := map[string]bool{r.Policy.Selection.Generation: true}
	for _, old := range r.Retired {
		s := r.Policy.Selection
		s.Generation = old.Generation
		if s.Validate() != nil || !DigestValid(old.Digest) || seen[old.Generation] {
			return ErrInvalid
		}
		seen[old.Generation] = true
	}
	return nil
}

// NextRegistryRecord applies a validated policy without reusing retired generations.
func NextRegistryRecord(previous *RegistryRecord, policy *RegistryPolicy, epoch string) (RegistryRecord, error) {
	if !uid.Valid(epoch) || (policy != nil && policy.Validate() != nil) {
		return RegistryRecord{}, ErrInvalid
	}
	if previous == nil {
		if policy == nil {
			return RegistryRecord{}, ErrInvalid
		}
		return RegistryRecord{Version: 1, Policy: *policy, ProcessEpoch: epoch, State: RegistryActive}, nil
	}
	if previous.Validate() != nil {
		return RegistryRecord{}, ErrInvalid
	}
	next := *previous
	next.Retired = slices.Clone(previous.Retired)
	next.ProcessEpoch = epoch
	if policy == nil {
		next.State = RegistryRevoked
		return next, nil
	}
	if policy.Selection.OwnerInstanceUID != previous.Policy.Selection.OwnerInstanceUID || policy.Selection.ConnectionUID != previous.Policy.Selection.ConnectionUID || policy.Provider != previous.Policy.Provider {
		return RegistryRecord{}, ErrInvalid
	}
	if policy.Selection.Generation == previous.Policy.Selection.Generation {
		if previous.State != RegistryActive {
			return RegistryRecord{}, ErrGenerationRetired
		}
		if policy.Digest != previous.Policy.Digest {
			return RegistryRecord{}, ErrGenerationChanged
		}
		return next, nil
	}
	for _, old := range previous.Retired {
		if old.Generation == policy.Selection.Generation {
			return RegistryRecord{}, ErrGenerationRetired
		}
	}
	if len(previous.Retired) >= 4096 {
		return RegistryRecord{}, ErrHistoryFull
	}
	next.Retired = append(next.Retired, RetiredGeneration{Generation: previous.Policy.Selection.Generation, Digest: previous.Policy.Digest})
	next.Policy = *policy
	next.State = RegistryActive
	return next, nil
}
