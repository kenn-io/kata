// Command sign-release-checksums signs kata release checksum metadata.
package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.kenn.io/kit/selfupdate"
)

var assetNamePattern = regexp.MustCompile(`^kata_[^_]+_([^_]+)_([^_.]+)\.(?:tar\.gz|zip)$`)

func main() {
	owner := flag.String("owner", "", "release owner")
	repo := flag.String("repo", "", "release repository")
	version := flag.String("version", "", "release tag, including v prefix")
	checksumsPath := flag.String("checksums", "SHA256SUMS", "sha256sum file")
	flag.Parse()

	if *owner == "" || *repo == "" || *version == "" {
		fatalf("--owner, --repo, and --version are required")
	}
	privateKey, err := privateKeyFromEnv()
	if err != nil {
		fatalf("%v", err)
	}
	if err := signChecksums(*checksumsPath, *owner, *repo, *version, privateKey); err != nil {
		fatalf("%v", err)
	}
}

func privateKeyFromEnv() (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(os.Getenv("KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX"))
	if raw == "" {
		return nil, fmt.Errorf("KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX is required")
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX: %w", err)
	}
	switch len(key) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(key), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(key), nil
	default:
		return nil, fmt.Errorf("KATA_UPDATE_SIGNING_PRIVATE_KEY_HEX must be %d-byte seed or %d-byte private key, got %d bytes", ed25519.SeedSize, ed25519.PrivateKeySize, len(key))
	}
}

func signChecksums(path, owner, repo, version string, privateKey ed25519.PrivateKey) error {
	//nolint:gosec // release workflow passes the repository-generated checksum file path.
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	dir := filepath.Dir(path)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		checksum := strings.ToLower(fields[0])
		asset := strings.TrimPrefix(fields[1], "*")
		goos, goarch, ok := assetPlatform(asset)
		if !ok {
			return fmt.Errorf("cannot infer platform from asset name %q", asset)
		}
		payload := selfupdate.SignaturePayload(selfupdate.SignatureMetadata{
			Owner:    owner,
			Repo:     repo,
			Version:  version,
			Asset:    asset,
			GOOS:     goos,
			GOARCH:   goarch,
			Checksum: checksum,
		})
		signature := ed25519.Sign(privateKey, payload)
		sigPath := filepath.Join(dir, asset+".sha256.sig")
		if err := os.WriteFile(sigPath, []byte(hex.EncodeToString(signature)+"\n"), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", sigPath, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func assetPlatform(asset string) (string, string, bool) {
	matches := assetNamePattern.FindStringSubmatch(asset)
	if len(matches) != 3 {
		return "", "", false
	}
	return matches[1], matches[2], true
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
