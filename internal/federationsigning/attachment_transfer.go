package federationsigning

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

const (
	// AttachmentMaxBodyBytes bounds binary upload spools at every signing hop.
	AttachmentMaxBodyBytes = 256 << 20
	// AttachmentTransferTimeout is independent of the short signature admission.
	AttachmentTransferTimeout = 75 * time.Minute
)

// IsAttachmentTransfer selects only the two binary federation service routes.
// Callers pass the unescaped native API path, without any external mount prefix.
func IsAttachmentTransfer(method, path string) bool {
	if !signingpolicy.Unambiguous(&url.URL{Path: path}) || strings.ContainsAny(path, "?#") {
		return false
	}
	for _, c := range path {
		if c <= ' ' || c == 127 {
			return false
		}
	}
	p := strings.Split(path, "/")
	if len(p) != 10 || p[0] != "" || p[1] != "api" || p[2] != "v1" || p[3] != "projects" || p[5] != "federation" || p[6] != "docbank" {
		return false
	}
	id, err := strconv.ParseInt(p[4], 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != p[4] {
		return false
	}
	if method == http.MethodPost && p[7] == "issues" && p[8] != "" && p[9] == "uploads" {
		return true
	}
	if method != http.MethodGet || p[7] != "versions" || p[9] != "content" || len(p[8]) != 36 {
		return false
	}
	for i, c := range p[8] {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// bodySpool owns both its descriptor and its removable temporary file. A process
// crash can leave a private kata-federation-body-* file in the OS temp directory.
// Recovery may remove only those files after all Kata processes sharing that
// directory have stopped; never sweep files that another process may be using.
type bodySpool struct {
	*os.File
	once sync.Once
	err  error
}

func (s *bodySpool) Close() error {
	s.once.Do(func() { s.err = errors.Join(s.File.Close(), os.Remove(s.Name())) })
	return s.err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// spoolBody limits memory to a copy buffer and disk to the upload cap. Closing
// the input on cancellation also interrupts network bodies blocked in Read.
func spoolBody(ctx context.Context, body io.ReadCloser, length, limit int64) (_ *bodySpool, digest string, err error) {
	if body != nil {
		defer func() { _ = body.Close() }()
	}
	if length > limit {
		return nil, "", &http.MaxBytesError{Limit: limit}
	}
	f, err := os.CreateTemp("", "kata-federation-body-*")
	if err != nil {
		return nil, "", err
	}
	s := &bodySpool{File: f}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	var input io.Reader = http.NoBody
	if body != nil {
		input = body
		stop := context.AfterFunc(ctx, func() { _ = body.Close() })
		defer stop()
	}
	h := sha256.New()
	// Limit reads to cap+1, but never write the overflow byte to the spool.
	r := contextReader{ctx: ctx, reader: input}
	n, err := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(r, limit), make([]byte, 32<<10))
	if err != nil {
		return nil, "", err
	}
	var extra [1]byte
	if count, readErr := io.ReadFull(r, extra[:]); count != 0 {
		return nil, "", &http.MaxBytesError{Limit: limit}
	} else if readErr != io.EOF {
		return nil, "", readErr
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if length >= 0 && n != length {
		return nil, "", signingpolicy.ErrInvalid
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	return s, "sha-256=:" + base64.StdEncoding.EncodeToString(h.Sum(nil)) + ":", nil
}
