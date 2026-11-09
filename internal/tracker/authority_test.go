package tracker

import "testing"

func TestProviderAuthorityHasOnlyCanonicalCredentialFreeOrigins(t *testing.T) {
	for _, a := range []ProviderAuthority{{APIOrigin: "https://api.example", WebOrigin: "https://app.example"}, {APIOrigin: "http://127.0.0.1:1234"}} {
		if a.Validate() != nil {
			t.Fatal("safe authority rejected")
		}
	}
	for _, raw := range []string{"https://user:fixture@api.example", "https://api.example/path", "https://api.example?fixture=value", "http://api.example", "https://api.example/", "https://api.example:443"} {
		if (ProviderAuthority{APIOrigin: raw}).Validate() == nil {
			t.Fatal("noncanonical/private origin details accepted")
		}
	}
}
