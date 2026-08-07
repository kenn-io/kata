package main

import (
	"fmt"

	"go.kenn.io/kata/internal/telemetry"
)

func main() {
	reporter, err := telemetry.NewReporter(telemetry.Options{DistinctID: "anonymous-instance-id"})
	if err != nil {
		panic(err)
	}
	defer reporter.Close()

	if reporter.Enabled() {
		fmt.Println("enabled")
		return
	}
	fmt.Println("disabled")
}
