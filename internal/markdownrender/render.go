// Package markdownrender renders user-authored Markdown for terminal display.
package markdownrender

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"

	"charm.land/glamour/v2"
	glamansi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kata/internal/textsafe"
)

// Options configures terminal Markdown rendering.
type Options struct {
	Width               int
	CodeBlockBackground *string
}

var terminalControlEntityPattern = regexp.MustCompile(
	`&(?:#[xX][0-9A-Fa-f]{1,8}|#[0-9]{1,10}|[A-Za-z][A-Za-z0-9]{0,31});`,
)

// SanitizeInput removes terminal controls from Markdown before either a
// built-in or external renderer can decode character references into them.
func SanitizeInput(markdown string) string {
	markdown = terminalControlEntityPattern.ReplaceAllStringFunc(markdown, func(entity string) string {
		decoded := html.UnescapeString(entity)
		if decoded != entity && strings.ContainsFunc(decoded, textsafe.IsUnsafeTerminalRune) {
			return decoded
		}
		return entity
	})
	return textsafe.Block(markdown)
}

// Render converts Markdown into ANSI terminal output. Callers must pass the
// result through ANSIWrappedLines before writing it to a terminal.
func Render(markdown string, opts Options) (out string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = ""
			err = fmt.Errorf("render markdown: %v", recovered)
		}
	}()
	width := max(1, opts.Width)
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(styleConfig(opts.CodeBlockBackground)),
		glamour.WithPreservedNewLines(),
		glamour.WithWordWrap(width),
		glamour.WithTableWrap(true),
	)
	if err != nil {
		return "", err
	}
	return renderer.Render(SanitizeInput(markdown))
}

// RenderLines converts Markdown into display-width-bounded terminal lines.
func RenderLines(markdown string, opts Options) ([]string, error) {
	if strings.TrimSpace(markdown) == "" {
		return nil, nil
	}
	rendered, err := Render(markdown, opts)
	if err != nil {
		return nil, err
	}
	return ANSIWrappedLines(rendered, opts.Width), nil
}

// ANSIWrappedLines sanitizes, normalizes, and hard-wraps rendered ANSI output.
func ANSIWrappedLines(rendered string, width int) []string {
	rendered = sanitizeRenderedOutput(rendered)
	raw := strings.Split(rendered, "\n")
	first := 0
	for first < len(raw) && ansi.Strip(raw[first]) == "" {
		first++
	}
	if first == len(raw) {
		return nil
	}
	last := len(raw) - 1
	for last > first && ansi.Strip(raw[last]) == "" {
		last--
	}
	raw[first] = strings.Join(raw[:first+1], "")
	// Preserve the existing trailing-space policy before trailing ANSI-only
	// rows make the state sequence the line's suffix.
	raw[last] = strings.TrimRight(raw[last], " ") + strings.Join(raw[last+1:], "")
	raw = raw[first : last+1]

	width = max(1, width)
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(line, " ")
		// Glamour word-wraps prose to width but leaves preformatted content
		// (code blocks, long URLs, table cells, and stack traces) at its natural
		// width. Hardwrap preserves all content across display rows.
		lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
	}
	return lines
}

func sanitizeRenderedOutput(rendered string) string {
	rendered = strings.ReplaceAll(rendered, "\r\n", "\n")
	rendered = strings.ReplaceAll(rendered, "\r", "\n")

	var out strings.Builder
	styleActive := false
	parser := ansi.NewParser()
	parser.SetHandler(ansi.Handler{
		Print: func(r rune) {
			if !textsafe.IsUnsafeTerminalRune(r) {
				out.WriteRune(r)
			}
		},
		Execute: func(control byte) {
			if control == '\n' || control == '\t' {
				out.WriteByte(control)
			}
		},
		HandleCsi: func(command ansi.Cmd, params ansi.Params) {
			if command.Prefix() != 0 || command.Intermediate() != 0 || command.Final() != 'm' {
				return
			}
			effect, ok := safeSGREffect(params)
			if !ok {
				return
			}
			out.WriteString("\x1b[")
			if len(params) == 0 {
				out.WriteByte('0')
			} else {
				// safeSGREffect rejects colon subparameters, so every
				// surviving param is ';'-separated.
				params.ForEach(0, func(i, param int, _ bool) {
					if i > 0 {
						out.WriteByte(';')
					}
					out.WriteString(strconv.Itoa(param))
				})
			}
			out.WriteByte('m')
			switch effect {
			case sgrReset:
				styleActive = false
			case sgrSet:
				styleActive = true
			}
		},
	})
	for i := range len(rendered) {
		parser.Advance(rendered[i])
	}
	if styleActive {
		out.WriteString("\x1b[0m")
	}
	return out.String()
}

type sgrEffect uint8

const (
	sgrUnchanged sgrEffect = iota
	sgrReset
	sgrSet
)

// safeSGREffect accepts only text styling and color operations used by the
// built-in and external Markdown renderers. Modes that can conceal, blink, or
// reverse content are rejected with the whole sequence.
func safeSGREffect(params ansi.Params) (sgrEffect, bool) {
	if len(params) == 0 {
		return sgrReset, true
	}

	values := make([]int, 0, len(params))
	valid := true
	params.ForEach(0, func(_ int, param int, hasMore bool) {
		if hasMore {
			valid = false
		}
		values = append(values, param)
	})
	if !valid {
		return sgrUnchanged, false
	}

	effect := sgrUnchanged
	for i := 0; i < len(values); {
		code := values[i]
		switch {
		case code == 0:
			effect = sgrReset
			i++
		case code == 1 || code == 2 || code == 3 || code == 4 || code == 9,
			code >= 30 && code <= 37,
			code >= 40 && code <= 47,
			code >= 90 && code <= 97,
			code >= 100 && code <= 107:
			effect = sgrSet
			i++
		case code == 22 || code == 23 || code == 24 || code == 29 ||
			code == 39 || code == 49:
			i++
		case code == 38 || code == 48:
			consumed, ok := safeExtendedColor(values[i:])
			if !ok {
				return sgrUnchanged, false
			}
			effect = sgrSet
			i += consumed
		default:
			return sgrUnchanged, false
		}
	}
	return effect, true
}

func safeExtendedColor(values []int) (int, bool) {
	if len(values) >= 3 && values[1] == 5 && values[2] >= 0 && values[2] <= 255 {
		return 3, true
	}
	if len(values) >= 5 && values[1] == 2 {
		for _, component := range values[2:5] {
			if component < 0 || component > 255 {
				return 0, false
			}
		}
		return 5, true
	}
	return 0, false
}

func styleConfig(codeBackground *string) glamansi.StyleConfig {
	bold := true
	italic := true
	zero := uint(0)
	quoteToken := "| "
	return glamansi.StyleConfig{
		Document: glamansi.StyleBlock{Margin: &zero},
		BlockQuote: glamansi.StyleBlock{
			Indent:      &zero,
			IndentToken: &quoteToken,
		},
		Paragraph: glamansi.StyleBlock{Margin: &zero},
		Heading: glamansi.StyleBlock{
			StylePrimitive: glamansi.StylePrimitive{Bold: &bold},
			Margin:         &zero,
		},
		H1:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H2:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H3:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H4:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H5:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H6:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		Strong: glamansi.StylePrimitive{Bold: &bold},
		Emph:   glamansi.StylePrimitive{Italic: &italic},
		Item:   glamansi.StylePrimitive{BlockPrefix: "- "},
		Enumeration: glamansi.StylePrimitive{
			BlockPrefix: ". ",
		},
		Task: glamansi.StyleTask{Ticked: "[x] ", Unticked: "[ ] "},
		Link: glamansi.StylePrimitive{Underline: &bold},
		Code: glamansi.StyleBlock{
			StylePrimitive: glamansi.StylePrimitive{Prefix: "`", Suffix: "`"},
		},
		CodeBlock: glamansi.StyleCodeBlock{
			StyleBlock: glamansi.StyleBlock{
				StylePrimitive: glamansi.StylePrimitive{
					BackgroundColor: codeBackground,
				},
				Margin: &zero,
			},
		},
		HorizontalRule: glamansi.StylePrimitive{Format: ""},
		ImageText: glamansi.StylePrimitive{
			Format: "[image: {{.text}}]",
		},
		List: glamansi.StyleList{
			StyleBlock:  glamansi.StyleBlock{Margin: &zero},
			LevelIndent: 2,
		},
	}
}
