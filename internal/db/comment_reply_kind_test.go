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
