package commentref

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommentReferenceForms(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Ref
	}{
		{"c:abcdef", Ref{Suffix: "abcdef"}},
		{"AB12:ABCDEF", Ref{IssueRef: "ab12", Suffix: "abcdef"}},
		{"example-project#AB12:ABCDEF", Ref{Project: "example-project", IssueRef: "ab12", Suffix: "abcdef"}},
		{"01aaaaaaaaaaaaaaaaaaaaaaaa", Ref{UID: "01AAAAAAAAAAAAAAAAAAAAAAAA"}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := Parse(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	for _, input := range []string{"", "c:abc", "c:iiiiii", "ab12:abcdef:abcdef", "example-project#c:abcdef", "#ab12:abcdef", "ab:abcdef", "abcdef"} {
		_, err := Parse(input)
		require.Error(t, err, input)
	}
}

func TestHandlesExtendProjectCollisions(t *testing.T) {
	uids := []string{"01AAAAAAAAAAAAAAAAAAABCDEF", "01BBBBBBBBBBBBBBBBBBABCDEF", "01CCCCCCCCCCCCCCCCCCCCCCCC"}
	got := Handles(uids)
	require.Equal(t, "aabcdef", got[uids[0]])
	require.Equal(t, "babcdef", got[uids[1]])
	require.Equal(t, "cccccc", got[uids[2]])
}

// Handles must always resolve back to their canonical UID, regardless of case.
func FuzzCommentReferenceRoundTrip(f *testing.F) {
	f.Add("01AAAAAAAAAAAAAAAAAAABCDEF", uint8(6))
	f.Add("01BBBBBBBBBBBBBBBBBBBBBBBB", uint8(26))
	f.Fuzz(func(t *testing.T, input string, length uint8) {
		ref, err := Parse(input)
		if err != nil || ref.UID == "" {
			t.Skip()
		}
		n := 6 + int(length)%21
		suffix := strings.ToLower(ref.UID[len(ref.UID)-n:])
		parsed, err := Parse("example-project#AB12:" + strings.ToUpper(suffix))
		require.NoError(t, err)
		require.Equal(t, "example-project", parsed.Project)
		require.Equal(t, "ab12", parsed.IssueRef)
		require.Equal(t, suffix, parsed.Suffix)
	})
}
