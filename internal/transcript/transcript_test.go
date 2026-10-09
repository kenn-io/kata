package transcript

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptLinks(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			ref := Transcript{Agent: agent, SessionID: "00000000-0000-4000-8000-000000000001"}
			require.NoError(t, ref.Validate())
			got, err := ref.Link("https://agentsview.example/archive/")
			require.NoError(t, err)
			want := "https://agentsview.example/archive/sessions/"
			if agent == "codex" {
				want += "codex/"
			}
			assert.Equal(t, want+ref.SessionID, got)
		})
	}
}

func FuzzTranscriptLinkPreservesBasePrefix(f *testing.F) {
	f.Add("team/archive", true, true)
	f.Add("team?archive#section", false, false)
	f.Fuzz(func(t *testing.T, prefix string, codex, https bool) {
		scheme := "http"
		if https {
			scheme = "https"
		}
		agent, route := "claude", "sessions/"
		if codex {
			agent, route = "codex", "sessions/codex/"
		}
		ref := Transcript{Agent: agent, SessionID: "00000000-0000-4000-8000-000000000001"}
		// Exercise both literal path separators and separators encoded within a
		// single segment. Canonical Path serialization alone misses the latter.
		for _, base := range []string{
			(&url.URL{Scheme: scheme, Host: "agentsview.example", Path: "/" + prefix}).String(),
			scheme + "://agentsview.example/" + url.PathEscape(prefix),
		} {
			got, err := ref.Link(base)
			require.NoError(t, err)
			assert.Equal(t, strings.TrimRight(base, "/")+"/"+route+ref.SessionID, got)
		}
	})
}

func TestTranscriptRejectsSensitiveCoordinates(t *testing.T) {
	for _, id := range []string{"", "../session", "/home/user/sessions/chat.jsonl", "secret?token=value", "session\nother", strings.Repeat("a", 10000)} {
		assert.Error(t, (&Transcript{Agent: "codex", SessionID: id}).Validate())
	}
	for _, base := range []string{"javascript:alert(1)", "https://user:secret@agentsview.example", "https://agentsview.example?token=secret", "https://agentsview.example/#secret", "//agentsview.example", "file:///sessions"} {
		_, err := (&Transcript{Agent: "codex", SessionID: "00000000-0000-4000-8000-000000000001"}).Link(base)
		assert.Error(t, err)
	}
}

func TestTranscriptLinkPreservesEncodedBasePrefix(t *testing.T) {
	ref := Transcript{Agent: "codex", SessionID: "00000000-0000-4000-8000-000000000001"}
	for _, base := range []string{"https://agentsview.example/team%2Farchive", "https://agentsview.example/%2e%2e/archive"} {
		got, err := ref.Link(base)
		require.NoError(t, err)
		assert.Equal(t, base+"/sessions/codex/"+ref.SessionID, got)
	}
}

// UUIDs are the stable native IDs for both supported harnesses. Exercise
// the whole byte domain, independently formatting the UUID and expected URL.
func FuzzTranscriptLinkPreservesSession(f *testing.F) {
	f.Add([]byte("0123456789abcdef"), true)
	f.Add(make([]byte, 16), false)
	f.Fuzz(func(t *testing.T, raw []byte, codex bool) {
		var bytes [16]byte
		copy(bytes[:], raw)
		hexID := hex.EncodeToString(bytes[:])
		id := hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:]
		agent, path := "claude", ""
		if codex {
			agent, path = "codex", "codex/"
		}
		ref := Transcript{Agent: agent, SessionID: id}
		got, err := ref.Link("https://agentsview.example")
		require.NoError(t, err)
		assert.Equal(t, "https://agentsview.example/sessions/"+path+id, got)
	})
}
