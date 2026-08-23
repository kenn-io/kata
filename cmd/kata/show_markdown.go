package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/charmbracelet/colorprofile"
	"go.kenn.io/kata/internal/colormode"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/markdownrender"
	"go.kenn.io/kata/internal/processtree"
)

const (
	rendererTimeout = 10 * time.Second
	rendererGrace   = 2 * time.Second
)

type markdownFieldKind string

const (
	markdownDescription markdownFieldKind = "description"
	markdownComment     markdownFieldKind = "comment"
)

type showMarkdownRenderer interface {
	Render(context.Context, markdownFieldKind, string, int) (string, error)
}

type builtinShowMarkdownRenderer struct {
	codeBlockBackground *string
}

func newBuiltinShowMarkdownRenderer(
	profile colorprofile.Profile,
	mode colormode.Mode,
) showMarkdownRenderer {
	var background *string
	switch profile {
	case colorprofile.ANSI, colorprofile.ANSI256, colorprofile.TrueColor:
		var value string
		switch mode {
		case colormode.Light:
			value = markdownrender.CodeBlockBackground(false)
		case colormode.Dark:
			value = markdownrender.CodeBlockBackground(true)
		}
		if value != "" {
			background = &value
		}
	}
	return &builtinShowMarkdownRenderer{codeBlockBackground: background}
}

func (r *builtinShowMarkdownRenderer) Render(
	_ context.Context,
	_ markdownFieldKind,
	markdown string,
	width int,
) (string, error) {
	return markdownrender.Render(markdown, markdownrender.Options{
		Width: width, CodeBlockBackground: r.codeBlockBackground,
	})
}

type externalShowMarkdownRenderer struct {
	argv    []string
	timeout time.Duration
	grace   time.Duration
}

func newExternalShowMarkdownRenderer(argv []string) showMarkdownRenderer {
	return &externalShowMarkdownRenderer{
		argv:    append([]string(nil), argv...),
		timeout: rendererTimeout,
		grace:   rendererGrace,
	}
}

func (r *externalShowMarkdownRenderer) Render(
	ctx context.Context,
	kind markdownFieldKind,
	markdown string,
	_ int,
) (string, error) {
	renderCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	//nolint:gosec // G204: argv is the explicit local user-configured Markdown renderer command.
	cmd := exec.CommandContext(renderCtx, r.argv[0], r.argv[1:]...)
	cmd.Stdin = bytes.NewBufferString(markdownrender.SanitizeInput(markdown))
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	tree, err := processtree.New(cmd)
	if err != nil {
		return "", rendererError(r.argv[0], kind, err)
	}
	defer func() { _ = tree.Close() }()
	cmd.WaitDelay = r.grace
	cmd.Cancel = func() error {
		return tree.TerminateWithGrace(r.grace)
	}

	if err := tree.Start(); err != nil {
		return "", rendererError(r.argv[0], kind, err)
	}
	if err := tree.Wait(); err != nil {
		// Prefer the context cause: a killed renderer reports the timeout or
		// cancellation rather than its incidental exit status.
		if cause := renderCtx.Err(); cause != nil {
			return "", rendererError(r.argv[0], kind, cause)
		}
		return "", rendererError(r.argv[0], kind, err)
	}
	return stdout.String(), nil
}

func rendererError(executable string, kind markdownFieldKind, cause error) error {
	return fmt.Errorf("markdown renderer %q failed for %s: %w", executable, kind, cause)
}

func configuredShowMarkdownRenderer(
	display config.DisplayConfig,
	rows *rowRenderer,
) showMarkdownRenderer {
	if len(display.MarkdownRenderer) == 0 {
		return rows.markdownRenderer()
	}
	return newExternalShowMarkdownRenderer(display.MarkdownRenderer)
}
