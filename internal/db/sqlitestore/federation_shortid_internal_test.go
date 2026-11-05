package sqlitestore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveFederatedIssueShortIDValidatesMatchingStoredID(t *testing.T) {
	for _, tt := range []struct {
		name      string
		preferred string
		stored    string
		wantErr   string
	}{
		{name: "invalid syntax", preferred: "!!!!", stored: "d4ec", wantErr: "invalid federated short_id"},
		{name: "wrong suffix", preferred: "abcd", stored: "d4ec", wantErr: "does not match uid"},
		{name: "valid suffix", preferred: "d4ec", stored: "d4ec"},
		{name: "extended suffix", preferred: "bcd4ec", stored: "bcd4ec"},
		{name: "omitted preference", stored: "d4ec"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Reject invalid preferences and reuse stored IDs without querying the database.
			got, err := resolveFederatedIssueShortID(t.Context(), nil, 1, "01HZNQ7VFPK1XGD8R5MABCD4EC", tt.preferred, &federatedIssueRow{shortID: tt.stored})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.stored, got)
		})
	}
}
