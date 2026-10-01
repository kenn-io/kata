package notionsync

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func pageFixture() PageContent {
	return PageContent{Page: Page{ID: "22222222-2222-4222-8222-222222222222", URL: "https://www.notion.so/example-page", DataSourceID: sourceID, CreatorID: "44444444-4444-4444-8444-444444444444", CreatedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}, Title: "Example task", Markdown: "# Content\n"}
}

func TestBuildImportBatchClassifiesLiveGroupMembership(t *testing.T) {
	ds := groupSchema()
	c, err := ResolveConfig(ds, Selectors{}, "")
	require.NoError(t, err)
	for _, tc := range []struct {
		id   *string
		want string
	}{{nil, "open"}, {new("ready"), "open"}, {new("active"), "open"}, {new("complete-a"), "closed"}, {new("complete-b"), "closed"}} {
		page := pageFixture()
		page.Page.StatusID = tc.id
		batch, err := BuildImportBatchWithSchema("notion:"+sourceID, c, ds, []PageContent{page})
		require.NoError(t, err)
		require.Equal(t, tc.want, batch.Items[0].Status)
		if tc.want == "closed" {
			require.Equal(t, "done", *batch.Items[0].ClosedReason)
			require.Equal(t, page.Page.UpdatedAt, *batch.Items[0].ClosedAt)
		} else {
			require.Nil(t, batch.Items[0].ClosedReason)
			require.Nil(t, batch.Items[0].ClosedAt)
		}
	}
	page := pageFixture()
	page.Page.StatusID = new("missing")
	_, err = BuildImportBatchWithSchema("notion:"+sourceID, c, ds, []PageContent{page})
	require.Error(t, err)
	// Group configs must never silently classify a page without its live schema.
	_, err = BuildImportBatch("notion:"+sourceID, c, []PageContent{page})
	require.Error(t, err)
}

func TestBuildImportBatchLifecycle(t *testing.T) {
	c := configFixture(t)
	page := pageFixture()
	done := "complete-b"
	owner := "33333333333343338333333333333333"
	page.Page.StatusID = &done
	page.OwnerID = &owner
	page.Title = ""
	batch, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{page})
	require.NoError(t, err)
	require.NoError(t, db.ValidateImportBatch(batch))
	require.Equal(t, "notion-sync", batch.Actor)
	require.Equal(t, "notion:"+sourceID, batch.Source)
	require.Len(t, batch.Items, 1)
	item := batch.Items[0]
	require.Equal(t, "page:22222222-2222-4222-8222-222222222222", item.ExternalID)
	require.Equal(t, "[Notion] (untitled)", item.Title)
	require.Equal(t, "closed", item.Status)
	require.Equal(t, "done", *item.ClosedReason)
	require.Equal(t, page.Page.UpdatedAt, *item.ClosedAt)
	require.Equal(t, "notion:33333333-3333-4333-8333-333333333333", *item.Owner)
	require.Equal(t, "notion:44444444-4444-4444-8444-444444444444", item.Author)
	require.Equal(t, "# Content\n\n---\nImported from Notion: https://www.notion.so/example-page", item.Body)
	require.Nil(t, item.Priority)
	require.Empty(t, item.Comments)
	require.Empty(t, item.Links)
	require.Empty(t, item.Labels)
	for _, status := range []*string{nil, new("active"), new("new-option")} {
		page.Page.StatusID = status
		page.OwnerID = nil
		page.Page.CreatorID = ""
		page.Title = "Résumé"
		batch, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{page})
		require.NoError(t, err)
		item := batch.Items[0]
		require.Equal(t, "open", item.Status)
		require.Nil(t, item.ClosedAt)
		require.Nil(t, item.ClosedReason)
		require.Nil(t, item.Owner)
		require.Equal(t, "notion-unknown", item.Author)
		require.Equal(t, "[Notion] Résumé", item.Title)
	}
}

func TestMappingSourceTimes(t *testing.T) {
	c := configFixture(t)
	p := pageFixture()
	p.Page.CreatedAt = time.Date(2026, 9, 27, 3, 0, 0, 123456789, time.FixedZone("offset", 10800))
	p.Page.UpdatedAt = p.Page.CreatedAt.Add(time.Hour)
	batch, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
	require.NoError(t, err)
	require.Equal(t, "2026-09-27T00:00:00.123Z", batch.Items[0].CreatedAt.Format(time.RFC3339Nano))
	require.Equal(t, "2026-09-27T01:00:00.123Z", batch.Items[0].UpdatedAt.Format(time.RFC3339Nano))
	p.Page.CreatedAt = time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC)
	batch, err = BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
	require.NoError(t, err)
	require.Equal(t, "0001-01-02T00:00:00Z", batch.Items[0].CreatedAt.Format(time.RFC3339))
	for _, change := range []func(*PageContent){func(p *PageContent) { p.Page.CreatedAt = time.Time{} }, func(p *PageContent) { p.Page.CreatedAt = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) }, func(p *PageContent) { p.Page.UpdatedAt = time.Time{} }, func(p *PageContent) { p.Page.UpdatedAt = p.Page.CreatedAt.Add(-time.Nanosecond) }, func(p *PageContent) { p.Page.UpdatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }} {
		p := pageFixture()
		change(&p)
		_, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
		require.Error(t, err)
	}
}

func TestMappingValidation(t *testing.T) {
	c := configFixture(t)
	for _, change := range []func(*PageContent){func(p *PageContent) { p.Page.ID = "bad" }, func(p *PageContent) { p.Page.DataSourceID = databaseID }, func(p *PageContent) { p.Page.CreatorID = "bad" }, func(p *PageContent) { p.OwnerID = new("bad") }, func(p *PageContent) { p.Title = "bad\x00title" }, func(p *PageContent) { p.Title = string([]byte{0xff}) }, func(p *PageContent) { p.Markdown = string([]byte{0xff}) }} {
		p := pageFixture()
		change(&p)
		_, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
		require.Error(t, err)
	}
	for _, url := range []string{"", "http://www.notion.so/page", "https://user:secret-marker@www.notion.so/page", "https://www.notion.so/page\nsecret-marker", "javascript:secret-marker", "https://www.notion.so/%0asecret-marker"} {
		p := pageFixture()
		p.Page.URL = url
		_, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret-marker")
	}
	_, err := BuildImportBatch("wrong-source", c, []PageContent{pageFixture()})
	require.Error(t, err)
	_, err = BuildImportBatch("notion:"+sourceID, c, []PageContent{pageFixture(), pageFixture()})
	require.Error(t, err)
}

func TestMappingMarkdownUTF8ByteLimit(t *testing.T) {
	c := configFixture(t)
	p := pageFixture()
	p.Markdown = strings.Repeat("é", (1<<20)/2)
	batch, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(batch.Items[0].Body, p.Markdown+"\n---\n"))
	p.Markdown += "a"
	_, err = BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
	require.ErrorContains(t, err, "markdown")
}

func TestMappingImportItemByteLimit(t *testing.T) {
	c := configFixture(t)
	p := pageFixture()
	p.Title = strings.Repeat("é", (64<<20)/2)
	_, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{p})
	require.ErrorContains(t, err, "64 MiB")
}

func TestMappingCumulativeImportItemByteLimit(t *testing.T) {
	c := configFixture(t)
	content := pageFixture()
	content.Markdown = strings.Repeat("a", 1<<20)
	pages := make([]PageContent, 64)
	for i := range pages {
		pages[i] = content
		pages[i].Page.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	}
	batch, err := BuildImportBatch("notion:"+sourceID, c, pages[:63])
	require.NoError(t, err)
	require.Len(t, batch.Items, 63)
	_, err = BuildImportBatch("notion:"+sourceID, c, pages)
	require.ErrorContains(t, err, "64 MiB")
}

func TestMappingTitlePrefix(t *testing.T) {
	for _, tc := range []struct {
		prefix      *bool
		title, want string
		labels      []string
	}{
		{nil, "Example task", "[Notion] Example task", nil},
		{new(true), "Example task", "[Notion] Example task", nil},
		{new(false), " Example task ", " Example task ", []string{"notion"}},
		{new(false), "", "(untitled)", []string{"notion"}},
	} {
		c := configFixture(t)
		c.TitlePrefix = tc.prefix
		page := pageFixture()
		page.Title = tc.title
		batch, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{page})
		require.NoError(t, err)
		item := batch.Items[0]
		require.Equal(t, tc.want, item.Title)
		require.Equal(t, tc.labels, item.Labels)
		require.Equal(t, "page:"+page.Page.ID, item.ExternalID)
		require.Equal(t, "# Content\n\n---\nImported from Notion: https://www.notion.so/example-page", item.Body)
	}
}

func TestMappingTitlePrefixWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prefix      *bool
		title, want string
		labels      []string
	}{
		{"disabled_spaces", new(false), "   ", "(untitled)", []string{"notion"}},
		{"disabled_tabs_newlines", new(false), "\t\n", "(untitled)", []string{"notion"}},
		{"enabled_spaces", new(true), "   ", "[Notion]    ", nil},
		{"enabled_tabs_newlines", new(true), "\t\n", "[Notion] \t\n", nil},
		{"default_spaces", nil, "   ", "[Notion]    ", nil},
		{"default_tabs_newlines", nil, "\t\n", "[Notion] \t\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := configFixture(t)
			c.TitlePrefix = tc.prefix
			page := pageFixture()
			page.Title = tc.title
			batch, err := BuildImportBatch("notion:"+sourceID, c, []PageContent{page})
			require.NoError(t, err)
			require.Equal(t, tc.want, batch.Items[0].Title)
			require.Equal(t, tc.labels, batch.Items[0].Labels)
		})
	}
}
