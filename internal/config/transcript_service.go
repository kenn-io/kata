package config

import (
	"errors"

	"go.kenn.io/kata/internal/transcriptservice"
	"go.kenn.io/kata/internal/uid"
)

// TranscriptServiceConfig is operator-owned policy keyed by immutable project
// UID. Discovery advertises configured policy only and never resolves secrets.
type TranscriptServiceConfig struct {
	Project map[string]TranscriptServiceTarget `toml:"project"`
}

type TranscriptServiceTarget struct {
	Enabled     bool   `toml:"enabled"`
	ReceiverURL string `toml:"receiver_url"`
	LocatorURL  string `toml:"locator_url"`
}

func NormalizeTranscriptServiceConfig(input TranscriptServiceConfig) (TranscriptServiceConfig, error) {
	output := TranscriptServiceConfig{Project: make(map[string]TranscriptServiceTarget, len(input.Project))}
	for project, target := range input.Project {
		if !uid.Valid(project) {
			return TranscriptServiceConfig{}, errors.New("transcript_service project key must be an immutable project UID")
		}
		if target.Enabled {
			var err error
			target.ReceiverURL, err = transcriptservice.CanonicalBase(target.ReceiverURL)
			if err != nil {
				return TranscriptServiceConfig{}, err
			}
			target.LocatorURL, err = transcriptservice.CanonicalBase(target.LocatorURL)
			if err != nil {
				return TranscriptServiceConfig{}, err
			}
		}
		output.Project[project] = target
	}
	return output, nil
}
