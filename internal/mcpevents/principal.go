package mcpevents

// Principal authorities (design spec §5.1).
const (
	AuthorityDBToken     = "db_token"
	AuthorityStaticToken = "static_token"
	AuthorityLocal       = "local"
	AuthorityHost        = "host"
)

// Principal is the authenticated daemon principal that owns a subscription.
// Authority and Subject form the identity; Actor is a display snapshot.
type Principal struct{ Authority, Subject, Actor string }

// Key is the stored principal key: authority and subject, never the actor.
func (p Principal) Key() string { return p.Authority + ":" + p.Subject }

// Valid reports whether the principal names a known authority and a subject.
func (p Principal) Valid() bool {
	switch p.Authority {
	case AuthorityDBToken, AuthorityStaticToken, AuthorityLocal, AuthorityHost:
		return p.Subject != ""
	}
	return false
}
