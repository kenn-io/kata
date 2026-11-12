package testfix

// ZeroReader supplies an unlimited stream of zero bytes.
type ZeroReader struct{}

func (ZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
