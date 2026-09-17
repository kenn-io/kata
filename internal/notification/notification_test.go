package notification

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecipientAndMetadataKeyKeepExistingInboxAddressContract(t *testing.T) {
	recipient, err := NormalizeRecipient(" coordinator/worker-a ")
	require.NoError(t, err)
	assert.Equal(t, "coordinator/worker-a", recipient)
	assert.Equal(t, "notify.Y29vcmRpbmF0b3Ivd29ya2VyLWE", MetadataKey(recipient))

	for _, raw := range []string{"", " \t", "worker\nname", "worker\u200bname", strings.Repeat("x", 129)} {
		_, err := NormalizeRecipient(raw)
		require.Error(t, err, raw)
	}
}

func TestSystemValueUsesOrdinaryNotificationJSON(t *testing.T) {
	raw, err := MarshalSystem(Scheduled, "2026-09-16T09:00")
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"from":"system","message":"Scheduled date reached: 2026-09-16T09:00"}`,
		string(raw),
	)

	raw, err = MarshalSystem(Deadline, "2026-09-16")
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"from":"system","message":"Deadline reached: 2026-09-16"}`,
		string(raw),
	)
}

func TestParseAutomaticRequiresTheCompleteCanonicalShape(t *testing.T) {
	key := MetadataKey("coordinator/worker-a")
	valid := []struct {
		raw  string
		kind Kind
		date string
	}{
		{`{"from":"system","message":"Scheduled date reached: 2026-09-16T09:00"}`, Scheduled, "2026-09-16T09:00"},
		{`{"message":"Deadline reached: 2026-09-16","from":"system"}`, Deadline, "2026-09-16"},
	}
	for _, test := range valid {
		got, ok := ParseAutomatic(key, json.RawMessage(test.raw))
		require.True(t, ok)
		assert.Equal(t, "coordinator/worker-a", got.Recipient)
		assert.Equal(t, test.kind, got.Kind)
		assert.Equal(t, test.date, got.SourceValue)
	}

	lookalikes := []string{
		`{"from":"operator","message":"Scheduled date reached: 2026-09-16"}`,
		`{"from":"system","teammate":"worker-a","message":"Scheduled date reached: 2026-09-16"}`,
		`{"from":"system","message":"Scheduled date reached: 2026-09-16","extra":true}`,
		`{"from":"system","message":"Scheduled date reached: tomorrow"}`,
		`{"from":"system","message":"Scheduled date reached: 2026-09-16 "}`,
		`{"from":"system","message":"scheduled date reached: 2026-09-16"}`,
		`{"from":"system","message":"Deadline reached: 2026-09-16T09:00+01:00"}`,
	}
	for _, raw := range lookalikes {
		_, ok := ParseAutomatic(key, json.RawMessage(raw))
		assert.False(t, ok, raw)
	}
	_, ok := ParseAutomatic("notify.not-base64!", json.RawMessage(valid[0].raw))
	assert.False(t, ok)
}
