package db

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestValidateExternalRootProjectionParamsRejectsInvalidNativeTitles(t *testing.T) {
	for _, title := range []string{" \t\n", "external\x00title"} {
		err := ValidateExternalRootProjectionParams(ExternalRootProjectionParams{
			BindingID: 1, ClaimToken: "claim-token", Title: title,
			ExternalRevision: "revision-one", IntegrationActor: "connector:notes",
			ExternalUpdatedAt: time.Now(), ExternalObservedAt: time.Now(),
		})
		assert.ErrorIs(t, err, ErrExternalRootValidation)
	}
}

func TestValidateExternalFieldProjectionParamsRejectsNoncanonicalPatch(t *testing.T) {
	tests := []struct {
		name      string
		kataField string
		patch     map[string]jsontext.Value
	}{
		{
			name: "padded Kata field", kataField: " scheduled_on",
			patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-08-21"`)},
		},
		{
			name: "disallowed key", kataField: "scheduled_on",
			patch: map[string]jsontext.Value{
				"scheduled_on": jsontext.Value(`"2026-08-21"`),
				"title":        jsontext.Value(`"not allowed"`),
			},
		},
		{
			name: "missing mapped key", kataField: "scheduled_on",
			patch: map[string]jsontext.Value{"timezone": jsontext.Value(`"Etc/UTC"`)},
		},
		{name: "empty patch", kataField: "scheduled_on", patch: map[string]jsontext.Value{}},
		{
			name: "invalid JSON", kataField: "scheduled_on",
			patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`{"unfinished"`)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateExternalFieldProjectionParams(ExternalFieldProjectionParams{
				BindingID: 1, MappingID: 2, ClaimToken: "claim-token",
				KataField: test.kataField, Patch: test.patch, ExpectedIssueRevision: 1,
				IntegrationActor: "connector:notes",
			})
			assert.ErrorIs(t, err, ErrExternalRootValidation)
		})
	}
}

func TestSafeExternalRootErrorRedactsCompleteAuthorizationValue(t *testing.T) {
	for _, credential := range []string{
		"Authorization: Basic dXNlcjpwYXNz",
		"Authorization=Bearer opaque-access-token",
	} {
		redacted := SafeExternalRootError("request failed: " + credential + "; retry later")
		assert.NotContains(t, redacted, credential)
		assert.NotContains(t, redacted, "dXNlcjpwYXNz")
		assert.NotContains(t, redacted, "opaque-access-token")
		assert.Contains(t, redacted, "Authorization")
		assert.Contains(t, redacted, "[redacted]")
	}
}

func TestExternalRevisionMappingIDsAreStableAndOpaque(t *testing.T) {
	comment := ExternalCommentRevisionMappingExternalID("provider-comment", "revision-one")
	assert.Equal(t, comment, ExternalCommentRevisionMappingExternalID("provider-comment", "revision-one"))
	assert.NotEqual(t, comment, ExternalCommentRevisionMappingExternalID("provider-comment", "revision-two"))
	assert.NotContains(t, comment, "provider-comment")
	assert.NotContains(t, comment, "revision-one")

	root := ExternalRootRevisionMappingExternalID("provider-root", "revision-one")
	assert.Equal(t, root, ExternalRootRevisionMappingExternalID("provider-root", "revision-one"))
	assert.NotEqual(t, root, ExternalRootRevisionMappingExternalID("provider-root", "revision-two"))
	assert.NotContains(t, root, "provider-root")
	assert.NotContains(t, root, "revision-one")
}
