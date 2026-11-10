package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"go.kenn.io/kit/safefileio"
)

const (
	agentsViewSnapshotLimit int64 = 64 << 20
	agentsViewRecordLimit   int64 = 16 << 10
)

// A frozen snapshot owns a private transport spool, never the retained source.
// No pathname is reopened after validation or exposed in portable records.
type agentsViewFrozenSnapshot struct {
	*os.File
	digest   string
	size     int64
	once     sync.Once
	closeErr error
}

func (s *agentsViewFrozenSnapshot) Close() error {
	s.once.Do(func() {
		name := s.Name()
		s.closeErr = s.File.Close()
		if err := os.Remove(name); s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

type agentsViewContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r agentsViewContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func validAgentsViewDigest(digest string) bool {
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(digest) == digest
}

// Empty digest/length are permitted only during preparation, before consent.
func captureAgentsViewSnapshot(ctx context.Context, path string) (_ *agentsViewFrozenSnapshot, err error) {
	if path == "" {
		return nil, errors.New("AgentsView: --snapshot must name the retained file")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := openAgentsViewSelectedFile(ctx, path)
	if err != nil {
		return nil, errors.New("AgentsView: cannot open selected snapshot; retain the original file")
	}
	defer func() { _ = source.Close() }()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > agentsViewSnapshotLimit {
		return nil, errors.New("AgentsView: snapshot must be a nonempty regular file of at most 64 MiB")
	}
	file, err := safefileio.CreatePrivateTemp("", "kata-agentsview-*")
	if err != nil {
		return nil, errors.New("AgentsView: cannot make a private snapshot copy")
	}
	snapshot := &agentsViewFrozenSnapshot{File: file}
	defer func() {
		if err != nil {
			_ = snapshot.Close()
		}
	}()
	hash := sha256.New()
	snapshot.size, err = io.Copy(io.MultiWriter(file, hash), io.LimitReader(agentsViewContextReader{ctx, source}, agentsViewSnapshotLimit+1))
	if err != nil {
		return nil, errors.New("AgentsView: snapshot capture interrupted")
	}
	if snapshot.size <= 0 || snapshot.size > agentsViewSnapshotLimit {
		return nil, errors.New("AgentsView: snapshot exceeds transfer limit")
	}
	snapshot.digest = hex.EncodeToString(hash.Sum(nil))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func freezeAgentsViewSnapshot(ctx context.Context, path, digest string, size int64) (*agentsViewFrozenSnapshot, error) {
	if !validAgentsViewDigest(digest) || size <= 0 || size > agentsViewSnapshotLimit {
		return nil, errors.New("AgentsView: invalid snapshot digest or length")
	}
	snapshot, err := captureAgentsViewSnapshot(ctx, path)
	if err != nil {
		return nil, err
	}
	if snapshot.digest != digest || snapshot.size != size {
		_ = snapshot.Close()
		return nil, errors.New("AgentsView: snapshot changed; use the original reviewed file, never re-export for retry")
	}
	return snapshot, nil
}

func writeAgentsViewConsent(ctx context.Context, path string, data []byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" || len(data) == 0 || int64(len(data)) > agentsViewRecordLimit {
		return errors.New("AgentsView: invalid consent output")
	}
	file, err := safefileio.CreatePrivateFile(path)
	if err != nil {
		return errors.New("AgentsView: consent output must be a new private file")
	}
	err = writeAgentsViewPrivateContent(ctx, file, data)
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

type agentsViewPrivateOutput interface {
	io.Writer
	Sync() error
	Close() error
}

func writeAgentsViewPrivateContent(ctx context.Context, file agentsViewPrivateOutput, data []byte) (err error) {
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = &agentsViewSafeFailure{state: "consent publication failed", cause: closeErr}
		}
	}()
	if _, err = io.Copy(file, agentsViewContextReader{ctx, strings.NewReader(string(data))}); err != nil {
		return &agentsViewSafeFailure{state: "consent publication failed", cause: err}
	}
	if err = file.Sync(); err != nil {
		return &agentsViewSafeFailure{state: "consent publication failed", cause: err}
	}
	return nil
}
