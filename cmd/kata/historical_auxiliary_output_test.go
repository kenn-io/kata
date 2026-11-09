package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestHistoricalAuxiliaryOutputRefs(t *testing.T) {
	for _, mode := range []string{"human", "agent"} {
		for _, operation := range []string{"meta set", "meta unset", "meta get whole", "meta get key", "notify", "delete", "purge", "status", "scoped token"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				if mode == "human" && strings.HasPrefix(operation, "meta get") {
					t.Skip("human metadata reads contain values without issue references")
				}
				var options []testenv.Option
				if operation == "scoped token" {
					options = []testenv.Option{testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity()}
				}
				env, dir, target, moved, oldRef := historicalOutputFixture(t, options...)
				qualified := target.Name + "#" + moved.ShortID
				var args []string
				switch operation {
				case "meta set":
					args = []string{"meta", "set", oldRef, "example.key", "value"}
				case "meta unset", "meta get whole", "meta get key":
					runCLI(t, env, dir, "meta", "set", oldRef, "example.key", "value")
					args = []string{"meta", "get", oldRef}
					switch operation {
					case "meta unset":
						args = []string{"meta", "unset", oldRef, "example.key"}
					case "meta get key":
						args = append(args, "example.key")
					}
				case "notify":
					args = []string{"notify", oldRef, "--to", "reviewer", "--message", "Review this work"}
				case "delete", "purge":
					args = []string{operation, oldRef, "--force", "--confirm", strings.ToUpper(operation) + " " + qualified}
				case "status":
					args = []string{"status", oldRef}
				case "scoped token":
					args = []string{"tokens", "create", "--issue", oldRef, "--actor", "worker", "--expires-in", "1h", "--token-file", privateTokenFilePath(t)}
				}
				output := runCLI(t, env, dir, append(args, "--format", mode)...)
				fields := strings.Fields(strings.SplitN(output, "\n", 2)[0])
				var ref string
				switch {
				case strings.HasPrefix(operation, "meta"):
					index := 2
					if mode == "agent" {
						index = 3
					}
					ref = fields[index]
				case operation == "status" && mode == "agent", operation == "scoped token":
					for _, field := range fields {
						if value, found := strings.CutPrefix(field, "issue="); found {
							ref = value
						}
					}
				case mode == "agent":
					ref = fields[2]
				case operation == "notify":
					ref = fields[1]
				default:
					ref = fields[0]
				}
				expected := qualified
				if strings.HasPrefix(operation, "meta get") {
					expected = oldRef // Metadata-only responses retain the known-valid request reference.
				}
				require.Equal(t, expected, ref, output)
				if operation == "purge" {
					return // The receipt retains the removed issue's project identity.
				}
				if operation == "delete" {
					runCLI(t, env, dir, "restore", ref)
				}
				require.Contains(t, runCLI(t, env, dir, "show", ref, "--json"), moved.UID)
			})
		}
	}
}

func TestHistoricalMetadataRetainsQualifiedRequestedRef(t *testing.T) {
	env, dir, _, _, oldRef := historicalOutputFixture(t)
	runCLI(t, env, dir, "meta", "set", oldRef, "example.key", "value")
	qualifiedOldRef := "source-project#" + oldRef
	output := runCLI(t, env, dir, "--project", "target-project", "meta", "get", qualifiedOldRef, "--agent")
	require.Contains(t, output, fmt.Sprintf("OK meta get %s ", qualifiedOldRef))
	require.Contains(t, output, "key=example.key value=")
}
