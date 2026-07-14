package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
	kitvec "go.kenn.io/kit/vector"
)

// Config configures an embedding Client. BaseURL and Model are required by the
// caller (daemon config validation enforces this). Dims defaults to 768.
type Config struct {
	BaseURL             string
	Model               string
	APIKey              string
	Salt                string
	Dims                int
	BatchSize           int
	Timeout             time.Duration
	TrustPrivateNetwork bool
}

// Client calls an OpenAI-compatible /embeddings endpoint.
type Client struct {
	http      *http.Client
	baseURL   string
	model     string
	salt      string
	dims      int
	batchSize int
}

const (
	defaultDims      = 768
	defaultBatchSize = 64
	defaultTimeout   = 30 * time.Second
)

// New builds a Client with an origin-pinned HTTP transport. Embedding request
// bodies carry issue text, so target safety and redirect origin pinning apply
// even when the endpoint does not use an API key.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("embedding: base_url and model are required")
	}
	dims := cfg.Dims
	if dims <= 0 {
		dims = defaultDims
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = defaultBatchSize
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	origin, err := config.BearerOriginForBaseURLWithTrust(cfg.BaseURL, cfg.TrustPrivateNetwork)
	if err != nil {
		return nil, fmt.Errorf("embedding: configure client: %w", err)
	}
	hc := &http.Client{
		Timeout: timeout,
		Transport: &embeddingTransport{
			origin:              origin,
			apiKey:              cfg.APIKey,
			trustPrivateNetwork: cfg.TrustPrivateNetwork,
		},
	}
	return &Client{
		http:      hc,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		model:     cfg.Model,
		salt:      cfg.Salt,
		dims:      dims,
		batchSize: batch,
	}, nil
}

type embeddingTransport struct {
	base                http.RoundTripper
	origin              string
	apiKey              string
	trustPrivateNetwork bool
}

func (t *embeddingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if err := config.CheckBearerTargetSafeURLWithTrust(req.URL, t.trustPrivateNetwork); err != nil {
		return nil, err
	}
	if reqOrigin := req.URL.Scheme + "://" + req.URL.Host; reqOrigin != t.origin {
		return nil, fmt.Errorf("refusing embedding request to origin %q - client is bound to embedding origin %q", reqOrigin, t.origin)
	}
	if t.apiKey == "" || req.Header.Get("Authorization") != "" {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return base.RoundTrip(clone)
}

// Dims returns the configured/expected vector dimensionality.
func (c *Client) Dims() int { return c.dims }

// BatchSize is the maximum number of inputs per request.
func (c *Client) BatchSize() int { return c.batchSize }

// Generation identifies the vector space this client produces: model, dims,
// recipe version, and the operator salt ("same model name, different
// weights"). The endpoint URL is deliberately excluded so moving a host or
// port never forces a re-embed.
func (c *Client) Generation() kitvec.Generation {
	params := map[string]string{"recipe": strconv.Itoa(RecipeVersion)}
	if c.salt != "" {
		params["salt"] = c.salt
	}
	return kitvec.Generation{Model: c.model, Dimensions: c.dims, Params: params}
}

// EncodeFunc adapts the client to kit's encoder contract. kit invokes
// encoders on its own worker goroutines, where a caller's recover cannot
// reach, so the adapter converts panics to errors.
func (c *Client) EncodeFunc() kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) (vecs [][]float32, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("embedding: encoder panic: %v", r)
			}
		}()
		return c.Embed(ctx, texts)
	}
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding embeddingVector `json:"embedding"`
	} `json:"data"`
}

// embeddingVector rejects null components at decode time. encoding/json
// leaves a float32 untouched for a JSON null, so a plain []float32 would
// silently turn a null component into 0 and the corrupted vector would be
// persisted as complete.
type embeddingVector []float32

func (v *embeddingVector) UnmarshalJSON(b []byte) error {
	var elements []*float32
	if err := json.Unmarshal(b, &elements); err != nil {
		return err
	}
	out := make([]float32, len(elements))
	for i, e := range elements {
		if e == nil {
			return fmt.Errorf("component %d is null", i)
		}
		out[i] = *e
	}
	*v = out
	return nil
}

// APIError is a non-2xx response from the embedding endpoint.
type APIError struct {
	StatusCode int
	RetryAfter time.Duration
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("embedding endpoint returned %d: %s", e.StatusCode, e.Body)
}

// Definitive reports whether retrying is pointless without operator action.
func (e *APIError) Definitive() bool {
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// Embed returns one L2-normalized vector per input, preserving order. Inputs
// are sent in batches of at most BatchSize.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.batchSize {
		end := start + c.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Cap the response read, but scale it to the expected payload: one vector
	// per input at `dims` float32s, JSON-encoded (~16 bytes/float) plus 1 MiB
	// of structural overhead. A fixed 1 MiB cap silently truncated common
	// configs (e.g. 3072-dim models at batch 64 ≈ 3 MiB), surfacing as a
	// misleading decode error.
	maxBytes := int64(c.dims)*int64(len(texts))*16 + (1 << 20)
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       string(rb),
		}
	}
	var er embedResponse
	if err := json.Unmarshal(rb, &er); err != nil {
		return nil, fmt.Errorf("embedding: decode response: %w", err)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embedding: got %d vectors for %d inputs", len(er.Data), len(texts))
	}
	vecs := make([][]float32, len(er.Data))
	for i, d := range er.Data {
		if len(d.Embedding) != c.dims {
			return nil, fmt.Errorf("embedding: vector dims %d != configured %d", len(d.Embedding), c.dims)
		}
		normalized, err := normalize(d.Embedding)
		if err != nil {
			return nil, fmt.Errorf("embedding: vector %d: %w", i, err)
		}
		vecs[i] = normalized
	}
	return vecs, nil
}

func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	// RFC 7231 also allows an HTTP-date form. Convert it to a delay; clamp a
	// past date to 0 so callers never treat it as a negative backoff.
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// normalize L2-normalizes v. A zero-norm vector is an error: it cannot
// participate in cosine distance, and persisting it would poison search
// rankings with no signal that a re-embed is needed.
func normalize(v []float32) ([]float32, error) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil, fmt.Errorf("zero norm")
	}
	inv := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * inv
	}
	return out, nil
}
