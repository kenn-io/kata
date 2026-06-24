package githubsync

import (
	"encoding/json"
	"fmt"
)

// DecodeSlurpArray flattens gh api --paginate --slurp list output.
func DecodeSlurpArray[T any](data []byte) ([]T, error) {
	var pages [][]T
	if err := json.Unmarshal(data, &pages); err != nil {
		return nil, fmt.Errorf("decode GitHub slurp array: %w", err)
	}
	total := 0
	for _, page := range pages {
		total += len(page)
	}
	out := make([]T, 0, total)
	for _, page := range pages {
		out = append(out, page...)
	}
	return out, nil
}
