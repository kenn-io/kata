// Package tracker defines credential-free tracker control authority. A descriptor
// is a discovery record; only a live, explicit grant authorizes its use.
package tracker

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/uid"
)

type SubjectKind string

const (
	SubjectAccount     SubjectKind = "account"
	SubjectOwnerLocal  SubjectKind = "owner-local"
	SubjectStaticOwner SubjectKind = "static-owner"
	SubjectMCPService  SubjectKind = "mcp-service"
	SubjectService     SubjectKind = "service"
)

// Principal comes from authentication, never from a user supplied actor label.
// AuthorityInstanceUID qualifies Subject; equal names at other daemons differ.
type Principal struct {
	AuthorityInstanceUID string      `json:"authority_instance_uid" toml:"authority_instance_uid"`
	Kind                 SubjectKind `json:"kind" toml:"kind"`
	Subject              string      `json:"subject" toml:"subject"`
}

func (p Principal) Validate() error {
	if !uid.Valid(p.AuthorityInstanceUID) || !ValidIdentityText(p.Subject, 256) {
		return ErrInvalid
	}
	switch p.Kind {
	case SubjectAccount, SubjectOwnerLocal, SubjectStaticOwner, SubjectMCPService, SubjectService:
		return nil
	}
	return ErrInvalid
}

type Action string

const (
	ActionDiscover Action = "discover"
	ActionBind     Action = "bind"
	ActionRead     Action = "read"
	ActionStatus   Action = "status"
	ActionDisable  Action = "disable"
	ActionRecover  Action = "recover"
)

func validAction(a Action) bool {
	switch a {
	case ActionDiscover, ActionBind, ActionRead, ActionStatus, ActionDisable, ActionRecover:
		return true
	}
	return false
}

var (
	ErrInvalid           = errors.New("invalid tracker connection policy")
	ErrDenied            = errors.New("tracker connection use is not authorized")
	ErrSelectionRequired = errors.New("choose an explicit tracker owner, connection and generation")
	ErrUnavailable       = errors.New("selected tracker connection is unavailable")
	ErrAmbiguous         = errors.New("tracker connection identity is ambiguous")
)

// Grant requires both a qualified principal and an explicit resource scope.
// Service grants are independent of the human who created a binding.
type Grant struct {
	RootInstanceUID      string    `json:"root_instance_uid" toml:"root_instance_uid"`
	ProjectUID           string    `json:"project_uid" toml:"project_uid"`
	Principal            Principal `json:"principal" toml:"principal"`
	Actions              []Action  `json:"actions" toml:"actions"`
	Sources              []string  `json:"sources,omitempty" toml:"sources"`
	AllAccessibleSources bool      `json:"all_accessible_sources,omitempty" toml:"all_accessible_sources"`
}

func (g Grant) Validate() error {
	if !uid.Valid(g.RootInstanceUID) || !uid.Valid(g.ProjectUID) || g.Principal.Validate() != nil || len(g.Actions) == 0 || len(g.Actions) > 6 || len(g.Sources) > 256 || g.AllAccessibleSources == (len(g.Sources) > 0) {
		return ErrInvalid
	}
	seenActions := map[Action]bool{}
	for _, a := range g.Actions {
		if !validAction(a) || seenActions[a] {
			return ErrInvalid
		}
		seenActions[a] = true
	}
	seenSources := map[string]bool{}
	for _, source := range g.Sources {
		if !ValidIdentityText(source, 2048) || seenSources[source] {
			return ErrInvalid
		}
		seenSources[source] = true
	}
	return nil
}
func (g Grant) allows(root, project string, p Principal, a Action, source string) bool {
	return g.RootInstanceUID == root && g.ProjectUID == project && g.Principal == p && slices.Contains(g.Actions, a) && (a == ActionDiscover || (ValidIdentityText(source, 2048) && (g.AllAccessibleSources || slices.Contains(g.Sources, source))))
}

// Descriptor contains only nonsecret routing identity and owner policy. Do not
// add credential selectors here: it is also used on authenticated relay hops.
type Descriptor struct {
	Authority ProviderAuthority `json:"authority" toml:"-"`
	Selection Selection         `json:"selection" toml:"selection"`
	Provider  string            `json:"provider" toml:"provider"`
	Label     string            `json:"label" toml:"label"`
	Grants    []Grant           `json:"grants" toml:"grants"`
}

func SupportedProvider(provider string) bool {
	switch provider {
	case "github", "notion", "linear", "plane", "ticktick", "todoist", "twenty":
		return true
	}
	return false
}
func (d Descriptor) Validate() error {
	if d.Authority.Validate() != nil || d.Selection.Validate() != nil || !SupportedProvider(d.Provider) || !ValidIdentityText(d.Label, 128) || len(d.Grants) == 0 || len(d.Grants) > 256 {
		return ErrInvalid
	}
	for _, g := range d.Grants {
		if g.Validate() != nil {
			return ErrInvalid
		}
	}
	return nil
}
func (d Descriptor) Allows(root, project string, p Principal, a Action, source string) bool {
	if d.Validate() != nil || p.Validate() != nil || !validAction(a) {
		return false
	}
	for _, g := range d.Grants {
		if g.allows(root, project, p, a, source) {
			return true
		}
	}
	return false
}

// PublicDescriptor is an allowlisted discovery view. It omits other members,
// service grants, credential locations, provider responses and transport tickets.
type PublicDescriptor struct {
	Authority ProviderAuthority `json:"authority"`
	Selection Selection         `json:"selection"`
	Provider  string            `json:"provider"`
	Label     string            `json:"label"`
	Actions   []Action          `json:"actions"`
}

func (d Descriptor) Public(root, project string, p Principal) (PublicDescriptor, bool) {
	if !d.Allows(root, project, p, ActionDiscover, "") {
		return PublicDescriptor{}, false
	}
	out := PublicDescriptor{Selection: d.Selection, Provider: d.Provider, Label: d.Label, Authority: d.Authority}
	for _, g := range d.Grants {
		if g.RootInstanceUID == root && g.ProjectUID == project && g.Principal == p {
			for _, a := range g.Actions {
				if !slices.Contains(out.Actions, a) {
					out.Actions = append(out.Actions, a)
				}
			}
		}
	}
	slices.Sort(out.Actions)
	return out, true
}

// Select never guesses by label, list order, local credentials or unique match.
// Discovery permission alone is insufficient for the requested action/source.
func Select(descriptors []Descriptor, selection *Selection, root, project string, p Principal, a Action, source string) (Descriptor, error) {
	if selection == nil {
		return Descriptor{}, ErrSelectionRequired
	}
	if selection.Validate() != nil {
		return Descriptor{}, ErrInvalid
	}
	var found *Descriptor
	for i := range descriptors {
		d := &descriptors[i]
		if d.Selection == *selection {
			if found != nil {
				return Descriptor{}, ErrAmbiguous
			}
			found = d
		}
	}
	if found == nil {
		return Descriptor{}, ErrUnavailable
	}
	if !found.Allows(root, project, p, a, source) {
		return Descriptor{}, ErrDenied
	}
	return *found, nil
}

// ValidIdentityText validates bounded portable identity text without controls.
func ValidIdentityText(s string, limit int) bool {
	return len(s) > 0 && len(s) <= limit && utf8.ValidString(s) && strings.TrimSpace(s) == s && strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) < 0
}
