package cron

// HLC identifies the winning definition independently of any later baseline.
type HLC struct {
	Version           int    `json:"version"`
	PhysicalMS        int64  `json:"physical_ms"`
	Counter           int64  `json:"counter"`
	OriginInstanceUID string `json:"origin_instance_uid"`
}

// Validate checks the portable clock version, counters and origin identity.
func (h HLC) Validate() error {
	if h.Version != 1 || h.PhysicalMS < 0 || h.Counter < 0 || !validUID(h.OriginInstanceUID) {
		return invalid("invalid definition HLC")
	}
	return nil
}

// Summary is bounded shared evidence, excluding local paths and raw logs.
type Summary struct {
	Version      int    `json:"version"`
	Message      string `json:"message,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitzero"`
	OutputTokens int64  `json:"output_tokens,omitzero"`
}

// ParseSummary decodes and validates one bounded observation summary.
func ParseSummary(input []byte) (Summary, error) {
	var value Summary
	if err := decode(input, &value, SummaryLimit); err != nil {
		return value, err
	}
	if value.Version != 1 || value.InputTokens < 0 || value.OutputTokens < 0 {
		return value, invalid("invalid run summary")
	}
	return value, nil
}
