package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/tokenactor"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

// TrackerConnectionConfig is owner-local only. Public descriptors are built
// explicitly; credential selectors must not be serialized onto a relay or API.
// Exactly one provider block is required, including an explicit token selector
// or GitHub App. Named accounts never inherit daemon defaults or gh CLI auth.
type TrackerConnectionConfig struct {
	UID        string              `toml:"uid"`
	Generation string              `toml:"generation"`
	Provider   string              `toml:"provider"`
	Label      string              `toml:"label"`
	Grants     []tracker.Grant     `toml:"grant"`
	GitHub     *GitHubSyncConfig   `toml:"github"`
	Notion     *NotionSyncConfig   `toml:"notion"`
	Linear     *LinearSyncConfig   `toml:"linear"`
	Plane      *PlaneSyncConfig    `toml:"plane"`
	TickTick   *TickTickSyncConfig `toml:"ticktick"`
	Todoist    *TodoistSyncConfig  `toml:"todoist"`
	Twenty     *TwentySyncConfig   `toml:"twenty"`
}

func (c TrackerConnectionConfig) Descriptor(owner string) tracker.Descriptor {
	d := tracker.Descriptor{Selection: tracker.Selection{OwnerInstanceUID: owner, ConnectionUID: c.UID, Generation: c.Generation}, Provider: c.Provider, Label: c.Label, Grants: cloneTrackerGrants(c.Grants)}
	switch c.Provider {
	case "plane":
		if c.Plane != nil {
			d.Authority = tracker.ProviderAuthority{APIOrigin: c.Plane.APIOrigin, WebOrigin: c.Plane.WebOrigin}
		}
	case "todoist":
		if c.Todoist != nil {
			d.Authority = tracker.ProviderAuthority{APIOrigin: c.Todoist.APIOrigin}
		}
	case "twenty":
		if c.Twenty != nil {
			d.Authority = tracker.ProviderAuthority{APIOrigin: c.Twenty.APIOrigin, WebOrigin: c.Twenty.WebOrigin}
		}
	}
	return d
}

// TrackerAccessMapping is root-local policy, not a claim supplied by a remote
// owner. Its actor is rechecked against live project/team/host policy on use.
type TrackerAccessMapping struct {
	ProjectUID string            `toml:"project_uid"`
	Principal  tracker.Principal `toml:"principal"`
	Actor      string            `toml:"actor"`
}

func ValidateTrackerAccess(mappings []TrackerAccessMapping) error {
	if len(mappings) > 1024 {
		return invalidTrackerConfig()
	}
	seen := map[struct {
		Project   string
		Principal tracker.Principal
	}]bool{}
	for _, m := range mappings {
		key := struct {
			Project   string
			Principal tracker.Principal
		}{m.ProjectUID, m.Principal}
		if !uid.Valid(m.ProjectUID) || m.Principal.Validate() != nil || tokenactor.Validate(m.Actor) != nil || seen[key] {
			return invalidTrackerConfig()
		}
		seen[key] = true
	}
	return nil
}
func invalidTrackerConfig() error {
	return errors.New("invalid tracker connection configuration; check identity, provider, explicit credential selector and grants")
}

// NormalizeTrackerConnections validates without credential lookup or I/O and
// returns detached snapshots. Errors never include supplied config values.
func NormalizeTrackerConnections(input []TrackerConnectionConfig) ([]TrackerConnectionConfig, error) {
	if len(input) > 128 {
		return nil, invalidTrackerConfig()
	}
	output := make([]TrackerConnectionConfig, 0, len(input))
	seen := map[string]bool{}
	for _, c := range input {
		identity := c.Descriptor("01ARZ3NDEKTSV4RRFFQ69G5FAV")
		// Provider normalization owns canonicalizing origin spelling below.
		identity.Authority = tracker.ProviderAuthority{}
		if identity.Validate() != nil || seen[c.UID] {
			return nil, invalidTrackerConfig()
		}
		seen[c.UID] = true
		count := 0
		for _, present := range []bool{c.GitHub != nil, c.Notion != nil, c.Linear != nil, c.Plane != nil, c.TickTick != nil, c.Todoist != nil, c.Twenty != nil} {
			if present {
				count++
			}
		}
		if count != 1 {
			return nil, invalidTrackerConfig()
		}
		var err error
		switch c.Provider {
		case "github":
			if c.GitHub == nil || (strings.TrimSpace(c.GitHub.TokenEnv) == "" && len(c.GitHub.Apps) == 0) {
				return nil, invalidTrackerConfig()
			}
			v := *c.GitHub
			v.Apps = slices.Clone(v.Apps)
			v, err = NormalizeGitHubSyncConfig(v)
			c.GitHub = &v
		case "notion":
			if c.Notion == nil || strings.TrimSpace(c.Notion.TokenEnv) == "" {
				return nil, invalidTrackerConfig()
			}
			v, e := NormalizeNotionSyncConfig(*c.Notion)
			err = e
			c.Notion = &v
		case "linear":
			if c.Linear == nil || strings.TrimSpace(c.Linear.TokenEnv) == "" {
				return nil, invalidTrackerConfig()
			}
			v, e := NormalizeLinearSyncConfig(*c.Linear)
			err = e
			c.Linear = &v
		case "plane":
			if c.Plane == nil || strings.TrimSpace(c.Plane.TokenEnv) == "" {
				return nil, invalidTrackerConfig()
			}
			v, e := NormalizePlaneSyncConfig(*c.Plane)
			err = e
			c.Plane = &v
		case "ticktick":
			if c.TickTick == nil || strings.TrimSpace(c.TickTick.TokenEnv) == "" {
				return nil, invalidTrackerConfig()
			}
			v, e := NormalizeTickTickSyncConfig(*c.TickTick)
			err = e
			c.TickTick = &v
		case "todoist":
			if c.Todoist == nil || strings.TrimSpace(c.Todoist.TokenEnv) == "" {
				return nil, invalidTrackerConfig()
			}
			v, e := NormalizeTodoistSyncConfig(*c.Todoist)
			err = e
			c.Todoist = &v
		case "twenty":
			if c.Twenty == nil || strings.TrimSpace(c.Twenty.TokenEnv) == "" {
				return nil, invalidTrackerConfig()
			}
			v, e := NormalizeTwentySyncConfig(*c.Twenty)
			err = e
			c.Twenty = &v
		default:
			return nil, invalidTrackerConfig()
		}
		if err != nil {
			return nil, invalidTrackerConfig()
		}
		if c.Descriptor("01ARZ3NDEKTSV4RRFFQ69G5FAV").Validate() != nil {
			return nil, invalidTrackerConfig()
		}
		c.Grants = cloneTrackerGrants(c.Grants)
		output = append(output, c)
	}
	return output, nil
}
func cloneTrackerGrants(grants []tracker.Grant) []tracker.Grant {
	out := slices.Clone(grants)
	for i := range out {
		out[i].Actions = slices.Clone(out[i].Actions)
		out[i].Sources = slices.Clone(out[i].Sources)
	}
	return out
}

// RegistryPolicy hashes local selectors and effective policy, not credential
// values. Labels are deliberately excluded so a presentation rename is inert.
func (c TrackerConnectionConfig) RegistryPolicy(owner string) (tracker.RegistryPolicy, error) {
	c.Label = ""
	raw, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		return tracker.RegistryPolicy{}, invalidTrackerConfig()
	}
	sum := sha256.Sum256(raw)
	return tracker.RegistryPolicy{Selection: tracker.Selection{OwnerInstanceUID: owner, ConnectionUID: c.UID, Generation: c.Generation}, Provider: c.Provider, Digest: hex.EncodeToString(sum[:])}, nil
}
