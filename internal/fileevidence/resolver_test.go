package fileevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: content identity covers complete raw bytes; metadata is irrelevant.
func TestLocalCaptureAndRead(t *testing.T) {
	root := t.TempDir()
	raw := []byte{0xef, 0xbb, 0xbf, 'a', '\r', '\n', 0xff, 0}
	p := filepath.Join(root, "report.md")
	require.NoError(t, os.WriteFile(p, raw, 0600))
	cfg := Config{Sources: map[string]Source{"notes": {Root: root, SHA256: true}}}
	ref, err := cfg.Capture(t.Context(), Reference{Source: "notes", Path: "report.md"}, "origin")
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(sum[:]), ref.SHA256)
	require.Equal(t, int64(len(raw)), *ref.Size)
	require.Equal(t, "origin", ref.Origin)
	require.Equal(t, "unchanged", cfg.Read(t.Context(), ref, "origin", false).Status)
	require.NoError(t, os.Chtimes(p, time.Now(), time.Now()))
	require.Equal(t, "unchanged", cfg.Read(t.Context(), ref, "origin", false).Status)
	raw[0] ^= 1
	require.NoError(t, os.WriteFile(p, raw, 0600))
	require.Equal(t, "changed", cfg.Read(t.Context(), ref, "origin", false).Status)
	require.Equal(t, "history-unavailable", cfg.Read(t.Context(), ref, "origin", true).Status)
	require.NoError(t, os.Remove(p))
	require.Equal(t, "missing", cfg.Read(t.Context(), ref, "origin", false).Status)
	require.Equal(t, "foreign-origin", cfg.Read(t.Context(), ref, "another-origin", false).Status)
}

func TestCapturePolicyAndConfinement(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "ok"), nil, 0600))
	cfg := Config{Sources: map[string]Source{"notes": {Root: root}}}
	ref, err := cfg.Capture(t.Context(), Reference{Source: "notes", Path: "ok"}, "origin")
	require.NoError(t, err)
	require.Empty(t, ref.SHA256)
	require.Equal(t, int64(0), *ref.Size)
	require.Equal(t, "unverified", cfg.Read(t.Context(), ref, "origin", false).Status)
	yes := true
	ref, err = cfg.Capture(t.Context(), Reference{Source: "notes", Path: "ok", CaptureSHA256: &yes}, "origin")
	require.NoError(t, err)
	require.Len(t, ref.SHA256, 64)
	require.Nil(t, ref.CaptureSHA256)
	for _, p := range []string{"../ok", "/ok", "a/../ok", "./ok", "a//ok", `a\ok`, ".", "", "a\x00b"} {
		_, err = cfg.Capture(t.Context(), Reference{Source: "notes", Path: p}, "origin")
		require.Error(t, err, p)
	}
	require.NoError(t, os.Mkdir(filepath.Join(root, "dir"), 0700))
	require.NoError(t, os.Symlink(filepath.Join(root, "ok"), filepath.Join(root, "link")))
	for _, p := range []string{"dir", "link"} {
		_, err = cfg.Capture(t.Context(), Reference{Source: "notes", Path: p}, "origin")
		require.Error(t, err, p)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "large"), make([]byte, MaxBytes+1), 0600))
	_, err = cfg.Capture(t.Context(), Reference{Source: "notes", Path: "large"}, "origin")
	require.Error(t, err)
}

// The configured command is the only executable; input is data on stdin.
func TestAdapter(t *testing.T) {
	cfg := Config{Sources: map[string]Source{"remote": {Command: []string{os.Args[0], "-test.run=^TestResolverProcess$"}, SHA256: true}}}
	for _, p := range []string{"binary", "missing", "history-unavailable", "unreachable", "mismatch", "bad-json", "trailing", "failure"} {
		t.Run(p, func(t *testing.T) {
			ref, err := cfg.Capture(t.Context(), Reference{Source: "remote", Path: p}, "origin")
			if p != "binary" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "revision-one", ref.Revision)
			got := cfg.Read(t.Context(), ref, "origin", true)
			require.Equal(t, "unchanged", got.Status)
			require.Equal(t, []byte{0, 255, 13, 10}, got.Content)
		})
	}
}

func TestResolverProcess(t *testing.T) {
	// Only the resolver subprocess has stdin redirected to a pipe.
	st, err := os.Stdin.Stat()
	require.NoError(t, err)
	if st.Mode()&os.ModeCharDevice != 0 {
		return
	}
	var request struct {
		Version  int    `json:"version"`
		Source   string `json:"source"`
		Path     string `json:"path"`
		Revision string `json:"revision"`
		SHA256   string `json:"sha256"`
	}
	if json.Unmarshal(func() []byte { b, _ := io.ReadAll(os.Stdin); return b }(), &request) != nil || request.Version != 1 {
		return
	}
	response := map[string]any{"version": 1, "source": request.Source, "path": request.Path, "status": "ok", "content_base64": base64.StdEncoding.EncodeToString([]byte{0, 255, 13, 10}), "revision": "revision-one"}
	switch request.Path {
	case "environment":
		response["content_base64"] = base64.StdEncoding.EncodeToString([]byte(os.Getenv("KATA_FILE_EVIDENCE_SYNTHETIC_SECRET")))
	case "wait":
		<-t.Context().Done()
	case "missing", "history-unavailable", "unreachable":
		response["status"] = request.Path
	case "mismatch":
		response["path"] = "another"
	case "bad-json":
		fmt.Print("{")
		os.Exit(0)
	case "trailing":
		fmt.Print(`{} {}`)
		os.Exit(0)
	case "failure":
		fmt.Fprint(os.Stderr, "credential-must-stay-private")
		os.Exit(2)
	}
	b, _ := json.Marshal(response)
	fmt.Print(string(b))
	os.Exit(0)
}

func TestRawBytesIdentityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		got := describe(Reference{Source: "notes", Path: "x"}, raw, true)
		sum := sha256.Sum256(raw)
		if got.SHA256 != hex.EncodeToString(sum[:]) || *got.Size != int64(len(raw)) {
			ht.Fatalf("raw byte identity mismatch")
		}

	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestPortablePathProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		ref := Reference{Source: "notes", Path: text}
		if ref.ValidateRequest() == nil && (filepath.IsAbs(text) || bytes.ContainsAny([]byte(text), "\\:\x00")) {
			ht.Fatalf("nonportable path accepted: %q", text)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestFileRequestRejectsSuppliedProvenance(t *testing.T) {
	for _, field := range []string{`"origin":""`, `"sha256":""`, `"size":null`, `"size":0`, `"revision":""`} {
		var ref Reference
		require.NoError(t, json.Unmarshal([]byte(`{"source":"notes","path":"x",`+field+`}`), &ref))
		require.Error(t, ref.ValidateRequest(), field)
	}
}

func TestSourceConfigValidation(t *testing.T) {
	for _, source := range []Source{{}, {Root: "relative"}, {Root: t.TempDir(), Command: []string{"adapter"}}, {Command: []string{""}}, {Command: []string{"adapter"}, Env: []string{"INVALID=VALUE"}}} {
		require.Error(t, (Config{Sources: map[string]Source{"notes": source}}).Validate())
	}
	require.NoError(t, (Config{Sources: map[string]Source{"notes": {Root: t.TempDir()}, "remote": {Command: []string{"adapter"}}}}).Validate())
}

func TestRequestedContentProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := append([]byte{}, hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))...)
		encoded, err := json.Marshal(Result{Status: "unverified", Content: raw})
		if err != nil {
			ht.Fatal(err)
		}
		var value map[string]any
		if err = json.Unmarshal(encoded, &value); err != nil {
			ht.Fatal(err)
		}
		content, ok := value["content"].(string)
		if !ok {
			ht.Fatal("requested successful bytes missing, including empty file")
		}
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil || !bytes.Equal(raw, decoded) {
			ht.Fatal("content bytes changed")
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestAdapterCallerCancellation(t *testing.T) {
	cfg := Config{Sources: map[string]Source{"remote": {Command: []string{os.Args[0], "-test.run=^TestResolverProcess$"}}}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := cfg.Capture(ctx, Reference{Source: "remote", Path: "wait"}, "origin")
	require.ErrorContains(t, err, "unreachable")
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestFileReferenceRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		ref := describe(Reference{Source: "notes", Path: "x", Origin: "origin", Revision: hegel.Draw(ht, hegel.Text())}, raw, hegel.Draw(ht, hegel.Booleans()))
		encoded, err := json.Marshal(ref)
		if err != nil {
			ht.Fatal(err)
		}
		var roundtrip Reference
		if err = json.Unmarshal(encoded, &roundtrip); err != nil {
			ht.Fatal(err)
		}
		if roundtrip.Source != ref.Source || roundtrip.Path != ref.Path || roundtrip.Origin != ref.Origin || roundtrip.Revision != ref.Revision || roundtrip.SHA256 != ref.SHA256 || *roundtrip.Size != *ref.Size {
			ht.Fatal("wire reference changed")
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestCaptureHashPolicyProperty(t *testing.T) {
	root := t.TempDir()
	hegel.Test(t, func(ht *hegel.T) {
		sourceHash := hegel.Draw(ht, hegel.Booleans())
		override := hegel.Draw(ht, hegel.Integers[int](0, 2))
		ref := Reference{Source: "notes", Path: "x"}
		expected := sourceHash
		if override > 0 {
			value := override == 2
			ref.CaptureSHA256 = &value
			expected = value
		}
		raw := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		if err := os.WriteFile(filepath.Join(root, "x"), raw, 0600); err != nil {
			ht.Fatal(err)
		}
		got, err := (Config{Sources: map[string]Source{"notes": {Root: root, SHA256: sourceHash}}}).Capture(t.Context(), ref, "origin")
		if len(raw) > MaxBytes {
			if err == nil {
				ht.Fatal("oversize capture accepted")
			}
			return
		}
		if err != nil {
			ht.Fatal(err)
		}
		if (got.SHA256 != "") != expected {
			ht.Fatal("capture preference changed")
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestMissingConfiguredEnvironmentFailsClosed(t *testing.T) {
	const key = "KATA_FILE_EVIDENCE_SYNTHETIC_UNSET"
	t.Setenv(key, "")
	require.NoError(t, os.Unsetenv(key))
	cfg := Config{Sources: map[string]Source{"remote": {Command: []string{os.Args[0], "-test.run=^TestResolverProcess$"}, Env: []string{key}}}}
	_, err := cfg.Capture(t.Context(), Reference{Source: "remote", Path: "binary"}, "origin")
	require.Error(t, err, "an explicitly configured env source must not silently fall back to ambient adapter credentials")
}

func TestAdapterDoesNotInheritMissingRuntimeEnv(t *testing.T) {
	if os.Getenv("KATA_FILE_EVIDENCE_EMPTY_ENV_TEST") == "inner" {
		cfg := Config{Sources: map[string]Source{"remote": {Command: []string{os.Args[0], "-test.run=^TestResolverProcess$"}, SHA256: true}}}
		got, err := cfg.Capture(t.Context(), Reference{Source: "remote", Path: "environment"}, "origin")
		require.NoError(t, err)
		empty := sha256.Sum256(nil)
		require.Equal(t, hex.EncodeToString(empty[:]), got.SHA256, "ambient credentials must not reach an adapter when the runtime allowlist is empty")
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAdapterDoesNotInheritMissingRuntimeEnv$") //nolint:gosec // This subprocess reruns this exact synthetic test binary.
	command.Env = []string{"KATA_FILE_EVIDENCE_EMPTY_ENV_TEST=inner", "KATA_FILE_EVIDENCE_SYNTHETIC_SECRET=not-allowed"}
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}
