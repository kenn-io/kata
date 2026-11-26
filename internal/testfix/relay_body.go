package testfix

import "io"

// ReadCountingBody records reads; without a reader it supplies zero bytes.
type ReadCountingBody struct {
	Reader io.Reader
	Reads  int
}

func (b *ReadCountingBody) Read(p []byte) (int, error) {
	b.Reads++
	if b.Reader == nil {
		clear(p)
		return len(p), nil
	}
	return b.Reader.Read(p)
}

// Close satisfies io.ReadCloser for HTTP request bodies.
func (*ReadCountingBody) Close() error { return nil }
