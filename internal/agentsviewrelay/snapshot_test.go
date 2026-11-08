package agentsviewrelay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Contract from the published v1 envelope: manifest.native_session_id is the
// canonical ID, while the association separately retains provider-native ID.
func TestSnapshotIdentityBeforeForwarding(t *testing.T) {
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`)
	sum := sha256.Sum256(body)
	c := Control{Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotBytes: int64(len(body)), SnapshotSHA256: hex.EncodeToString(sum[:])}
	if err := VerifySnapshot(t.Context(), bytes.NewReader(body), c); err != nil {
		t.Fatal(err)
	}
	c.SessionID = "pi:other"
	if err := VerifySnapshot(t.Context(), bytes.NewReader(body), c); err == nil {
		t.Fatal("accepted session B under session A controls")
	}
	for _, raw := range []string{`{"v":1,"v":1}`, string(body) + `{}`, `{"v":1,"session_id":"pi:selected","manifest":{"v":1,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"other"}}}`} {
		b := []byte(raw)
		h := sha256.Sum256(b)
		c.SessionID = "pi:selected"
		c.SnapshotBytes = int64(len(b))
		c.SnapshotSHA256 = hex.EncodeToString(h[:])
		if err := VerifySnapshot(t.Context(), bytes.NewReader(b), c); err == nil {
			t.Fatal("accepted invalid envelope identity")
		}
	}
}
