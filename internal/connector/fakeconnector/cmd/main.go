// Command fakeconnector is the executable wrapper for the shared black-box fixture.
package main

import (
	"flag"
	"os"
	"strings"
	// Embed zones as the kata binary does: a trimmed build cannot fall back to
	// the Go installation's copy, and Windows has no system zone database.
	_ "time/tzdata"

	"go.kenn.io/kata/internal/connector/fakeconnector"
)

func main() {
	statePath := flag.String("state", "", "test-owned JSON state")
	flag.Parse()
	if strings.TrimSpace(*statePath) == "" {
		os.Exit(1)
	}
	os.Exit(fakeconnector.Run(*statePath, os.Stdin, os.Stdout))
}
