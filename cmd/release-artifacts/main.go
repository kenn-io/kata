// Command release_artifacts assembles and verifies Kata release-only artifacts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	releasepkg "go.kenn.io/kata/internal/release"
)

const releaseArtifactsUsage = "usage: release_artifacts <build-source|verify-source|render-homebrew-core> [flags]"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(releaseArtifactsUsage)
	}
	switch args[0] {
	case "build-source":
		return runBuildSource(ctx, args[1:], stdout, stderr)
	case "verify-source":
		return runVerifySource(ctx, args[1:], stdout, stderr)
	case "render-homebrew-core":
		return runRenderHomebrewCore(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown subcommand %q; %s", args[0], releaseArtifactsUsage)
	}
}

func runBuildSource(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("build-source", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repoRoot := flags.String("repo", ".", "repository root")
	version := flags.String("version", "", "release or snapshot version")
	tag := flags.String("tag", "", "release tag")
	snapshot := flags.Bool("snapshot", false, "build from HEAD without a release tag")
	output := flags.String("output", "", "source archive output path")
	metadata := flags.String("metadata", "", "metadata JSON output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *version == "" || *output == "" || *metadata == "" {
		return errors.New("build-source requires --version, --output, and --metadata")
	}
	meta, err := releasepkg.BuildSourceArchive(ctx, releasepkg.SourceArchiveOptions{
		RepoRoot: *repoRoot, Version: *version, Tag: *tag, Snapshot: *snapshot, Output: *output,
	})
	if err != nil {
		return err
	}
	if err := releasepkg.WriteSourceArchiveMetadata(*metadata, meta); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "built %s sha256=%s\n", *output, meta.SHA256)
	return err
}

func runVerifySource(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify-source", flag.ContinueOnError)
	flags.SetOutput(stderr)
	archive := flags.String("archive", "", "source archive path")
	metadata := flags.String("metadata", "", "metadata JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *archive == "" || *metadata == "" {
		return errors.New("verify-source requires --archive and --metadata")
	}
	meta, err := releasepkg.ReadSourceArchiveMetadata(*metadata)
	if err != nil {
		return err
	}
	if err := releasepkg.VerifySourceArchive(ctx, *archive, meta); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "verified %s\n", *archive)
	return err
}

func runRenderHomebrewCore(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("render-homebrew-core", flag.ContinueOnError)
	flags.SetOutput(stderr)
	templatePath := flags.String("template", "", "Homebrew formula template path")
	output := flags.String("output", "", "rendered formula output path")
	metadata := flags.String("metadata", "", "source archive metadata JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *templatePath == "" || *output == "" || *metadata == "" {
		return errors.New("render-homebrew-core requires --template, --output, and --metadata")
	}
	meta, err := releasepkg.ReadSourceArchiveMetadata(*metadata)
	if err != nil {
		return err
	}
	if err := releasepkg.RenderHomebrewCoreFormula(*templatePath, *output, meta); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "rendered %s\n", *output)
	return err
}
