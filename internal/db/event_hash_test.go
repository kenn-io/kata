package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContentHashCanonicalizesPayloadJSON(t *testing.T) {
	base := EventHashInput{
		UID:               "01HZNQ7VFPK1XGD8R5MABCD4EX",
		OriginInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EY",
		ProjectUID:        "01HZNQ7VFPK1XGD8R5MABCD4EZ",
		ProjectName:       "kata",
		Type:              "issue.updated",
		Actor:             "agent",
		HLCPhysicalMS:     1,
		HLCCounter:        0,
		CreatedAt:         "2026-05-23T12:00:00.000Z",
	}
	a := base
	a.Payload = json.RawMessage(`{"title":"new","body":"text"}`)
	b := base
	b.Payload = json.RawMessage(`{ "body" : "text", "title" : "new" }`)

	hashA, err := EventContentHash(a)
	require.NoError(t, err)
	hashB, err := EventContentHash(b)
	require.NoError(t, err)

	assert.Equal(t, hashA, hashB)
	assert.Regexp(t, `^[a-f0-9]{64}$`, hashA)
}

func TestContentHashChangesWhenPortableContentChanges(t *testing.T) {
	base := EventHashInput{
		UID:               "01HZNQ7VFPK1XGD8R5MABCD4EX",
		OriginInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EY",
		ProjectUID:        "01HZNQ7VFPK1XGD8R5MABCD4EZ",
		ProjectName:       "kata",
		Type:              "issue.updated",
		Actor:             "agent",
		HLCPhysicalMS:     1,
		HLCCounter:        0,
		CreatedAt:         "2026-05-23T12:00:00.000Z",
		Payload:           json.RawMessage(`{"title":"new"}`),
	}
	changed := base
	changed.Actor = "other"

	hashA, err := EventContentHash(base)
	require.NoError(t, err)
	hashB, err := EventContentHash(changed)
	require.NoError(t, err)

	assert.NotEqual(t, hashA, hashB)
}

func TestContentHashIgnoresProjectName(t *testing.T) {
	base := EventHashInput{
		UID:               "01HZNQ7VFPK1XGD8R5MABCD4EX",
		OriginInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EY",
		ProjectUID:        "01HZNQ7VFPK1XGD8R5MABCD4EZ",
		ProjectName:       "old-name",
		Type:              "issue.updated",
		Actor:             "agent",
		HLCPhysicalMS:     1,
		HLCCounter:        0,
		CreatedAt:         "2026-05-23T12:00:00.000Z",
		Payload:           json.RawMessage(`{"title":"new"}`),
	}
	renamed := base
	renamed.ProjectName = "new-name"

	hashA, err := EventContentHash(base)
	require.NoError(t, err)
	hashB, err := EventContentHash(renamed)
	require.NoError(t, err)

	assert.Equal(t, hashA, hashB)
}

func TestContentHashPreservesLargeJSONNumbers(t *testing.T) {
	base := EventHashInput{
		UID:               "01HZNQ7VFPK1XGD8R5MABCD4EX",
		OriginInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EY",
		ProjectUID:        "01HZNQ7VFPK1XGD8R5MABCD4EZ",
		Type:              "issue.updated",
		Actor:             "agent",
		HLCPhysicalMS:     1,
		HLCCounter:        0,
		CreatedAt:         "2026-05-23T12:00:00.000Z",
	}
	a := base
	a.Payload = json.RawMessage(`{"n":9007199254740992}`)
	b := base
	b.Payload = json.RawMessage(`{"n":9007199254740993}`)

	hashA, err := EventContentHash(a)
	require.NoError(t, err)
	hashB, err := EventContentHash(b)
	require.NoError(t, err)

	assert.NotEqual(t, hashA, hashB)
}
