package agentsviewrelay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"strings"
	"testing"
)

// Contract from the published v1 envelope: manifest.native_session_id is the
// canonical ID, while the association separately retains provider-native ID.
func TestSnapshotIdentityBeforeForwarding(t *testing.T) {
	body := []byte(`{"v":1,"session_id":"pi:selected","manifest":{"v":5,"native_session_id":"pi:selected","session":{"id":"pi:selected","agent":"pi"}},"segments":[]}`)
	sum := sha256.Sum256(body)
	c := Control{Agent: "pi", NativeSessionID: "selected", SessionID: "pi:selected", SnapshotBytes: int64(len(body)), SnapshotSHA256: hex.EncodeToString(sum[:])}
	if err := VerifySnapshot(t.Context(), bytes.NewReader(body), c); err != nil {
		t.Fatal(err)
	}
	c.SessionID = "pi:other"
	if err := VerifySnapshot(t.Context(), bytes.NewReader(body), c); err == nil {
		t.Fatal("accepted session B under session A controls")
	}
	for _, raw := range []string{strings.Replace(string(body), `"v":1,`, `"v":1,"v":1,`, 1), string(body) + `{}`, strings.Replace(string(body), `"agent":"pi"`, `"agent":"other"`, 1)} {
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

func TestControlAcceptsAuthenticatedForwardedHolderKinds(t *testing.T) {
	a := validAssociation()
	c := Control{Version: 1, IssueUID: a.IssueUID, ProjectUID: a.ProjectUID, AuthorityUID: a.AuthorityUID, CloseEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", ServiceID: a.ServiceID, Agent: a.Agent, NativeSessionID: a.NativeSessionID, SessionID: a.SessionID, SnapshotSHA256: a.SnapshotSHA256, SnapshotBytes: a.SnapshotBytes, Route: a.Route}
	for _, kind := range []string{"host:v1", "spoke-host:v1:" + strings.Repeat("A", 43), "relay:v1:" + strings.Repeat("A", 43)} {
		c.ClientKind = kind
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeControl(raw); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}
