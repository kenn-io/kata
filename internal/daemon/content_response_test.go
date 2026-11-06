package daemon

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"hegel.dev/go/hegel"
)

// Once release is denied, restoring authority cannot resume that request,
// including when the producer ignores an earlier Write error and flushes.
func TestContentReleaseDenialStaysTerminalProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		afterFirstRelease := hegel.Draw(ht, hegel.Integers(0, 1)) == 1
		allowed := true
		denial := errors.New("synthetic content authorization denial")
		sink := httptest.NewRecorder()
		writer := &revalidatedContentResponse{underlying: sink, header: make(http.Header), validate: func() error {
			if !allowed {
				return denial
			}
			return nil
		}}
		var want []byte
		if afterFirstRelease {
			n, err := writer.Write(prefix)
			if err != nil || n != len(prefix) {
				ht.Fatalf("authorized prefix failed: %d %v", n, err)
			}
			want = prefix
		}
		allowed = false
		if n, err := writer.Write([]byte("denied")); n != 0 || !errors.Is(err, denial) {
			ht.Fatalf("denied write released bytes: %d %v", n, err)
		}
		allowed = true
		if n, err := writer.Write([]byte("restored")); n != 0 || !errors.Is(err, denial) {
			ht.Fatalf("denied request resumed: %d %v", n, err)
		}
		if err := writer.FlushError(); !errors.Is(err, denial) || !bytes.Equal(want, sink.Body.Bytes()) {
			ht.Fatalf("denied flush released content: %v", err)
		}
	}, hegel.WithTestCases(2048))
}
