// Command testprogram reports whether the Kata telemetry wrapper is enabled.
package main

import (
	"flag"
	"fmt"

	"go.kenn.io/kata/internal/telemetry"
)

func main() {
	version := flag.String("version", "v1.2.3", "build version")
	flag.Parse()
	reporter, err := telemetry.NewReporter(telemetry.Options{DistinctID: "anonymous-instance-id", Version: *version})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := reporter.Close(); err != nil {
			panic(err)
		}
	}()

	switch {
	case reporter.Enabled():
		fmt.Println("enabled")
	case !reporter.EventAllowed("app_opened"):
		// The capture route answers disabled only while the allowlist survives opt-out.
		fmt.Println("disabled without allowlist")
	default:
		fmt.Println("disabled")
	}
}
