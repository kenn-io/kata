package mcpevents_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/mcpevents"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	sealed, err := mcpevents.Seal(key, "sub_a", "secret", []byte("hunter2"))
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), "hunter2")
	got, err := mcpevents.Open(key, "sub_a", "secret", sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("hunter2"), got)
}

func TestSealUsesFreshNonce(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	a, err := mcpevents.Seal(key, "sub_a", "secret", []byte("x"))
	require.NoError(t, err)
	b, err := mcpevents.Seal(key, "sub_a", "secret", []byte("x"))
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

func TestOpenRejectsTamperingAndWrongBinding(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	sealed, err := mcpevents.Seal(key, "sub_a", "secret", []byte("x"))
	require.NoError(t, err)

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	_, err = mcpevents.Open(key, "sub_a", "secret", tampered)
	assert.Error(t, err)
	_, err = mcpevents.Open(key, "sub_b", "secret", sealed)
	assert.Error(t, err)
	_, err = mcpevents.Open(key, "sub_a", "token", sealed)
	assert.Error(t, err)
	_, err = mcpevents.Open(bytes.Repeat([]byte{8}, 32), "sub_a", "secret", sealed)
	assert.Error(t, err)
	_, err = mcpevents.Open(key, "sub_a", "secret", sealed[:5])
	assert.Error(t, err)
}

func TestSealAssociatedDataIsUnambiguous(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	sealed, err := mcpevents.Seal(key, "sub_a", "bsecret", []byte("x"))
	require.NoError(t, err)
	_, err = mcpevents.Open(key, "sub_ab", "secret", sealed)
	assert.Error(t, err)
}
