package mcpserver

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	katadocs "go.kenn.io/kata/docs"
)

const (
	docsBaseURL       = "https://katatracker.com/docs/"
	docExcerptRunes   = 240
	maximumQueryBytes = 512
)

// SearchDocsInput selects documentation sections by keyword.
type SearchDocsInput struct {
	Query string `json:"query" jsonschema:"Words to find in Kata's documentation; every word must match"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum sections to return; default 20"`
}

// DocMatch is one ranked documentation section.
type DocMatch struct {
	ID       string `json:"id" jsonschema:"Pass to kata.read_doc to read the full section"`
	Document string `json:"document"`
	Heading  string `json:"heading"`
	URL      string `json:"url"`
	Excerpt  string `json:"excerpt"`
}

// SearchDocsOutput lists the best-matching documentation sections.
type SearchDocsOutput struct {
	Matches   []DocMatch `json:"matches"`
	Truncated bool       `json:"truncated"`
}

// ReadDocInput selects one documentation section.
type ReadDocInput struct {
	ID string `json:"id" jsonschema:"Section id returned by kata.search_docs"`
}

// ReadDocOutput is one full documentation section in Markdown.
type ReadDocOutput struct {
	ID       string `json:"id"`
	Document string `json:"document"`
	Heading  string `json:"heading"`
	URL      string `json:"url"`
	Content  string `json:"content"`
}

type docSection struct {
	id, document, heading, url, content string
	lowerDocument, lowerHeading, lower  string
}

var docSections = sync.OnceValues(func() ([]docSection, error) {
	return loadDocSections(katadocs.FS)
})

func registerDocsTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	addTool(server, "kata.search_docs", "Search docs", "Search Kata's user documentation by keyword and return matching sections with short excerpts.", read, handlers.searchDocs)
	addTool(server, "kata.read_doc", "Read doc section", "Read one documentation section in Markdown by the id kata.search_docs returned.", read, handlers.readDoc)
}

func (toolHandlers) searchDocs(_ context.Context, _ *sdkmcp.CallToolRequest, input SearchDocsInput) (*sdkmcp.CallToolResult, SearchDocsOutput, error) {
	if len(input.Query) > maximumQueryBytes {
		return nil, SearchDocsOutput{}, errors.New("query must be at most 512 bytes")
	}
	terms := docTerms(input.Query)
	if len(terms) == 0 {
		return nil, SearchDocsOutput{}, errors.New("query must contain a letter or digit")
	}
	limit, err := boundedLimit(input.Limit)
	if err != nil {
		return nil, SearchDocsOutput{}, err
	}
	sections, err := docSections()
	if err != nil {
		return nil, SearchDocsOutput{}, err
	}
	type scored struct {
		section *docSection
		score   int
	}
	phrase := strings.Join(terms, " ")
	found := []scored{}
	for i := range sections {
		section := &sections[i]
		score := 0
		for _, term := range terms {
			body := strings.Count(section.lower, term)
			if body == 0 && !strings.Contains(section.lowerDocument, term) {
				score = 0
				break
			}
			score += 1 + min(body, 10) + 10*min(strings.Count(section.lowerHeading, term), 3) + 3*min(strings.Count(section.lowerDocument, term), 3)
		}
		if score == 0 {
			continue
		}
		if strings.Contains(section.lowerHeading, phrase) {
			score += 20
		}
		found = append(found, scored{section: section, score: score})
	}
	slices.SortStableFunc(found, func(a, b scored) int { return b.score - a.score })
	output := SearchDocsOutput{Matches: make([]DocMatch, 0, min(len(found), limit)), Truncated: len(found) > limit}
	for _, match := range found[:min(len(found), limit)] {
		section := match.section
		output.Matches = append(output.Matches, DocMatch{
			ID: section.id, Document: section.document, Heading: section.heading, URL: section.url,
			Excerpt: docExcerpt(section.content, section.lower, terms),
		})
	}
	return successResult(), output, nil
}

func (toolHandlers) readDoc(_ context.Context, _ *sdkmcp.CallToolRequest, input ReadDocInput) (*sdkmcp.CallToolResult, ReadDocOutput, error) {
	sections, err := docSections()
	if err != nil {
		return nil, ReadDocOutput{}, err
	}
	id := strings.TrimSpace(input.ID)
	for _, section := range sections {
		if section.id == id {
			return successResult(), ReadDocOutput{
				ID: section.id, Document: section.document, Heading: section.heading, URL: section.url, Content: section.content,
			}, nil
		}
	}
	return nil, ReadDocOutput{}, errors.New("unknown documentation section; use an id returned by kata.search_docs")
}

func docTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	return compactStrings(fields)
}

// docExcerpt returns one line of text around the first matched term.
func docExcerpt(content, lower string, terms []string) string {
	start := 0
	for _, term := range terms {
		if index := strings.Index(lower, term); index >= 0 {
			start = index
			break
		}
	}
	// Lowercasing can change byte lengths, so map the offset through runes.
	runes := []rune(content)
	position := min(len([]rune(lower[:start])), len(runes))
	from := max(0, position-docExcerptRunes/3)
	to := min(len(runes), from+docExcerptRunes)
	return strings.Join(strings.Fields(string(runes[from:to])), " ")
}

// loadDocSections splits each page at its level 1-3 headings, skipping
// fenced code, so one section stays small enough to return whole.
func loadDocSections(files fs.FS) ([]docSection, error) {
	var paths []string
	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && path.Ext(name) == ".md" {
			paths = append(paths, name)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	var sections []docSection
	for _, name := range paths {
		raw, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		sections = append(sections, splitDocSections(name, strings.ReplaceAll(string(raw), "\r\n", "\n"))...)
	}
	return sections, nil
}

func splitDocSections(name, text string) []docSection {
	title, body := docFrontmatter(text)
	pageURL := docsBaseURL + strings.TrimSuffix(strings.TrimSuffix(name, ".md"), "index")
	if !strings.HasSuffix(pageURL, "/") {
		pageURL += "/"
	}
	var sections []docSection
	used := map[string]int{}
	heading, slug := title, ""
	var lines []string
	fence := ""
	flush := func() {
		content := strings.TrimSpace(strings.Join(lines, "\n"))
		if content == "" {
			return
		}
		id, url := name, pageURL
		if slug != "" {
			id, url = name+"#"+slug, pageURL+"#"+slug
		}
		sections = append(sections, docSection{
			id: id, document: title, heading: heading, url: url, content: content,
			lowerDocument: strings.ToLower(title), lowerHeading: strings.ToLower(heading), lower: strings.ToLower(content),
		})
	}
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) && strings.Trim(strings.TrimLeft(trimmed, fence[:1]), " \t") == "" {
				fence = ""
			}
		} else if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			info := strings.TrimLeft(trimmed, trimmed[:1])
			fence = trimmed[:len(trimmed)-len(info)]
		} else if level, text := docHeading(line); level > 0 {
			flush()
			lines, heading, slug = nil, text, ""
			if level == 1 && title == "" && len(sections) == 0 {
				title = text
			}
			// The page's leading title keeps the bare page id.
			if level > 1 || len(sections) > 0 {
				slug = docSlug(text)
				if count := used[slug]; count > 0 {
					slug += "_" + strconv.Itoa(count)
				}
				used[docSlug(text)]++
			}
		}
		lines = append(lines, line)
	}
	flush()
	return sections
}

func docFrontmatter(text string) (string, string) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", text
	}
	header, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return "", text
	}
	for line := range strings.SplitSeq(header, "\n") {
		if value, found := strings.CutPrefix(line, "title:"); found {
			return strings.Trim(strings.TrimSpace(value), `"'`), body
		}
	}
	return "", body
}

func docHeading(line string) (int, string) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 3 || level >= len(line) || line[level] != ' ' {
		return 0, ""
	}
	return level, strings.TrimSpace(line[level:])
}

// docSlug follows the site generator's anchor rule for ASCII headings.
func docSlug(heading string) string {
	var builder strings.Builder
	dash := false
	for _, r := range strings.ToLower(heading) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'):
			if dash && builder.Len() > 0 {
				builder.WriteByte('-')
			}
			dash = false
			builder.WriteRune(r)
		case r == '-' || unicode.IsSpace(r):
			dash = true
		}
	}
	if builder.Len() == 0 {
		return "section"
	}
	return builder.String()
}
