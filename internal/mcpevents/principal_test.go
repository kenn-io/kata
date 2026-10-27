package mcpevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrincipalKeyIsAuthorityAndSubject(t *testing.T) {
	for _, tc := range []struct {
		p    Principal
		key  string
		good bool
	}{
		{Principal{Authority: AuthorityDBToken, Subject: "42", Actor: "example-user"}, "db_token:42", true},
		{Principal{Authority: AuthorityStaticToken, Subject: "static"}, "static_token:static", true},
		{Principal{Authority: AuthorityLocal, Subject: "local"}, "local:local", true},
		{Principal{Authority: AuthorityHost, Subject: "tenant-1", Actor: "example-user"}, "host:tenant-1", true},
		{Principal{Authority: "bootstrap", Subject: "x"}, "bootstrap:x", false},
		{Principal{Authority: AuthorityHost}, "host:", false},
		{Principal{}, ":", false},
	} {
		assert.Equal(t, tc.key, tc.p.Key())
		assert.Equal(t, tc.good, tc.p.Valid(), tc.key)
	}
	a := Principal{Authority: AuthorityHost, Subject: "tenant-1", Actor: "shared"}
	b := Principal{Authority: AuthorityHost, Subject: "tenant-2", Actor: "shared"}
	assert.NotEqual(t, a.Key(), b.Key(), "the actor is a display snapshot, not identity")
	a.Actor = "renamed"
	assert.Equal(t, "host:tenant-1", a.Key())
}
