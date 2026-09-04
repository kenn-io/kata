package markdownrender

import (
	"fmt"
	"html"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

const (
	ansiBoldOn       = "\x1b[1m"
	ansiBoldOff      = "\x1b[22m"
	ansiItalicOn     = "\x1b[3m"
	ansiItalicOff    = "\x1b[23m"
	ansiUnderlineOn  = "\x1b[4m"
	ansiUnderlineOff = "\x1b[24m"
	ansiStrikeOn     = "\x1b[9m"
	ansiStrikeOff    = "\x1b[29m"
)

type inlineStyle uint8

const (
	inlineBold inlineStyle = 1 << iota
	inlineItalic
	inlineUnderline
	inlineStrike
)

type terminalRenderer struct {
	source []byte
	opts   Options
}

func renderMarkdownDocument(markdown string, opts Options) (string, error) {
	source := []byte(markdown)
	parser := goldmark.New(goldmark.WithExtensions(
		extension.GFM,
		extension.DefinitionList,
	)).Parser()
	document := parser.Parse(text.NewReader(source))
	renderer := terminalRenderer{source: source, opts: opts}
	out := strings.TrimRight(renderer.renderBlocks(document), "\n")
	if out == "" {
		return "", nil
	}
	return out + "\n", nil
}

func (r terminalRenderer) renderBlocks(parent ast.Node) string {
	var out strings.Builder
	var previous ast.Node
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		var block string
		switch node := node.(type) {
		case *ast.Heading:
			block = ansiBoldOn + r.renderInlinesStyled(node, inlineBold) + ansiBoldOff
		case *ast.Paragraph, *ast.TextBlock:
			block = r.wrap(r.renderInlines(node), r.opts.Width)
		case *ast.CodeBlock:
			block = r.renderCodeBlock(node.Lines())
		case *ast.FencedCodeBlock:
			block = r.renderCodeBlock(node.Lines())
		case *ast.HTMLBlock:
			block = r.renderHTMLBlock(node)
		case *ast.Blockquote:
			block = prefixLines(r.renderBlocks(node), "| ")
		case *ast.List:
			block = r.renderList(node, 0)
		case *extast.Table:
			block = r.renderTable(node)
		case *extast.DefinitionList:
			block = r.renderDefinitionList(node)
		case *ast.ThematicBreak:
			continue
		default:
			if node.HasChildren() {
				block = r.renderBlocks(node)
			}
		}
		block = strings.TrimRight(block, "\n")
		if block != "" {
			if out.Len() > 0 {
				separator := "\n\n"
				if _, ok := previous.(*ast.Heading); ok {
					separator = "\n"
				}
				out.WriteString(separator)
			}
			out.WriteString(block)
			previous = node
		}
	}
	return out.String()
}

func (r terminalRenderer) renderList(list *ast.List, depth int) string {
	lines := make([]string, 0, list.ChildCount())
	itemNumber := list.Start
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		item, ok := child.(*ast.ListItem)
		if !ok {
			continue
		}
		marker := "- "
		if list.IsOrdered() {
			marker = fmt.Sprintf("%d. ", itemNumber)
			itemNumber++
		}
		if isTaskListItem(item) && !list.IsOrdered() {
			marker = ""
		}
		indent := strings.Repeat("  ", depth)
		continuation := indent + strings.Repeat(" ", ansi.StringWidth(marker))
		itemLines := r.renderListItem(item, depth, max(1, r.opts.Width-ansi.StringWidth(indent+marker)))
		if len(itemLines) == 0 {
			lines = append(lines, indent+marker)
			continue
		}
		lines = append(lines, indent+marker+itemLines[0])
		for _, line := range itemLines[1:] {
			if line == "" {
				lines = append(lines, "")
			} else if strings.HasPrefix(line, strings.Repeat("  ", depth+1)) {
				lines = append(lines, line)
			} else {
				lines = append(lines, continuation+line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func isTaskListItem(item *ast.ListItem) bool {
	firstBlock := item.FirstChild()
	return firstBlock != nil && firstBlock.FirstChild() != nil &&
		firstBlock.FirstChild().Kind() == extast.KindTaskCheckBox
}

func (r terminalRenderer) renderListItem(item *ast.ListItem, depth, width int) []string {
	var lines []string
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		switch child := child.(type) {
		case *ast.Paragraph, *ast.TextBlock:
			paragraph := r.wrap(r.renderInlines(child), width)
			lines = append(lines, strings.Split(paragraph, "\n")...)
		case *ast.List:
			nested := r.renderList(child, depth+1)
			lines = append(lines, strings.Split(nested, "\n")...)
		case *ast.CodeBlock:
			lines = append(lines, strings.Split(r.renderCodeBlock(child.Lines()), "\n")...)
		case *ast.FencedCodeBlock:
			lines = append(lines, strings.Split(r.renderCodeBlock(child.Lines()), "\n")...)
		default:
			if child.HasChildren() {
				lines = append(lines, strings.Split(r.renderBlocks(child), "\n")...)
			}
		}
	}
	return lines
}

func (r terminalRenderer) renderCodeBlock(segments *text.Segments) string {
	var code strings.Builder
	for i := range segments.Len() {
		segment := segments.At(i)
		code.Write(segment.Value(r.source))
	}
	raw := strings.TrimSuffix(code.String(), "\n")
	if r.opts.CodeBlockBackground == nil || *r.opts.CodeBlockBackground == "" {
		return raw
	}
	prefix := "\x1b[48;5;" + *r.opts.CodeBlockBackground + "m"
	lines := strings.Split(raw, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i] + "\x1b[49m"
	}
	return strings.Join(lines, "\n")
}

func (r terminalRenderer) renderHTMLBlock(block *ast.HTMLBlock) string {
	var raw strings.Builder
	for i := range block.Lines().Len() {
		segment := block.Lines().At(i)
		raw.Write(segment.Value(r.source))
	}
	if block.HasClosure() {
		raw.Write(block.ClosureLine.Value(r.source))
	}
	return stripHTMLTags(raw.String())
}

func stripHTMLTags(value string) string {
	var out strings.Builder
	inTag := false
	var quote rune
	for _, char := range value {
		if !inTag {
			if char == '<' {
				inTag = true
				continue
			}
			out.WriteRune(char)
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '>':
			inTag = false
		}
	}
	return strings.TrimSpace(html.UnescapeString(out.String()))
}

func (r terminalRenderer) renderTable(table *extast.Table) string {
	rows := make([]string, 0, table.ChildCount()+1)
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		cells := make([]string, 0, child.ChildCount())
		for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, r.renderInlines(cell))
		}
		rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
		if _, ok := child.(*extast.TableHeader); ok {
			separators := make([]string, len(cells))
			for i := range separators {
				separators[i] = "---"
			}
			rows = append(rows, "| "+strings.Join(separators, " | ")+" |")
		}
	}
	return strings.Join(rows, "\n")
}

func (r terminalRenderer) renderDefinitionList(list *extast.DefinitionList) string {
	parts := make([]string, 0, list.ChildCount())
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		var part string
		switch child := child.(type) {
		case *extast.DefinitionTerm:
			part = r.renderInlines(child)
		case *extast.DefinitionDescription:
			part = r.renderBlocks(child)
		}
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

func (r terminalRenderer) renderInlines(parent ast.Node) string {
	return r.renderInlinesStyled(parent, 0)
}

func (r terminalRenderer) renderInlinesStyled(parent ast.Node, active inlineStyle) string {
	var out strings.Builder
	for node := parent.FirstChild(); node != nil; node = node.NextSibling() {
		switch node := node.(type) {
		case *ast.Text:
			out.WriteString(html.UnescapeString(string(node.Segment.Value(r.source))))
			if node.HardLineBreak() || node.SoftLineBreak() {
				out.WriteByte('\n')
			}
		case *ast.String:
			out.WriteString(html.UnescapeString(string(node.Value)))
		case *ast.CodeSpan:
			out.WriteByte('`')
			out.WriteString(strings.Join(strings.Fields(r.renderInlinesStyled(node, active)), " "))
			out.WriteByte('`')
		case *ast.Emphasis:
			if node.Level == 2 {
				out.WriteString(r.renderInlineStyle(
					node, active, inlineBold, ansiBoldOn, ansiBoldOff,
				))
			} else {
				out.WriteString(r.renderInlineStyle(
					node, active, inlineItalic, ansiItalicOn, ansiItalicOff,
				))
			}
		case *ast.Link:
			out.WriteString(r.renderInlinesStyled(node, active))
			if destination := visibleLinkDestination(node.Destination); destination != "" {
				out.WriteByte(' ')
				out.WriteString(ansiUnderlineOn + destination + ansiUnderlineOff)
			}
		case *ast.Image:
			out.WriteString("[image: " + r.renderInlinesStyled(node, active) + "]")
			if destination := html.UnescapeString(string(node.Destination)); destination != "" {
				out.WriteByte(' ')
				out.WriteString(destination)
			}
		case *ast.AutoLink:
			label := html.UnescapeString(string(node.Label(r.source)))
			if node.AutoLinkType == ast.AutoLinkEmail {
				out.WriteString(label)
			} else {
				out.WriteString(ansiUnderlineOn + label + ansiUnderlineOff)
			}
		case *ast.RawHTML:
			out.WriteString(r.renderRawHTML(node))
		case *extast.Strikethrough:
			out.WriteString(r.renderInlineStyle(
				node, active, inlineStrike, ansiStrikeOn, ansiStrikeOff,
			))
		case *extast.TaskCheckBox:
			if node.IsChecked {
				out.WriteString("[x] ")
			} else {
				out.WriteString("[ ] ")
			}
		default:
			if node.HasChildren() {
				out.WriteString(r.renderInlinesStyled(node, active))
			}
		}
	}
	return out.String()
}

func (r terminalRenderer) renderRawHTML(node *ast.RawHTML) string {
	var raw strings.Builder
	for i := range node.Segments.Len() {
		segment := node.Segments.At(i)
		raw.Write(segment.Value(r.source))
	}
	value := raw.String()
	trimmed := strings.TrimSpace(value)
	if len(trimmed) >= 3 && trimmed[0] == '<' && trimmed[len(trimmed)-1] == '>' {
		fields := strings.Fields(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
		if len(fields) > 0 && strings.EqualFold(strings.TrimSuffix(fields[0], "/"), "br") {
			return "\n"
		}
	}
	return stripHTMLTags(value)
}

func visibleLinkDestination(destination []byte) string {
	value := html.UnescapeString(string(destination))
	if strings.HasPrefix(value, "#") {
		return ""
	}
	return value
}

func (r terminalRenderer) renderInlineStyle(
	node ast.Node,
	active, style inlineStyle,
	on, off string,
) string {
	if active&style != 0 {
		return r.renderInlinesStyled(node, active)
	}
	return on + r.renderInlinesStyled(node, active|style) + off
}

func (r terminalRenderer) wrap(value string, width int) string {
	return ansi.Wordwrap(value, max(1, width), "")
}

func prefixLines(value, prefix string) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	for i := range lines {
		if lines[i] == "" {
			lines[i] = strings.TrimRight(prefix, " ")
		} else {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}
