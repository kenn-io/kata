package transcript

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptRejectsNonUUIDBuiltInSessions(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			assert.Error(t, (Transcript{Agent: agent, SessionID: "session-token"}).Validate())
		})
	}
}

func TestTranscriptLinks(t *testing.T) {
	for _, tc := range []struct{ agent, id, path string }{
		{"claude", "00000000-0000-4000-8000-000000000001", "sessions/00000000-0000-4000-8000-000000000001"},
		{"codex", "00000000-0000-4000-8000-000000000001", "sessions/codex/00000000-0000-4000-8000-000000000001"},
		{"pi", "00000000-0000-4000-8000-000000000001", "sessions/pi/00000000-0000-4000-8000-000000000001"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			ref := Transcript{Agent: tc.agent, SessionID: tc.id}
			require.NoError(t, ref.Validate())
			got, err := ref.Link("https://agentsview.example/archive/")
			require.NoError(t, err)
			assert.Equal(t, "https://agentsview.example/archive/"+tc.path, got)
		})
	}
}

func FuzzTranscriptLinkPreservesBasePrefix(f *testing.F) {
	f.Add("team/archive", true)
	f.Add("team?archive#section", false)
	f.Fuzz(func(t *testing.T, prefix string, https bool) {
		scheme := "http"
		if https {
			scheme = "https"
		}
		ref := Transcript{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}
		// Exercise both literal path separators and separators encoded within a
		// single segment. Canonical Path serialization alone misses the latter.
		for _, base := range []string{
			(&url.URL{Scheme: scheme, Host: "agentsview.example", Path: "/" + prefix}).String(),
			scheme + "://agentsview.example/" + url.PathEscape(prefix),
		} {
			got, err := ref.Link(base)
			require.NoError(t, err)
			assert.Equal(t, strings.TrimRight(base, "/")+"/sessions/"+ref.SessionID, got)
		}
	})
}

func TestTranscriptValidatesCoordinatesAndLinks(t *testing.T) {
	for _, id := range []string{"T-00000000-0000-4000-8000-000000000001", "ses_0123456789AbCd", "a.b_c-d", "0", strings.Repeat("a", 128)} {
		assert.NoError(t, (Transcript{Agent: "amp", SessionID: id}).Validate())
	}
	for _, agent := range []string{"a", "0-agent", strings.Repeat("a", 32)} {
		assert.NoError(t, (Transcript{Agent: agent, SessionID: "00000000-0000-4000-8000-000000000001"}).Validate())
	}
	for _, agent := range []string{"", "Pi", "-pi", "pi/omp", "pi_omp", strings.Repeat("a", 33)} {
		assert.Error(t, (Transcript{Agent: agent, SessionID: "00000000-0000-4000-8000-000000000001"}).Validate())
	}
	for _, id := range []string{"", "..", "../session", "/home/user/sessions/chat.jsonl", "secret?token=value", "session\nother", "session other", "session\\id", "session#other", strings.Repeat("a", 129), strings.Repeat("a", 10000)} {
		assert.Error(t, (&Transcript{Agent: "amp", SessionID: id}).Validate())
	}
	for _, base := range []string{"javascript:alert(1)", "https://user:secret@agentsview.example", "https://agentsview.example?token=secret", "https://agentsview.example/#secret", "//agentsview.example", "file:///sessions"} {
		_, err := (&Transcript{Agent: "codex", SessionID: "00000000-0000-4000-8000-000000000001"}).Link(base)
		assert.Error(t, err)
	}
}

func TestTranscriptLinkPreservesEncodedBasePrefix(t *testing.T) {
	ref := Transcript{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}
	for _, base := range []string{
		"https://agentsview.example/team%2Farchive",
		"https://agentsview.example/%2e%2e/archive",
		"https://agentsview.example/team%25archive",
	} {
		t.Run(base, func(t *testing.T) {
			got, err := ref.Link(base)
			require.NoError(t, err)
			assert.Equal(t, base+"/sessions/"+ref.SessionID, got)
		})
	}
}

// Exercise UUID sessions across the whole byte domain, independently
// formatting the UUID and expected URL.
func FuzzTranscriptLinkPreservesSession(f *testing.F) {
	f.Add([]byte("0123456789abcdef"))
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var bytes [16]byte
		copy(bytes[:], raw)
		hexID := hex.EncodeToString(bytes[:])
		id := hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:]
		ref := Transcript{Agent: "claude", SessionID: id}
		got, err := ref.Link("https://agentsview.example")
		require.NoError(t, err)
		assert.Equal(t, "https://agentsview.example/sessions/"+id, got)
	})
}
