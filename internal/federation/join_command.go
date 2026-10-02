package federation

import (
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/federation/joincommand"
)

// JoinCommand returns a runnable shell command for the supplied project grant,
// or an empty string if the grant or target cannot support a join.
func JoinCommand(in api.FederationJoinInstructions) string { return joincommand.Build(in) }
