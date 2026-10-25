// Package docbank implements the folder, upload, and immutable-content API used by Kata.
package docbank

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"uuid"
)

// Error exposes only the upstream status, keeping credentials and response details private.
type Error struct{ Status int }

func (e *Error) Error() string { return "docbank: HTTP " + strconv.Itoa(e.Status) }

// Client uses an endpoint normalized by config and resolves its key per request.
type Client struct {
	base string
	key  func() (string, error)
	http *http.Client
}

// NewClient uses the normalized endpoint and resolves credentials on each request.
func NewClient(base string, key func() (string, error)) *Client {
	return &Client{base: base, key: key, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+endpoint, body)
	if err != nil {
		return nil, &Error{}
	}
	key, err := c.key()
	if err != nil || key == "" {
		return nil, &Error{Status: 401}
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("X-Api-Key", key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, &Error{Status: resp.StatusCode}
	}
	return resp, nil
}
func (c *Client) json(ctx context.Context, method, endpoint string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return &Error{}
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.request(ctx, method, endpoint, body, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), out) != nil {
		return &Error{}
	}
	return nil
}

// EnsureFolder creates missing ancestors and returns the final folder ID.
func (c *Client) EnsureFolder(ctx context.Context, folder string) (int64, error) {
	var node struct {
		ID int64 `json:"id"`
	}
	var current strings.Builder
	for segment := range strings.SplitSeq(strings.Trim(folder, "/"), "/") {
		if segment != "" {
			current.WriteString("/" + segment)
		}
		endpoint := "/api/v1/path?" + url.Values{"path": {current.String()}}.Encode()
		err := c.json(ctx, "GET", endpoint, nil, &node)
		if e, ok := errors.AsType[*Error](err); ok && e.Status == 404 {
			err = c.json(ctx, "POST", "/api/v1/path/mkdir", map[string]string{"path": current.String()}, &node)
			if e, ok := errors.AsType[*Error](err); ok && e.Status == 409 {
				err = c.json(ctx, "GET", endpoint, nil, &node)
			}
		}
		if err != nil {
			return 0, err
		}
	}
	return node.ID, nil
}

// Upload sends Docbank the size and hash it validates and returns its immutable version ID.
func (c *Client) Upload(ctx context.Context, parent int64, name string, body io.Reader, size int64, sha string) (uuid.UUID, error) {
	var envelope bytes.Buffer
	mw := multipart.NewWriter(&envelope)
	if _, err := mw.CreateFormFile("file", name); err != nil {
		return uuid.Nil(), err
	}
	split := envelope.Len()
	if err := mw.Close(); err != nil {
		return uuid.Nil(), err
	}
	stream := io.MultiReader(bytes.NewReader(envelope.Bytes()[:split]), body, bytes.NewReader(envelope.Bytes()[split:]))
	endpoint := "/api/v1/uploads?" + url.Values{"parent_id": {strconv.FormatInt(parent, 10)}, "name": {name}}.Encode()
	resp, err := c.request(ctx, "POST", endpoint, stream, http.Header{"Content-Type": {mw.FormDataContentType()}, "X-Docbank-Blob-Hash": {sha}, "X-Docbank-Blob-Size": {strconv.FormatInt(size, 10)}})
	if err != nil {
		return uuid.Nil(), err
	}
	defer func() { _ = resp.Body.Close() }()
	var receipt struct {
		Node struct {
			VersionID uuid.UUID `json:"current_version_id"`
		} `json:"node"`
	}
	if json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &receipt) != nil || receipt.Node.VersionID == uuid.Nil() {
		return uuid.Nil(), &Error{}
	}
	return receipt.Node.VersionID, nil
}

// Content verifies immutable bytes against Docbank's blob hash and retains its reported length.
func (c *Client) Content(ctx context.Context, version uuid.UUID) (*http.Response, error) {
	return verifiedContent(c.request(ctx, "GET", "/api/v1/versions/"+version.String()+"/content", nil, nil))
}

func verifiedContent(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	resp.Body = &contentStream{ReadCloser: resp.Body, reader: bufio.NewReader(io.TeeReader(resp.Body, h)), hash: h, expected: resp.Header.Get("X-Docbank-Blob-Hash")}
	if size, err := strconv.ParseInt(resp.Header.Get("X-Docbank-Blob-Size"), 10, 64); err == nil {
		resp.ContentLength = size
	}
	return resp, nil
}

type contentStream struct {
	io.ReadCloser
	reader   *bufio.Reader
	hash     hash.Hash
	expected string
}

func (s *contentStream) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if err == nil {
		_, err = s.reader.Peek(1)
	}
	if err != nil && err != io.EOF {
		return 0, err
	}
	// Withhold the last chunk until the complete stream matches Docbank's blob hash.
	if err == io.EOF && s.expected != hex.EncodeToString(s.hash.Sum(nil)) {
		return 0, errors.New("docbank: content digest mismatch")
	}
	return n, err
}
