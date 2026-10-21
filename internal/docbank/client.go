// Package docbank is Kata's transport-only Docbank client. Folder and upload
// handling follow msgvault/internal/docbankmedia; no Docbank packages are imported.
package docbank

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/metadata"
)

// Error keeps upstream details and credentials out of logs and API responses.
type Error struct {
	Kind   string
	Status int
}

func (e *Error) Error() string { return "docbank: " + e.Kind }

// ErrorKind returns the stable, credential-free failure category.
func ErrorKind(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	if err != nil {
		return "unreachable"
	}
	return ""
}

func fault(kind string) error { return &Error{Kind: kind} }
func statusError(n int) error {
	kind := "upstream"
	switch n {
	case 401, 403:
		kind = "unauthorized"
	case 404, 410:
		kind = "not_found"
	case 413:
		kind = "too_large"
	}
	return &Error{Kind: kind, Status: n}
}

// Client provides the pinned-content subset of the Docbank API.
type Client struct {
	base string
	key  func() (string, error)
	http *http.Client
}

// NewClient validates the endpoint and resolves credentials for each request.
func NewClient(base string, key func() (string, error)) (*Client, error) {
	base, err := httpurl.CanonicalHTTPBaseURL(base)
	if err != nil {
		return nil, err
	}
	return &Client{base: base, key: key, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+endpoint, body)
	if err != nil {
		return nil, fault("unreachable")
	}
	if c.key == nil {
		return nil, fault("unauthorized")
	}
	key, err := c.key()
	if err != nil || key == "" {
		return nil, fault("unauthorized")
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fault("unreachable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, statusError(resp.StatusCode)
	}
	return resp, nil
}
func (c *Client) json(ctx context.Context, method, endpoint string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var r io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fault("invalid_receipt")
		}
		r = bytes.NewReader(b)
	}
	resp, err := c.request(ctx, method, endpoint, r, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return fault("unreachable")
	}
	if len(b) > 1<<20 || json.Unmarshal(b, out) != nil {
		return fault("invalid_receipt")
	}
	return nil
}

// Info returns the vault identity.
func (c *Client) Info(ctx context.Context) (string, error) {
	var v struct {
		VaultID string `json:"vault_id"`
	}
	if err := c.json(ctx, "GET", "/api/v1/info", nil, &v); err != nil {
		return "", err
	}
	if v.VaultID == "" {
		return "", fault("invalid_receipt")
	}
	return v.VaultID, nil
}

// Node is a live Docbank file or folder.
type Node struct {
	ID        int64  `json:"id"`
	ParentID  int64  `json:"parent_id"`
	Kind      string `json:"kind"`
	BlobHash  string `json:"blob_hash"`
	VersionID string `json:"current_version_id"`
	Revision  int64  `json:"revision"`
	TrashedAt string `json:"trashed_at"`
}

// Version identifies immutable content owned by one node.
type Version struct {
	ID       string `json:"id"`
	NodeID   int64  `json:"node_id"`
	BlobHash string `json:"blob_hash"`
	Size     int64  `json:"size"`
}

// Receipt binds an upload to its immutable content and computed digest.
type Receipt struct {
	Status       string `json:"status"`
	Node         Node   `json:"node"`
	ComputedHash string `json:"computed_hash"`
	ComputedSize int64  `json:"computed_size"`
}

// EnsureFolder creates missing ancestors and returns the final folder node.
func (c *Client) EnsureFolder(ctx context.Context, folder string) (int64, error) {
	var current strings.Builder
	var node Node
	for segment := range strings.SplitSeq(strings.Trim(folder, "/"), "/") {
		if segment == "" {
			continue
		}
		current.WriteString("/" + segment)
		endpoint := "/api/v1/path?" + url.Values{"path": {current.String()}}.Encode()
		node = Node{}
		err := c.json(ctx, "GET", endpoint, nil, &node)
		if ErrorKind(err) == "not_found" {
			err = c.json(ctx, "POST", "/api/v1/path/mkdir", map[string]string{"path": current.String()}, &node)
			if e, ok := errors.AsType[*Error](err); ok && e.Status == 409 {
				node = Node{}
				err = c.json(ctx, "GET", endpoint, nil, &node)
			}
		}
		if err != nil {
			return 0, err
		}
		if node.ID <= 0 || node.Kind != "dir" {
			return 0, fault("invalid_receipt")
		}
	}
	if current.Len() == 0 {
		if err := c.json(ctx, "GET", "/api/v1/path?path=%2F", nil, &node); err != nil {
			return 0, err
		}
	}
	if node.ID <= 0 || node.Kind != "dir" {
		return 0, fault("invalid_receipt")
	}
	return node.ID, nil
}
func streamTimeout(size int64) time.Duration {
	return 30*time.Second + time.Duration(min(max(size, 0), int64(2<<30))/(1<<20))*time.Second
}

// Upload streams a file and validates its receipt against the declared size and hash.
func (c *Client) Upload(ctx context.Context, parent int64, name, mediaType string, r io.Reader, size int64, sha string) (Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, streamTimeout(size))
	defer cancel()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	mw := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		headers := make(textproto.MIMEHeader)
		headers.Set("Content-Disposition", multipart.FileContentDisposition("file", name))
		headers.Set("Content-Type", mediaType)
		part, err := mw.CreatePart(headers)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if err == nil {
			err = mw.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	endpoint := "/api/v1/uploads?" + url.Values{"parent_id": {strconv.FormatInt(parent, 10)}, "name": {name}}.Encode()
	resp, err := c.request(ctx, "POST", endpoint, reader, http.Header{"Content-Type": {mw.FormDataContentType()}, "X-Docbank-Blob-Hash": {sha}, "X-Docbank-Blob-Size": {strconv.FormatInt(size, 10)}})
	if err != nil {
		_ = reader.CloseWithError(err)
		<-done
		return Receipt{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	_ = reader.Close()
	writeErr := <-done
	if err != nil {
		return Receipt{}, fault("unreachable")
	}
	if writeErr != nil {
		return Receipt{}, fault("unreachable")
	}
	var receipt Receipt
	if len(b) > 1<<20 || json.Unmarshal(b, &receipt) != nil || receipt.ComputedHash != sha || receipt.ComputedSize != size || receipt.Node.ID <= 0 || receipt.Node.ParentID != parent || receipt.Node.Kind != "file" || receipt.Node.BlobHash != sha || receipt.Node.VersionID == "" || (receipt.Status != "added" && receipt.Status != "skipped") {
		return Receipt{}, fault("invalid_receipt")
	}
	return receipt, nil
}

// Node reads the current node, including deletion state.
func (c *Client) Node(ctx context.Context, id int64) (Node, error) {
	var n Node
	err := c.json(ctx, "GET", "/api/v1/nodes/"+strconv.FormatInt(id, 10), nil, &n)
	return n, err
}

// Version reads one immutable content version.
func (c *Client) Version(ctx context.Context, id string) (Version, error) {
	var v Version
	err := c.json(ctx, "GET", "/api/v1/versions/"+url.PathEscape(id), nil, &v)
	return v, err
}
func (c *Client) verify(ctx context.Context, e metadata.Attachment) (Node, error) {
	vault, err := c.Info(ctx)
	if err != nil {
		return Node{}, err
	}
	if vault != e.Docbank.VaultUID {
		return Node{}, fault("foreign_vault")
	}
	n, err := c.Node(ctx, e.Docbank.NodeID)
	if err != nil {
		return n, err
	}
	if n.TrashedAt != "" {
		return n, fault("not_found")
	}
	if n.ID != e.Docbank.NodeID || n.Kind != "file" || n.Revision <= 0 {
		return n, fault("integrity")
	}
	v, err := c.Version(ctx, e.Docbank.ContentVersionID)
	if err != nil {
		return n, err
	}
	if v.ID != e.Docbank.ContentVersionID || v.NodeID != e.Docbank.NodeID || v.BlobHash != e.Docbank.ContentSHA256 || v.Size != e.Size {
		return n, fault("integrity")
	}
	return n, nil
}

type verifiedStream struct {
	resp    *http.Response
	h       hash.Hash
	size, n int64
	sha     string
	err     error
	ended   bool
	cancel  context.CancelFunc
}

func (s *verifiedStream) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.ended {
		return 0, io.EOF
	}
	n, err := s.resp.Body.Read(p)
	s.n += int64(n)
	_, _ = s.h.Write(p[:n])
	if s.n > s.size {
		s.err = fault("integrity")
		return n, s.err
	}
	if err != nil {
		if err != io.EOF {
			s.err = fault("unreachable")
		} else {
			s.ended = true
			digest := "sha-256=:" + base64.StdEncoding.EncodeToString(s.h.Sum(nil)) + ":"
			if s.n != s.size || hex.EncodeToString(s.h.Sum(nil)) != s.sha || s.resp.Trailer.Get("Content-Digest") != digest {
				s.err = fault("integrity")
			}
		}
		if s.err != nil {
			return n, s.err
		}
	}
	return n, err
}
func (s *verifiedStream) Close() error {
	err := s.resp.Body.Close()
	s.cancel()
	if s.err != nil {
		return s.err
	}
	if !s.ended {
		return fault("integrity")
	}
	return err
}

// Content verifies catalog identity before opening the pinned version and checks
// count, hash, and the EOF digest trailer while streaming. Close checks completion.
func (c *Client) Content(ctx context.Context, e metadata.Attachment) (io.ReadCloser, error) {
	if _, err := c.verify(ctx, e); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, streamTimeout(e.Size))
	resp, err := c.request(ctx, "GET", "/api/v1/versions/"+url.PathEscape(e.Docbank.ContentVersionID)+"/content", nil, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.Header.Get("X-Docbank-Content-Version") != e.Docbank.ContentVersionID || resp.Header.Get("X-Docbank-Blob-Hash") != e.Docbank.ContentSHA256 || resp.Header.Get("X-Docbank-Blob-Size") != strconv.FormatInt(e.Size, 10) {
		_ = resp.Body.Close()
		cancel()
		return nil, fault("integrity")
	}
	return &verifiedStream{resp: resp, h: sha256.New(), size: e.Size, sha: e.Docbank.ContentSHA256, cancel: cancel}, nil
}
