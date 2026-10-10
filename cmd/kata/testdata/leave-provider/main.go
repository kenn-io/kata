package main

import (
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/pkg/federationprovider"
	"os"
)

// The external fixture accepts only the saved request after the daemon has
// durably marked its connection as leaving.
func main() {
	if os.Getenv("KATA_TEST_LEAVE_PROVIDER") != "1" {
		return
	}
	r, err := federationprovider.DecodeRequest(os.Stdin)
	if err != nil || r.Operation != "release" {
		os.Exit(2)
	}
	entries, err := config.ReadFederationCredentials()
	if err != nil {
		os.Exit(2)
	}
	for _, c := range entries.Projects {
		if c.Provider != nil && c.Provider.RequestID == r.RequestID && c.LeavePending {
			err := federationprovider.WriteResponse(os.Stdout, r, federationprovider.Response{
				Version: 1, Operation: r.Operation, RequestID: r.RequestID,
				Status: federationprovider.Status(os.Getenv("KATA_TEST_LEAVE_DECISION")),
			})
			if err == nil {
				os.Exit(0)
			}
		}
	}
	os.Exit(2)
}
