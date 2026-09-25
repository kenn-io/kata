package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveTUIActorUsesUserEnvBeforeAnonymous(t *testing.T) { //nolint:paralleltest // sets KATA_AUTHOR and USER
	t.Setenv("KATA_AUTHOR", "")
	t.Setenv("USER", "operator")

	assert.Equal(t, "operator", resolveTUIActor())
}

func TestResolveTUIActorPrefersKataAuthor(t *testing.T) { //nolint:paralleltest // sets KATA_AUTHOR and USER
	t.Setenv("KATA_AUTHOR", "configured-agent")
	t.Setenv("USER", "operator")

	assert.Equal(t, "configured-agent", resolveTUIActor())
}
