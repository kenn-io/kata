package docs

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
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
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"\n---\n", " ---\n", "---x\n", "--- \n", "---\t\n", "\ufeff---\n"}))
		raw := prefix + hegel.Draw(ht, hegel.Text())
		require.Equal(ht, strings.ReplaceAll(raw, "\r\n", "\n"), pageBody(raw))
	}, hegel.WithTestCases(1000))
}

func TestPagePreservesBody(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		body := hegel.Draw(ht, hegel.Text())
		require.Equal(ht, strings.ReplaceAll(body, "\r\n", "\n"), pageBody("---\ntitle: Guide\n---\n"+body))
	}, hegel.WithTestCases(1000))
}

func TestPageLineEndingsAgree(t *testing.T) {
	// A line excludes line terminators; arbitrary Unicode bodies are covered above.
	lines := hegel.Lists(hegel.Text().ExcludeCharacters("\r\n"))
	hegel.Test(t, func(ht *hegel.T) {
		body := strings.Join(hegel.Draw(ht, lines), "\n")
		lf := "---\ntitle: Guide\n---\n" + body
		require.Equal(ht, pageBody(lf), pageBody(strings.ReplaceAll(lf, "\n", "\r\n")))
	}, hegel.WithTestCases(1000))
}

func TestPagePreservesMarkdown(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		payload := hegel.Draw(ht, hegel.Text())
		body := "[Cron](https://katatracker.com/docs/reference/cron/#triggers)\n\n`" + payload + "`\n\n```text\n" + payload + "\n```\n"
		require.Equal(ht, strings.ReplaceAll(body, "\r\n", "\n"), pageBody("---\ntitle: Guide\n---\n"+body))
	}, hegel.WithTestCases(1000))
}

func TestPageEmbeddedGuide(t *testing.T) {
	body, err := Page("guide/cron-authoring.md")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimSpace(body), "# Author cron jobs with an agent\n"))
	for _, name := range []string{"", "missing.md", "../guide/cron-authoring.md", "/guide/cron-authoring.md", "./guide/cron-authoring.md", "guide//cron-authoring.md", "guide/cron-authoring.md/", "guide"} {
		got, err := Page(name)
		require.Error(t, err, name)
		require.Empty(t, got, name)
		var pathErr *fs.PathError
		require.ErrorAs(t, err, &pathErr)
		require.Equal(t, name, pathErr.Path)
	}
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
