package mcpevents_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/mcpevents"
)

var codecKey = bytes.Repeat([]byte{3}, 32)

func codecMAC(key []byte, sub, inc, cg, id string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("c1." + sub + "." + inc + "." + cg + "." + id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

func TestCursorCodecExactEncoding(t *testing.T) {
	c := mcpevents.Cursor{Incarnation: "inc1", ContinuityGeneration: 2, EventID: 345}
	got := mcpevents.EncodeCursor(codecKey, "sub_a", c)
	assert.Equal(t, "c1.inc1.2.345."+codecMAC(codecKey, "sub_a", "inc1", "2", "345"), got)
	back, err := mcpevents.DecodeCursor(codecKey, "sub_a", got)
	require.NoError(t, err)
	assert.Equal(t, c, back)
}

func TestCursorCodecRoundTripExtremes(t *testing.T) {
	for _, c := range []mcpevents.Cursor{
		{Incarnation: "a", ContinuityGeneration: 0, EventID: 0},
		{Incarnation: "A-b_9", ContinuityGeneration: math.MaxInt64, EventID: math.MaxInt64},
	} {
		back, err := mcpevents.DecodeCursor(codecKey, "sub_a", mcpevents.EncodeCursor(codecKey, "sub_a", c))
		require.NoError(t, err)
		assert.Equal(t, c, back)
	}
}

func TestCursorCodecVerifiesMACForAnyIncarnation(t *testing.T) {
	other := mcpevents.EncodeCursor(codecKey, "sub_a", mcpevents.Cursor{Incarnation: "otherinc", ContinuityGeneration: 1, EventID: 9})
	got, err := mcpevents.DecodeCursor(codecKey, "sub_a", other)
	require.NoError(t, err)
	assert.Equal(t, "otherinc", got.Incarnation)

	// The incarnation is MAC-covered: swapping it invalidates the cursor.
	forged := "c1.evilinc.1.9." + other[len(other)-22:]
	_, err = mcpevents.DecodeCursor(codecKey, "sub_a", forged)
	assert.ErrorIs(t, err, mcpevents.ErrInvalidCursor)
}

func TestCursorCodecBindsSubscriptionAndKey(t *testing.T) {
	raw := mcpevents.EncodeCursor(codecKey, "sub_a", mcpevents.Cursor{Incarnation: "i", ContinuityGeneration: 1, EventID: 1})
	_, err := mcpevents.DecodeCursor(codecKey, "sub_b", raw)
	assert.ErrorIs(t, err, mcpevents.ErrInvalidCursor)
	_, err = mcpevents.DecodeCursor(bytes.Repeat([]byte{4}, 32), "sub_a", raw)
	assert.ErrorIs(t, err, mcpevents.ErrInvalidCursor)
}

func TestCursorCodecRejectsFlippedByte(t *testing.T) {
	raw := mcpevents.EncodeCursor(codecKey, "sub_a", mcpevents.Cursor{Incarnation: "inc", ContinuityGeneration: 1, EventID: 100})
	for i := range raw {
		b := []byte(raw)
		b[i] ^= 1
		_, err := mcpevents.DecodeCursor(codecKey, "sub_a", string(b))
		assert.ErrorIs(t, err, mcpevents.ErrInvalidCursor, "byte %d", i)
	}
}

func TestCursorCodecRejectsNonCanonicalDecimals(t *testing.T) {
	for _, tc := range []struct{ inc, cg, id string }{
		{"i", "01", "5"}, {"i", "1", "05"}, {"i", "00", "0"}, {"i", "+1", "5"},
		{"i", "1", "+5"}, {"i", "-1", "5"}, {"i", "1", "-0"}, {"i", "", "5"},
		{"i", "1", ""}, {"i", "1", "9223372036854775808"}, {"i", "0x1", "5"}, {"i", "1", " 5"},
	} {
		// A correct MAC proves rejection comes from the decimal check.
		raw := "c1." + tc.inc + "." + tc.cg + "." + tc.id + "." + codecMAC(codecKey, "sub_a", tc.inc, tc.cg, tc.id)
		_, err := mcpevents.DecodeCursor(codecKey, "sub_a", raw)
		assert.ErrorIs(t, err, mcpevents.ErrInvalidCursor, "%q", raw)
	}
}

func TestCursorCodecRejectsMalformedShapes(t *testing.T) {
	mac := codecMAC(codecKey, "sub_a", "i", "1", "5")
	for _, raw := range []string{
		"", "c1", "c1.i.1.5", "c2.i.1.5." + mac, "c1..1.5." + mac, "c1.i.1.5." + mac + ".x",
		"c1.i.1.5." + mac + "=", "c1.i.1.5.", "c1.i.1.5." + mac[:10], "c1.i!.1.5." + mac,
		"c1.i.1.5." + mac + "AA",
	} {
		_, err := mcpevents.DecodeCursor(codecKey, "sub_a", raw)
		assert.ErrorIs(t, err, mcpevents.ErrInvalidCursor, "%q", raw)
	}
	_, err := mcpevents.DecodeCursor(codecKey, "sub_a", "c1.i.1."+strconv.Itoa(5)+"."+mac)
	assert.NoError(t, err)
}
