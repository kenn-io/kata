//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEmbeddingCredentialFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embedding.key")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan EmbeddingCredential, 1)
	go func() { done <- (EmbeddingsConfig{APIKeyFile: path}).ResolveCredential() }()
	select {
	case c := <-done:
		if c.Key != "" || !strings.Contains(c.Reason, "regular file") {
			t.Fatalf("unexpected reason %q", c.Reason)
		}
	case <-time.After(200 * time.Millisecond):
		// Release an implementation that blocked in open before failing the test.
		f, err := os.OpenFile(path, os.O_RDWR, 0600) //nolint:gosec // G304: test-owned FIFO under t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		<-done
		_ = f.Close()
		t.Fatal("credential resolver blocked opening a FIFO")
	}
}

func TestOpenEmbeddingCredentialFileFIFOIsNonblocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "embedding.key")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		f, err := openEmbeddingCredentialFile(path)
		done <- result{file: f, err: err}
	}()
	select {
	case opened := <-done:
		if opened.err != nil {
			t.Fatal(opened.err)
		}
		defer func() { _ = opened.file.Close() }()
		info, err := opened.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() {
			t.Fatal("FIFO opened as a regular file")
		}
	case <-time.After(200 * time.Millisecond):
		// Release an implementation that opened the FIFO in blocking mode.
		writer, err := os.OpenFile(path, os.O_RDWR, 0600) //nolint:gosec // G304: test-owned FIFO under t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		opened := <-done
		if opened.file != nil {
			_ = opened.file.Close()
		}
		_ = writer.Close()
		t.Fatal("credential file open blocked on a FIFO")
	}
}

func TestEmbeddingCredentialFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.key")
	if err := os.WriteFile(target, []byte("file-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "embedding.key")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	c := (EmbeddingsConfig{APIKeyFile: path}).ResolveCredential()
	if c.Key != "" || !strings.Contains(c.Reason, "symbolic link") {
		t.Fatalf("expected a readable symlink rejection, got key match=%t reason=%q", c.Key == "file-secret", c.Reason)
	}
}

func TestEmbeddingCredentialFileRejectsSymlinkBeforeParentTraversal(t *testing.T) {
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(targetDir, "subdir"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "embedding.key")
	if err := os.WriteFile(target, []byte("file-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(targetDir, "subdir"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "embedding.key"), []byte("wrong-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	path := dir + string(os.PathSeparator) + "alias" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "embedding.key"

	c := (EmbeddingsConfig{APIKeyFile: path}).ResolveCredential()
	if c.Key != "" || !strings.Contains(c.Reason, "symbolic link") {
		t.Fatalf("expected a readable symlink rejection, got key match=%t reason=%q", c.Key == "wrong-secret", c.Reason)
	}
}

func TestEmbeddingCredentialFileRejectsWritableParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0777); err != nil { //nolint:gosec // G302: test-owned unsafe directory fixture.
		t.Fatal(err)
	}
	path := filepath.Join(dir, "embedding.key")
	if err := os.WriteFile(path, []byte("file-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}

	c := (EmbeddingsConfig{APIKeyFile: path}).ResolveCredential()
	if c.Key != "" || !strings.Contains(c.Reason, "parent directories") {
		t.Fatalf("expected a readable parent-directory rejection, got key match=%t reason=%q", c.Key == "file-secret", c.Reason)
	}
}
