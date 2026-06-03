//go:build kata_test

package telemetry

func init() {
	disablePostHogTelemetry()
}
