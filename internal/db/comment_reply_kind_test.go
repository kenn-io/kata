package db

import "testing"

func TestValidCommentReplyKind(t *testing.T) {
	cases := map[string]bool{
		"reply":     true,
		"confirm":   true,
		"refute":    true,
		"supersede": true,
		"answer":    false,
		"":          false,
		"unknown":   false,
		"Answer":    false,
	}
	for kind, want := range cases {
		t.Run(kind, func(t *testing.T) {
			if got := ValidCommentReplyKind(kind); got != want {
				t.Errorf("ValidCommentReplyKind(%q) = %t, want %t", kind, got, want)
			}
		})
	}
}

func FuzzCommentReplyKindMatchesValidation(f *testing.F) {
	for _, kind := range []string{"reply", "confirm", "refute", "supersede", "answer", "", "unknown"} {
		f.Add(kind)
	}
	f.Fuzz(func(t *testing.T, kind string) {
		valid := ValidCommentReplyKind(kind)
		err := ValidateCommentReply("01AAAAAAAAAAAAAAAAAAAAAAAA", "01BBBBBBBBBBBBBBBBBBBBBBBB", kind)
		if valid != (err == nil) {
			t.Fatalf("kind %q: ValidCommentReplyKind=%t, ValidateCommentReply error=%v", kind, valid, err)
		}
	})
}
