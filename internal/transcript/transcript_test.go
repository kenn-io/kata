package transcript

import (
	"encoding/hex"
	"net/url"
	"slices"
	"strings"
	"testing"

	"hegel.dev/go/hegel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptAgentsMatchNativeSessionContract(t *testing.T) {
	require.Equal(t, []string{"claude", "codex", "pi", "omp"}, Agents())
	for _, agent := range []string{"", "amp", "openclaw", "cursor", "gemini", "qwen", "muse", "PI", "OMP", "Claude", "CODEX", "pi "} {
		ref := Transcript{Agent: agent, SessionID: "00000000-0000-4000-8000-000000000001"}
		require.Error(t, ref.Validate(), "agent=%q", agent)
		_, err := ref.Link("https://agentsview.example")
		require.Error(t, err, "agent=%q", agent)
	}
}

func TestTranscriptAgentsReturnsDefensiveCopy(t *testing.T) {
	agents := Agents()
	agents[0] = "muse"

	require.Equal(t, []string{"claude", "codex", "pi", "omp"}, Agents())
	require.NoError(t, (Transcript{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}).Validate())
	require.Error(t, (Transcript{Agent: "muse", SessionID: "00000000-0000-4000-8000-000000000001"}).Validate())
}

func TestLinkPerAgent(t *testing.T) {
	for _, tc := range []struct{ agent, route string }{
		{"claude", "sessions/"}, {"codex", "sessions/codex/"},
		{"pi", "sessions/pi/"}, {"omp", "sessions/omp/"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			ref := Transcript{Agent: tc.agent, SessionID: "00000000-0000-4000-8000-000000000001"}
			for _, base := range []string{"https://agentsview.example", "https://agentsview.example/archive///", "https://agentsview.example/team%2Farchive", "https://agentsview.example/team%2farchive", "https://agentsview.example/%2e%2e/archive", "https://agentsview.example/%25archive", "https://agentsview.example/%E2%98%83", "https://agentsview.example/archive%2F"} {
				got, err := ref.Link(base)
				require.NoError(t, err)
				assert.Equal(t, strings.TrimRight(base, "/")+"/"+tc.route+ref.SessionID, got)
			}
		})
	}
}

// Spec property 15: every supported agent accepts native UUIDs, while all other
// agent strings reject them. Generate both arbitrary text and supported names
// so the accept and reject directions are exercised together.
func TestPropertyValidateAcceptsExactlyAgents(t *testing.T) {
	agents := Agents()
	hegel.Test(t, func(ht *hegel.T) {
		agent := hegel.Draw(ht, hegel.OneOf[string](hegel.Text(), hegel.SampledFrom(agents)))
		raw := hegel.Draw(ht, hegel.Binary(16, 16))
		id := hex.EncodeToString(raw)
		id = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
		err := (Transcript{Agent: agent, SessionID: id}).Validate()
		if (err == nil) != slices.Contains(agents, agent) {
			ht.Fatalf("Validate(agent=%q, session=%q) = %v; accepted agents=%q", agent, id, err, agents)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Retain the minimal counterexample from property 15: Pi with a zero UUID.
func TestTranscriptAcceptsPiZeroUUID(t *testing.T) {
	require.NoError(t, (Transcript{Agent: "pi", SessionID: "00000000-0000-0000-0000-000000000000"}).Validate())
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
	for _, base := range []string{"javascript:alert(1)", "https://user:secret@agentsview.example", "https://agentsview.example?token=secret", "https://agentsview.example?", "https://agentsview.example/%", "https://agentsview.example/#secret", "//agentsview.example", "file:///sessions"} {
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
