package docs

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// The embedded guide must print without metadata and without rewriting Markdown.
func TestPageBody(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"empty", "", ""},
		{"plain", "# Guide\n", "# Guide\n"},
		{"header", "---\ntitle: Guide\n---\n\n# Guide\n", "\n# Guide\n"},
		{"CRLF", "---\r\ntitle: Guide\r\n---\r\n\r\n# Guide\r\n", "\n# Guide\n"},
		{"unclosed", "---\ntitle: Guide\n# Guide\n", "---\ntitle: Guide\n# Guide\n"},
		{"incomplete closer", "---\ntitle: Guide\n--", "---\ntitle: Guide\n--"},
		{"indented closer", "---\ntitle: Guide\n ---\n", "---\ntitle: Guide\n ---\n"},
		{"spaced closer", "---\ntitle: Guide\n--- \n", "---\ntitle: Guide\n--- \n"},
		{"closer at EOF", "---\ntitle: Guide\n---", ""},
		{"empty header", "---\n---\n# Guide\n", "# Guide\n"},
		{"not leading", "\n---\ntitle: Guide\n---\n", "\n---\ntitle: Guide\n---\n"},
		{"one header", "---\ntitle: Guide\n---\n---\nbody\n---\n", "---\nbody\n---\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pageBody(tc.raw))
		})
	}
}

func TestPagePreservesNonleadingAndNearOpeners(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		prefix := rapid.SampledFrom([]string{"\n---\n", " ---\n", "---x\n", "--- \n", "---\t\n", "\ufeff---\n"}).Draw(rt, "prefix")
		raw := prefix + rapid.String().Draw(rt, "suffix")
		require.Equal(rt, strings.ReplaceAll(raw, "\r\n", "\n"), pageBody(raw))
	})
}

func TestPagePreservesBody(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		body := rapid.String().Draw(rt, "body")
		require.Equal(rt, strings.ReplaceAll(body, "\r\n", "\n"), pageBody("---\ntitle: Guide\n---\n"+body))
	})
}

func TestPageLineEndingsAgree(t *testing.T) {
	// Generate logical lines without carriage returns, then encode them both ways.
	rapid.Check(t, func(rt *rapid.T) {
		body := strings.ReplaceAll(rapid.String().Draw(rt, "body"), "\r", "")
		lf := "---\ntitle: Guide\n---\n" + body
		require.Equal(rt, pageBody(lf), pageBody(strings.ReplaceAll(lf, "\n", "\r\n")))
	})
}

func TestPagePreservesMarkdown(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		payload := rapid.String().Draw(rt, "payload")
		body := "[Cron](https://katatracker.com/docs/reference/cron/#triggers)\n\n`" + payload + "`\n\n```text\n" + payload + "\n```\n"
		require.Equal(rt, strings.ReplaceAll(body, "\r\n", "\n"), pageBody("---\ntitle: Guide\n---\n"+body))
	})
}

func TestPageEmbeddedGuide(t *testing.T) {
	body, err := Page("guide/cron-authoring.md")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimSpace(body), "# Author cron jobs with an agent\n"))
	got, err := Page("missing.md")
	require.Error(t, err)
	require.Empty(t, got)
}

func TestPageSelectsEmbeddedDocument(t *testing.T) {
	for _, tc := range []struct{ name, heading string }{
		{"get-started/install.md", "# Install\n"},
		{"guide/concepts.md", "# Concepts\n"},
		{"operations/doctor.md", "# Diagnosing setup\n"},
	} {
		body, err := Page(tc.name)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(strings.TrimSpace(body), tc.heading), tc.name)
		if tc.name == "get-started/install.md" {
			raw, err := FS.ReadFile(tc.name)
			require.NoError(t, err)
			require.Equal(t, string(raw), body, "plain embedded pages pass through exactly")
		}
	}
}
