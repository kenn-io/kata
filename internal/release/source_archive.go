package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"
	"time"
)

var releaseVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$`)

// SourceArchiveOptions describes a deterministic release source archive build.
type SourceArchiveOptions struct {
	RepoRoot string
	Version  string
	Tag      string
	Snapshot bool
	Output   string
}

// SourceArchiveMetadata records immutable inputs used by the Core formula.
type SourceArchiveMetadata struct {
	Version   string `json:"version"`
	Tag       string `json:"tag"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	Archive   string `json:"archive"`
	SHA256    string `json:"sha256"`
	Snapshot  bool   `json:"snapshot"`
}

// BuildSourceArchive creates a release source archive with production web assets.
func BuildSourceArchive(ctx context.Context, opts SourceArchiveOptions) (SourceArchiveMetadata, error) {
	if opts.RepoRoot == "" || opts.Version == "" || opts.Output == "" {
		return SourceArchiveMetadata{}, errors.New("repo root, version, and output are required")
	}
	repoRoot, err := filepath.Abs(opts.RepoRoot)
	if err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("resolve repo root: %w", err)
	}
	ref := "HEAD"
	if !opts.Snapshot {
		if !releaseVersionPattern.MatchString(opts.Version) {
			return SourceArchiveMetadata{}, fmt.Errorf("invalid release version %q", opts.Version)
		}
		wantTag := "v" + opts.Version
		if opts.Tag != wantTag {
			return SourceArchiveMetadata{}, fmt.Errorf("release tag %q does not match %q", opts.Tag, wantTag)
		}
		ref = opts.Tag
		head, err := gitOutput(ctx, repoRoot, "rev-parse", "HEAD")
		if err != nil {
			return SourceArchiveMetadata{}, err
		}
		tagCommit, err := gitOutput(ctx, repoRoot, "rev-parse", opts.Tag+"^{commit}")
		if err != nil {
			return SourceArchiveMetadata{}, err
		}
		if head != tagCommit {
			return SourceArchiveMetadata{}, fmt.Errorf("release tag %s resolves to %s, checked-out HEAD is %s", opts.Tag, tagCommit, head)
		}
	}
	commit, err := gitOutput(ctx, repoRoot, "rev-parse", ref+"^{commit}")
	if err != nil {
		return SourceArchiveMetadata{}, err
	}
	buildDate, err := gitOutput(ctx, repoRoot, "show", "-s", "--format=%cI", commit)
	if err != nil {
		return SourceArchiveMetadata{}, err
	}
	commitTime, err := time.Parse(time.RFC3339, buildDate)
	if err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("parse commit timestamp %q: %w", buildDate, err)
	}
	buildDate = commitTime.UTC().Format(time.RFC3339)

	archiveBytes, err := gitArchive(ctx, repoRoot, ref)
	if err != nil {
		return SourceArchiveMetadata{}, err
	}
	vendorRoot, cleanupVendor, err := vendorDependencies(ctx, repoRoot)
	if err != nil {
		return SourceArchiveMetadata{}, err
	}
	defer cleanupVendor()
	if err := os.MkdirAll(filepath.Dir(opts.Output), 0o750); err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("create archive directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(opts.Output), "."+filepath.Base(opts.Output)+".tmp-*")
	if err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("create temporary archive: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	root := "kata-" + opts.Version
	if err := writeSourceTarGzip(tmp, bytes.NewReader(archiveBytes), repoRoot, vendorRoot, root, commitTime.UTC()); err != nil {
		_ = tmp.Close()
		return SourceArchiveMetadata{}, err
	}
	if err := tmp.Close(); err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("close source archive: %w", err)
	}
	if err := os.Rename(tmpName, opts.Output); err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("publish source archive: %w", err)
	}
	contents, err := os.ReadFile(opts.Output)
	if err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("read source archive checksum: %w", err)
	}
	sum := sha256.Sum256(contents)
	meta := SourceArchiveMetadata{
		Version: opts.Version, Commit: commit, BuildDate: buildDate,
		Archive: filepath.Base(opts.Output), SHA256: hex.EncodeToString(sum[:]), Snapshot: opts.Snapshot,
	}
	if !opts.Snapshot {
		meta.Tag = opts.Tag
	}
	return meta, nil
}

// WriteSourceArchiveMetadata persists metadata for later release hooks.
func WriteSourceArchiveMetadata(metadataPath string, meta SourceArchiveMetadata) error {
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o750); err != nil {
		return fmt.Errorf("create metadata directory: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("encode source archive metadata: %w", err)
	}
	if err := os.WriteFile(metadataPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write source archive metadata: %w", err)
	}
	return nil
}

// ReadSourceArchiveMetadata loads metadata produced by BuildSourceArchive.
func ReadSourceArchiveMetadata(metadataPath string) (SourceArchiveMetadata, error) {
	contents, err := os.ReadFile(metadataPath) //nolint:gosec // explicit release-hook path
	if err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("read source archive metadata: %w", err)
	}
	var meta SourceArchiveMetadata
	if err := json.Unmarshal(contents, &meta); err != nil {
		return SourceArchiveMetadata{}, fmt.Errorf("decode source archive metadata: %w", err)
	}
	return meta, nil
}

// VerifySourceArchive builds and inspects an assembled source archive in a clean tree.
func VerifySourceArchive(ctx context.Context, archivePath string, meta SourceArchiveMetadata) error {
	contents, err := os.ReadFile(archivePath) //nolint:gosec // explicit release-hook path
	if err != nil {
		return fmt.Errorf("read source archive: %w", err)
	}
	if meta.SHA256 != "" {
		sum := sha256.Sum256(contents)
		if got := hex.EncodeToString(sum[:]); got != meta.SHA256 {
			return fmt.Errorf("source archive checksum mismatch: got %s want %s", got, meta.SHA256)
		}
	}
	tmpDir, err := os.MkdirTemp("", "kata-source-verify-*")
	if err != nil {
		return fmt.Errorf("create source verification directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	if err := extractSourceArchive(bytes.NewReader(contents), tmpDir, "kata-"+meta.Version); err != nil {
		return err
	}
	sourceRoot := filepath.Join(tmpDir, "kata-"+meta.Version)
	bin := verificationBinaryPath(tmpDir, runtime.GOOS)
	ldflags := strings.Join([]string{
		"-X", "go.kenn.io/kata/internal/version.Version=v" + meta.Version,
		"-X", "go.kenn.io/kata/internal/version.Distribution=homebrew",
		"-X", "go.kenn.io/kata/internal/version.BuildDate=" + meta.BuildDate,
	}, " ")
	build := exec.CommandContext(ctx, "go", "build", "-mod=vendor", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, "./cmd/kata") //nolint:gosec // fixed build command with release metadata
	build.Dir = sourceRoot
	build.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOMODCACHE="+filepath.Join(tmpDir, "gomodcache"),
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOWORK=off",
		"HOME="+filepath.Join(tmpDir, "home"),
	)
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build source archive: %w\n%s", err, out)
	}
	if out, err := runVerifiedBinary(ctx, bin, "_web-assets-check"); err != nil {
		return fmt.Errorf("validate embedded web assets: %w\n%s", err, out)
	}
	out, err := runVerifiedBinary(ctx, bin, "version", "--json")
	if err != nil {
		return fmt.Errorf("inspect source archive build: %w\n%s", err, out)
	}
	var got struct {
		Version      string `json:"version"`
		Commit       string `json:"commit"`
		Built        string `json:"built"`
		Distribution string `json:"distribution"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return fmt.Errorf("decode source archive version: %w", err)
	}
	if got.Version != "v"+meta.Version || got.Distribution != "homebrew" || got.Built != meta.BuildDate || got.Commit != "unknown" {
		return fmt.Errorf("unexpected source build metadata: version=%q commit=%q built=%q distribution=%q", got.Version, got.Commit, got.Built, got.Distribution)
	}
	return nil
}

// RenderHomebrewCoreFormula renders the Core source formula from release metadata.
func RenderHomebrewCoreFormula(templatePath, outputPath string, meta SourceArchiveMetadata) error {
	tmpl, err := template.New(filepath.Base(templatePath)).Option("missingkey=error").ParseFiles(templatePath)
	if err != nil {
		return fmt.Errorf("parse Homebrew formula template: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("create Homebrew formula directory: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, meta); err != nil {
		return fmt.Errorf("render Homebrew formula: %w", err)
	}
	if err := os.WriteFile(outputPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write Homebrew formula: %w", err)
	}
	return nil
}

func gitOutput(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // arguments are fixed by release code
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitArchive(ctx context.Context, repoRoot, ref string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-c", "core.autocrlf=false", "-c", "core.eol=lf", "archive", "--format=tar", ref) //nolint:gosec // ref was validated as HEAD or the exact release tag
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git archive %s: %w", ref, err)
	}
	return out, nil
}

func vendorDependencies(ctx context.Context, repoRoot string) (string, func(), error) {
	tmpDir, err := os.MkdirTemp("", "kata-source-vendor-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create vendor directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	vendorRoot := filepath.Join(tmpDir, "vendor")
	cmd := exec.CommandContext(ctx, "go", "mod", "vendor", "-o", vendorRoot) //nolint:gosec // fixed release assembly command
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("vendor source dependencies: %w\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(vendorRoot, "modules.txt")); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("validate vendored dependencies: %w", err)
	}
	return vendorRoot, cleanup, nil
}

func writeSourceTarGzip(dst io.Writer, tracked io.Reader, repoRoot, vendorRoot, root string, modTime time.Time) error {
	gz := gzip.NewWriter(dst)
	gz.ModTime = modTime
	gz.OS = 255
	tw := tar.NewWriter(gz)
	tr := tar.NewReader(tracked)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read git archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if name == "internal/web/dist" || strings.HasPrefix(name, "internal/web/dist/") || name == "vendor" || strings.HasPrefix(name, "vendor/") {
			continue
		}
		normalized := normalizedTarHeader(hdr, root+"/"+hdr.Name, modTime)
		if err := tw.WriteHeader(normalized); err != nil {
			return fmt.Errorf("write tracked source header: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // input is git archive output from the verified release commit
				return fmt.Errorf("write tracked source file: %w", err)
			}
		}
	}
	assetRoot := filepath.Join(repoRoot, "internal", "web", "dist")
	if err := writeArchiveTree(tw, assetRoot, root+"/internal/web/dist", modTime); err != nil {
		return fmt.Errorf("overlay production web assets: %w", err)
	}
	if err := writeArchiveTree(tw, vendorRoot, root+"/vendor", modTime); err != nil {
		return fmt.Errorf("overlay vendored dependencies: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close source tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close source gzip: %w", err)
	}
	return nil
}

func writeArchiveTree(tw *tar.Writer, sourceRoot, archiveRoot string, modTime time.Time) error {
	return filepath.WalkDir(sourceRoot, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("web asset symlink is not supported: %s", filePath)
		}
		rel, err := filepath.Rel(sourceRoot, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		name := archiveRoot
		if rel != "." {
			name += "/" + rel
		}
		if entry.IsDir() {
			name += "/"
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr = normalizedTarHeader(hdr, name, modTime)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		f, err := os.Open(filePath) //nolint:gosec // WalkDir confines paths to the production asset root
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		return errors.Join(copyErr, closeErr)
	})
}

func normalizedTarHeader(hdr *tar.Header, name string, modTime time.Time) *tar.Header {
	copyHeader := *hdr
	copyHeader.Name = name
	copyHeader.Uid = 0
	copyHeader.Gid = 0
	copyHeader.Uname = "root"
	copyHeader.Gname = "root"
	copyHeader.ModTime = modTime
	copyHeader.AccessTime = time.Time{}
	copyHeader.ChangeTime = time.Time{}
	copyHeader.PAXRecords = nil
	return &copyHeader
}

func extractSourceArchive(contents io.Reader, destination, expectedRoot string) error {
	gz, err := gzip.NewReader(contents)
	if err != nil {
		return fmt.Errorf("open source archive gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read source archive: %w", err)
		}
		cleanName := path.Clean(hdr.Name)
		if path.IsAbs(hdr.Name) || cleanName == "." || (cleanName != expectedRoot && !strings.HasPrefix(cleanName, expectedRoot+"/")) {
			return fmt.Errorf("unsafe source archive path %q", hdr.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(cleanName))
		if !pathWithin(destination, target) {
			return fmt.Errorf("source archive path escapes destination: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)&0o777); err != nil { //nolint:gosec // permission bits are masked before conversion
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(hdr.Mode)&0o777) //nolint:gosec // target is constrained to the extraction directory above
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr) //nolint:gosec // archive is generated and checksummed by this release pipeline
			closeErr := f.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			linkTarget := path.Clean(path.Join(path.Dir(cleanName), hdr.Linkname)) //nolint:gosec // result is constrained to expectedRoot below
			if linkTarget != expectedRoot && !strings.HasPrefix(linkTarget, expectedRoot+"/") {
				return fmt.Errorf("source archive link escapes root: %q", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
			continue
		default:
			return fmt.Errorf("unsupported source archive entry %q type %d", hdr.Name, hdr.Typeflag)
		}
	}
	return nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func runVerifiedBinary(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is the just-built temporary verifier and args are fixed by the caller
	return cmd.CombinedOutput()
}

func verificationBinaryPath(dir, goos string) string {
	name := "kata"
	if goos == "windows" {
		name += ".exe"
	}
	return filepath.Join(dir, name)
}
