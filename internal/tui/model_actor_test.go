package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveTUIActorUsesUserEnvBeforeAnonymous(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "")
	t.Setenv("USER", "operator")

	assert.Equal(t, "operator", resolveTUIActor())
}

func TestResolveTUIActorPrefersKataAuthor(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "configured-agent")
	t.Setenv("USER", "operator")

	assert.Equal(t, "configured-agent", resolveTUIActor())
}
