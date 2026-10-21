// Package docbanktest implements the Docbank HTTP routes used by Kata tests.
// Shapes and integrity headers follow Docbank's internal/api routes, not Kata's client.
package docbanktest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// VaultUID is the fixed identity of an isolated fake vault.
const VaultUID = "6f1c2e8a-1111-4111-8111-111111111111"

// Fake implements the Docbank routes used by attachment clients.
type Fake struct {
	Server   *httptest.Server
	Fault    string
	mu       sync.Mutex
	requests []string
	dirs     map[string]int64
	nodes    map[int64]map[string]any
	versions map[string]map[string]any
	content  map[string][]byte
}

// New starts a fake server and registers its cleanup.
func New(t testing.TB) *Fake {
	f := &Fake{dirs: map[string]int64{"/": 1}, nodes: map[int64]map[string]any{}, versions: map[string]map[string]any{}, content: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// Requests returns the observed method and path pairs.
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}
func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("X-Api-Key") != "test-key" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if status, ok := strings.CutPrefix(f.Fault, "status:"); ok {
		n, _ := strconv.Atoi(status)
		http.Error(w, "upstream", n)
		return
	}
	send := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.MarshalWrite(w, v) }
	p := r.URL.Path
	switch {
	case p == "/api/v1/info":
		send(map[string]any{"vault_id": VaultUID})
	case p == "/api/v1/path":
		id, ok := f.dirs[r.URL.Query().Get("path")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		send(map[string]any{"id": id, "kind": "dir"})
	case p == "/api/v1/path/mkdir" && r.Method == "POST":
		var in struct {
			Path string `json:"path"`
		}
		_ = json.UnmarshalRead(r.Body, &in)
		if _, ok := f.dirs[in.Path]; ok {
			http.Error(w, "exists", http.StatusConflict)
			return
		}
		if _, ok := f.dirs[path.Dir(in.Path)]; !ok {
			http.NotFound(w, r)
			return
		}
		id := int64(len(f.dirs) + 1)
		f.dirs[in.Path] = id
		send(map[string]any{"id": id, "kind": "dir"})
	case p == "/api/v1/uploads" && r.Method == "POST":
		r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: bounded above with MaxBytesReader in this test server.
			http.Error(w, "multipart", 400)
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		files := r.MultipartForm.File["file"]
		if len(files) != 1 {
			http.Error(w, "file count", 400)
			return
		}
		file, err := files[0].Open()
		if err != nil {
			http.Error(w, "file", 400)
			return
		}
		defer func() { _ = file.Close() }()
		b, _ := io.ReadAll(file)
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		parent, _ := strconv.ParseInt(r.URL.Query().Get("parent_id"), 10, 64)
		if hash != r.Header.Get("X-Docbank-Blob-Hash") || strconv.Itoa(len(b)) != r.Header.Get("X-Docbank-Blob-Size") {
			http.Error(w, "identity", http.StatusUnprocessableEntity)
			return
		}
		name := r.URL.Query().Get("name")
		var node map[string]any
		status := "added"
		for _, n := range f.nodes {
			if n["name"] == name && n["parent_id"] == parent {
				node = n
				status = "skipped"
			}
		}
		if node == nil {
			id := int64(len(f.nodes) + 10)
			version := fmt.Sprintf("6f1c2e8a-2222-4222-8222-%012d", id)
			node = map[string]any{"id": id, "parent_id": parent, "kind": "file", "name": name, "blob_hash": hash, "size": len(b), "current_version_id": version, "revision": 3}
			f.nodes[id] = node
			f.versions[version] = map[string]any{"id": version, "node_id": id, "blob_hash": hash, "size": len(b)}
			f.content[version] = b
		}
		if f.Fault == "receipt" {
			hash = strings.Repeat("0", 64)
		}
		send(map[string]any{"status": status, "node": node, "computed_hash": hash, "computed_size": len(b)})
	case strings.HasPrefix(p, "/api/v1/nodes/"):
		id, _ := strconv.ParseInt(path.Base(p), 10, 64)
		n := f.nodes[id]
		if n == nil || f.Fault == "missing" {
			http.NotFound(w, r)
			return
		}
		send(n)
	case strings.HasPrefix(p, "/api/v1/versions/"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/versions/"), "/content")
		v := f.versions[id]
		if v == nil || f.Fault == "missing" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasSuffix(p, "/content") {
			send(v)
			return
		}
		w.Header().Set("X-Docbank-Content-Version", id)
		w.Header().Set("X-Docbank-Blob-Hash", v["blob_hash"].(string))
		w.Header().Set("X-Docbank-Blob-Size", fmt.Sprint(v["size"]))
		w.Header().Set("Trailer", "Content-Digest")
		w.Header().Set("Set-Cookie", "must-not-leak=1")
		b := append([]byte(nil), f.content[id]...)
		if f.Fault == "short" && len(b) > 0 {
			b = b[:len(b)-1]
		}
		if f.Fault == "bytes" && len(b) > 0 {
			b[0] ^= 1
		}
		_, _ = w.Write(b)
		sum := sha256.Sum256(b)
		digest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
		if f.Fault == "trailer" {
			digest = "wrong"
		}
		w.Header().Set("Content-Digest", digest)
	default:
		http.NotFound(w, r)
	}
}
