package agentsviewrelay

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

func TestReceiptCommitsToSnapshotAndOriginalClose(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	r := Receipt{Version: 1, ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", AuthorityUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", CloseEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", ServiceID: strings.Repeat("a", 64), SnapshotSHA256: strings.Repeat("b", 64), SnapshotBytes: 123, Agent: "pi", SessionID: "pi:selected", Locator: "https://archive.example/sessions/pi/selected", Status: "created", AcceptedAt: time.Now().UTC()}
	r, err := SignReceipt(r, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyReceipt(r, key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	r.SnapshotBytes++
	if err := VerifyReceipt(r, key.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("receipt did not bind byte count")
	}
}
