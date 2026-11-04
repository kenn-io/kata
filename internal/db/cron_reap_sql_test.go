package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

// Reap's audit survives even when existing evidence fills the summary budget.
func TestReapedSummaryBound(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		// Bound materialization, not the text generator's input alphabet.
		if len(text) > 8000 {
			text = text[:8000]
			text = strings.ToValidUTF8(text, "")
		}
		text = strings.Repeat(text, 9)
		old := cron.Summary{Version: 1, Message: text, InputTokens: 9223372036854775807, OutputTokens: 9223372036854775807}
		now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
		got, err := reapedSummary(old, "operator", now, now.Add(-24*time.Hour), now, "Operator note")
		if err != nil {
			ht.Fatal(err)
		}
		raw, err := json.Marshal(got)
		if err != nil || len(raw) > cron.SummaryLimit || !utf8.ValidString(got.Message) {
			ht.Fatalf("invalid bounded summary: %d %v", len(raw), err)
		}
		if !strings.HasPrefix(got.Message, "Marked unknown by operator at ") || !strings.Contains(got.Message, "Operator note") || got.InputTokens != old.InputTokens || got.OutputTokens != old.OutputTokens {
			ht.Fatal("reap lost audit or token counts")
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestReapedSummaryEscapedLimit(t *testing.T) {
	old := cron.Summary{Version: 1, Message: strings.Repeat("\x01", 10900)}
	oldRaw, err := json.Marshal(old)
	require.NoError(t, err)
	require.LessOrEqual(t, len(oldRaw), cron.SummaryLimit)
	_, err = cron.ParseSummary(oldRaw)
	require.NoError(t, err)
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	got, err := reapedSummary(old, "operator", at, at, at, strings.Repeat("\x01", 1024))
	require.NoError(t, err)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), cron.SummaryLimit)
	require.Contains(t, got.Message, "Previous:")
	require.Less(t, len(got.Message), len(old.Message)+1024+300)
}

// PostgreSQL JSONB cannot represent U+0000. New operator input must fail as a
// portable validation error before either backend attempts its write.
func TestReapRejectsNonPortableOperatorText(t *testing.T) {
	for _, tc := range []struct{ actor, message string }{{"operator", "note\x00"}, {"operator\x00", "note"}} {
		writer := CronSQL{Transact: func(context.Context, func(*sql.Tx) error) error {
			return errors.New("invalid operator text reached storage")
		}}
		_, err := writer.ReapRun(t.Context(), ReapCronRun{ProjectID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ExpectedRevision: 1, Actor: tc.actor, Message: tc.message, Force: true, Now: time.Now()})
		require.ErrorIs(t, err, cron.ErrInvalid)
	}
}

// Truncation preserves the exact audit text's longest UTF-8 prefix, with JSON
// escaping measured independently by appending the next whole rune.
func TestReapedSummaryMaximalPrefix(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	hegel.Test(t, func(ht *hegel.T) {
		pattern := hegel.Draw(ht, hegel.SampledFrom([]string{"a", "\x01", "\x01😀", "é", "\"\\", "\t\n"}))
		count := hegel.Draw(ht, hegel.Integers(0, 12000))
		old := cron.Summary{Version: 1, InputTokens: 9223372036854775807, OutputTokens: 42}
		lo, hi := 0, count
		for lo < hi {
			mid := lo + (hi-lo+1)/2
			old.Message = strings.Repeat(pattern, mid)
			raw, err := json.Marshal(old)
			if err != nil {
				ht.Fatal(err)
			}
			if len(raw) <= cron.SummaryLimit {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		old.Message = strings.Repeat(pattern, lo)
		oldRaw, err := json.Marshal(old)
		if err != nil {
			ht.Fatal(err)
		}
		if _, err := cron.ParseSummary(oldRaw); err != nil {
			ht.Fatalf("generated previous summary is not production-valid: %v", err)
		}
		message := hegel.Draw(ht, hegel.Text())
		if len(message) > 1024 {
			message = strings.ToValidUTF8(message[:1024], "")
		}
		got, err := reapedSummary(old, "operator", at, at, at, message)
		if err != nil {
			ht.Fatal(err)
		}
		want := "Marked unknown by operator at 2026-11-03T12:00:00Z: no run evidence since 2026-11-03T12:00:00Z (stale at 2026-11-03T12:00:00Z)."
		if message != "" {
			want += "\n" + message
		}
		if old.Message != "" {
			want += "\nPrevious: " + old.Message
		}
		if !utf8.ValidString(got.Message) || !strings.HasPrefix(want, got.Message) {
			ht.Fatal("summary is not a valid audit prefix")
		}
		if len(got.Message) < len(want) {
			next, width := utf8.DecodeRuneInString(want[len(got.Message):])
			if width == 0 {
				ht.Fatal("missing next rune")
			}
			candidate := got
			candidate.Message += string(next)
			encoded, err := json.Marshal(candidate)
			if err != nil {
				ht.Fatal(err)
			}
			if len(encoded) <= cron.SummaryLimit {
				ht.Fatal("summary discarded a rune that fits")
			}
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestReapOperatorInputByteContract(t *testing.T) {
	accepted := errors.New("admitted input")
	actorGen := hegel.OneOf[string](hegel.Text(), hegel.SampledFrom([]string{"operator", " ", "\xff", strings.Repeat("é", 128), strings.Repeat("é", 129)}))
	messageGen := hegel.OneOf[string](hegel.Text(), hegel.SampledFrom([]string{"note", "\x00", "\xff", strings.Repeat("é", 512), strings.Repeat("é", 513)}))
	hegel.Test(t, func(ht *hegel.T) {
		actor, message := hegel.Draw(ht, actorGen), hegel.Draw(ht, messageGen)
		if len(actor) > 2048 {
			actor = actor[:2048]
		}
		if len(message) > 4096 {
			message = message[:4096]
		}
		writer := CronSQL{Transact: func(context.Context, func(*sql.Tx) error) error { return accepted }}
		_, err := writer.ReapRun(t.Context(), ReapCronRun{ProjectID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ExpectedRevision: 1, Actor: actor, Message: message, Now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)})
		valid := len(actor) <= 256 && strings.TrimSpace(actor) != "" && utf8.ValidString(actor) && !strings.ContainsRune(actor, 0) && len(message) <= 1024 && utf8.ValidString(message) && !strings.ContainsRune(message, 0)
		if valid && !errors.Is(err, accepted) {
			ht.Fatalf("valid text rejected: %v", err)
		}
		if !valid && !errors.Is(err, cron.ErrInvalid) {
			ht.Fatalf("invalid portable text admitted: %v", err)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
