package db

import "context"

// RelayMetadataFeatureReader inspects the content a relay may need to receive.
type RelayMetadataFeatureReader interface {
	RelayMetadataRequiresCrossLinks(ctx context.Context, projectID int64) (bool, error)
}
