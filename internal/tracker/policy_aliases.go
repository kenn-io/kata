// Package tracker defines tracker operations and credential-free policy.
package tracker

import "go.kenn.io/kata/internal/trackerpolicy"

// Connection policy types are shared with client configuration without linking
// worker transport and signing code into client-only programs.
type (
	// SubjectKind identifies a principal's authentication class.
	SubjectKind = trackerpolicy.SubjectKind
	// Principal is an authenticated tracker principal.
	Principal = trackerpolicy.Principal
	// Action identifies a tracker permission.
	Action = trackerpolicy.Action
	// Grant authorizes a scoped principal and actions.
	Grant = trackerpolicy.Grant
	// Descriptor is a credential-free connection policy.
	Descriptor = trackerpolicy.Descriptor
	// PublicDescriptor is the allowlisted discovery view.
	PublicDescriptor = trackerpolicy.PublicDescriptor
	// ProviderAuthority describes permitted provider origins.
	ProviderAuthority = trackerpolicy.ProviderAuthority
	// Selection names a specific owner, connection, and generation.
	Selection = trackerpolicy.Selection
	// RegistryState records retained authority state.
	RegistryState = trackerpolicy.RegistryState
	// RegistryPolicy identifies normalized owner-local policy.
	RegistryPolicy = trackerpolicy.RegistryPolicy
	// RetiredGeneration records an earlier used generation.
	RetiredGeneration = trackerpolicy.RetiredGeneration
	// RegistryRecord retains generation history.
	RegistryRecord = trackerpolicy.RegistryRecord
)

// Subject, action, and registry constants share the policy package's values.
const (
	SubjectAccount     = trackerpolicy.SubjectAccount
	SubjectOwnerLocal  = trackerpolicy.SubjectOwnerLocal
	SubjectStaticOwner = trackerpolicy.SubjectStaticOwner
	SubjectMCPService  = trackerpolicy.SubjectMCPService
	SubjectService     = trackerpolicy.SubjectService
	ActionDiscover     = trackerpolicy.ActionDiscover
	ActionBind         = trackerpolicy.ActionBind
	ActionRead         = trackerpolicy.ActionRead
	ActionStatus       = trackerpolicy.ActionStatus
	ActionDisable      = trackerpolicy.ActionDisable
	ActionRecover      = trackerpolicy.ActionRecover
	RegistryActive     = trackerpolicy.RegistryActive
	RegistryRevoked    = trackerpolicy.RegistryRevoked
	RegistryRestored   = trackerpolicy.RegistryRestored
)

// Policy errors share identity with the client-safe policy package.
var (
	ErrInvalid           = trackerpolicy.ErrInvalid
	ErrDenied            = trackerpolicy.ErrDenied
	ErrSelectionRequired = trackerpolicy.ErrSelectionRequired
	ErrUnavailable       = trackerpolicy.ErrUnavailable
	ErrAmbiguous         = trackerpolicy.ErrAmbiguous
	ErrGenerationChanged = trackerpolicy.ErrGenerationChanged
	ErrGenerationRetired = trackerpolicy.ErrGenerationRetired
	ErrHistoryFull       = trackerpolicy.ErrHistoryFull
)

// SupportedProvider reports whether Kata recognizes a provider.
func SupportedProvider(provider string) bool { return trackerpolicy.SupportedProvider(provider) }

// ValidIdentityText checks bounded identity text.
func ValidIdentityText(s string, limit int) bool { return trackerpolicy.ValidIdentityText(s, limit) }

// DigestValid checks the lowercase hexadecimal digest format.
func DigestValid(s string) bool { return trackerpolicy.DigestValid(s) }

// Select resolves an explicitly selected, authorized connection.
func Select(descriptors []Descriptor, selection *Selection, root, project string, p Principal, action Action, source string) (Descriptor, error) {
	return trackerpolicy.Select(descriptors, selection, root, project, p, action, source)
}

// NextRegistryRecord advances retained generation history.
func NextRegistryRecord(previous *RegistryRecord, policy *RegistryPolicy, epoch string) (RegistryRecord, error) {
	return trackerpolicy.NextRegistryRecord(previous, policy, epoch)
}

func validAction(a Action) bool {
	return trackerpolicy.ValidAction(a)
}
