package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/tracker"
)

// Include owner-local actor mappings that can exercise this connection. Remote
// roots own their independent actor policy; they cannot assign the owner's
// local actor. Irrelevant maps and presentation ordering do not alter authority.
func trackerPolicyWithMappings(policy tracker.RegistryPolicy, c config.TrackerConnectionConfig, mappings []config.TrackerAccessMapping) (tracker.RegistryPolicy, error) {
	relevant := []config.TrackerAccessMapping{}
	for _, m := range mappings {
		if slices.ContainsFunc(c.Grants, func(g tracker.Grant) bool { return g.ProjectUID == m.ProjectUID && g.Principal == m.Principal }) {
			relevant = append(relevant, m)
		}
	}
	if len(relevant) == 0 {
		return policy, nil
	}
	key := func(m config.TrackerAccessMapping) string {
		return m.ProjectUID + "\x00" + m.Principal.AuthorityInstanceUID + "\x00" + string(m.Principal.Kind) + "\x00" + m.Principal.Subject + "\x00" + m.Actor
	}
	slices.SortFunc(relevant, func(a, b config.TrackerAccessMapping) int { return strings.Compare(key(a), key(b)) })
	raw, err := json.Marshal(struct {
		Domain   string
		Policy   string
		Mappings []config.TrackerAccessMapping
	}{"kata:tracker-owner-mapping-policy:v1", policy.Digest, relevant}, json.Deterministic(true))
	if err != nil {
		return tracker.RegistryPolicy{}, tracker.ErrInvalid
	}
	sum := sha256.Sum256(raw)
	policy.Digest = hex.EncodeToString(sum[:])
	return policy, nil
}
