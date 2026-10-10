package jsonl

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func FuzzCommentReplyRecordNormalization(f *testing.F) {
	f.Add(int64(123456789), "reply")
	f.Add(int64(999999999), "confirm")
	f.Fuzz(func(t *testing.T, nanos int64, kind string) {
		switch kind {
		case "reply", "confirm", "refute", "supersede":
		default:
			t.Skip()
		}
		nanos = nanos % 1_000_000_000
		if nanos < 0 {
			nanos = -nanos
		}
		timestamp := time.Date(2026, 10, 7, 12, 0, 0, int(nanos), time.UTC)
		input := db.CommentExport{UID: "01AAAAAAAAAAAAAAAAAAAAAAAA", ReplyToUID: "01BBBBBBBBBBBBBBBBBBBBBBBB", ReplyKind: kind, EditedAt: timestamp.Format(time.RFC3339Nano), CreatedAt: "2026-10-07T11:00:00Z", Body: "Evidence", Author: "worker"}
		data, err := json.Marshal(input)
		require.NoError(t, err)
		record, err := toImportRecord(Envelope{Kind: KindComment, Data: jsontext.Value(data)}, 32, "", nil)
		require.NoError(t, err)
		got := record.(*db.CommentExport)
		require.Equal(t, input.ReplyToUID, got.ReplyToUID)
		require.Equal(t, input.ReplyKind, got.ReplyKind)
		parsed, err := time.Parse(time.RFC3339Nano, got.EditedAt)
		require.NoError(t, err)
		require.True(t, timestamp.Equal(parsed))
		malformed := input
		malformed.EditedAt = "bad"
		data, err = json.Marshal(malformed)
		require.NoError(t, err)
		_, err = toImportRecord(Envelope{Kind: KindComment, Data: jsontext.Value(data)}, 32, "", nil)
		require.Error(t, err)
	})
}
