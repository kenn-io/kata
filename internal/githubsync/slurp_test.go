package githubsync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeSlurpArray(t *testing.T) {
	issuesJSON := []byte(`[
		[
			{"node_id":"I_first","number":1,"title":"first"},
			{"node_id":"I_second","number":2,"title":"second"}
		],
		[
			{"node_id":"I_third","number":3,"title":"third"}
		]
	]`)

	issues, err := DecodeSlurpArray[Issue](issuesJSON)
	require.NoError(t, err)
	require.Len(t, issues, 3)
	assert.Equal(t, "I_first", issues[0].NodeID)
	assert.Equal(t, "I_second", issues[1].NodeID)
	assert.Equal(t, "I_third", issues[2].NodeID)

	commentsJSON := []byte(`[
		[
			{"node_id":"C_first","body":"first comment"}
		],
		[
			{"node_id":"C_second","body":"second comment"},
			{"node_id":"C_third","body":"third comment"}
		]
	]`)

	comments, err := DecodeSlurpArray[Comment](commentsJSON)
	require.NoError(t, err)
	require.Len(t, comments, 3)
	assert.Equal(t, "C_first", comments[0].NodeID)
	assert.Equal(t, "C_second", comments[1].NodeID)
	assert.Equal(t, "C_third", comments[2].NodeID)
}
