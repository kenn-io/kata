package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdoptionBaselineFeatures(t *testing.T) {
	legacy := []CronEvent{{Type: "cron.job.snapshot", Payload: `{}`}}
	require.NoError(t, RequireAdoptionEventFeatures("", legacy))

	baseline := append(legacy, CronEvent{Type: "cron.run.snapshot", Payload: `{"summary":{"version":1,"sessions_incomplete":true}}`})
	for _, features := range []string{"", CronEventFeature, CronRunSessionsEventFeature} {
		require.ErrorIs(t, RequireAdoptionEventFeatures(features, baseline), ErrUnsupportedEventFeatures)
	}
	require.NoError(t, RequireAdoptionEventFeatures(SupportedEventFeatures, baseline))

	// Classification covers every emitted payload, including issue payloads.
	baseline = append(baseline, CronEvent{Type: "issue.moved", Payload: `{}`})
	require.ErrorIs(t, RequireAdoptionEventFeatures(CronEventFeature+","+CronRunSessionsEventFeature, baseline), ErrUnsupportedEventFeatures)
	require.NoError(t, RequireAdoptionEventFeatures(SupportedEventFeatures, baseline))
}
