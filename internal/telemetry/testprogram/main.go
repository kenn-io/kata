// Command testprogram reports whether the Kata telemetry wrapper is enabled.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"go.kenn.io/kata/internal/telemetry"
)

func main() {
	reporter, err := telemetry.NewReporter(telemetry.Options{DistinctID: "anonymous-instance-id"})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := reporter.Close(); err != nil {
			panic(err)
		}
	}()

	if reporter.Enabled() {
		fmt.Println("enabled")
		return
	}
	fmt.Println("disabled")

	// Only a disabled reporter reaches this capture, so the program never sends anything.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/ui/telemetry",
		strings.NewReader(`{"event":"app_opened","properties":{"surface":"tui"}}`))
	recorder := httptest.NewRecorder()
	telemetry.CaptureHandler(reporter).ServeHTTP(recorder, request)
	fmt.Println("capture", recorder.Code, strings.TrimSpace(recorder.Body.String()))
}
