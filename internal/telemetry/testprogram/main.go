// Command testprogram reports whether the Kata telemetry wrapper is enabled.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
)

func main() {
	version := flag.String("version", "v1.2.3", "build version")
	changes := flag.Bool("consent-changes", false, "exercise consent changes")
	flag.Parse()
	if *changes {
		consentChanges(*version)
		return
	}
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

func consentChanges(version string) {
	if err := posthog.RecordConsent(true); err != nil {
		panic(err)
	}
	reporter, err := telemetry.NewReporter(telemetry.Options{DistinctID: "anonymous-instance-id", Version: version})
	if err != nil {
		panic(err)
	}
	defer reporter.Close()
	fmt.Print(reporter.Enabled(), " ")
	if err := posthog.RecordConsent(false); err != nil {
		panic(err)
	}
	fmt.Print(reporter.Enabled(), " ")
	status, err := reporter.CaptureConfirmed(context.Background(), "setup_finished", nil, posthog.Delivery{})
	if err != nil {
		panic(err)
	}
	fmt.Print(status, " ")
	if err := os.Setenv("TELEMETRY_ENABLED", "1"); err != nil {
		panic(err)
	}
	fmt.Print(reporter.Enabled(), " ")
	if err := os.Unsetenv("TELEMETRY_ENABLED"); err != nil {
		panic(err)
	}
	startedOff, err := telemetry.NewReporter(telemetry.Options{DistinctID: "anonymous-instance-id", Version: version})
	if err != nil {
		panic(err)
	}
	defer startedOff.Close()
	if err := posthog.RecordConsent(true); err != nil {
		panic(err)
	}
	fmt.Println(startedOff.Enabled())
}
